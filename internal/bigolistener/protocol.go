package bigolistener

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rc4"
	"crypto/rsa"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uriEncryptExchangeKey    = 5636
	uriEncryptExchangeKeyRes = 5892
	uriPrepareLogin          = 512279
	uriPrepareLoginRes       = 512535
	uriLogin                 = 512791
	uriLoginRes              = 513047
	uriPing                  = 791
	uriAddFollow             = 845341
	uriAddFollowRes          = 845597
	uriSetLiveNotice         = 1007901
	uriSetLiveNoticeRes      = 1008157
	uriGetAuthToken          = 527389
	uriGetAuthTokenRes       = 527645
	uriLiveStartNotify       = 524823
	uriLiveStartNotifyAck    = 525079
	uriNewLiveNotify         = 525335

	protocolStatusOK = 200
	protocolAppID    = 60
	// AddFollow returns 8 when the requested relationship already exists. The
	// desired postcondition is already satisfied, so notification setup must
	// continue instead of treating a restart as a failed subscription.
	protocolFollowAlreadyActive = 8
	maxFrameSize                = 4 << 20
	linkdIOTimeout              = 15 * time.Second
	linkdPingEvery              = 20 * time.Second
	linkdIdleTimeout            = 3 * linkdPingEvery
	legacyRSAKeyBits            = 512
	legacyRSAExponent           = 65537
	// BIGO uses 0 for an active foreground client and 7 for a background one.
	// Active delivery is required for this persistent listener.
	linkdForegroundStatus int16 = 0
)

var errProtocolRejected = errors.New("BIGO rejected the native Linkd request")

type linkdFrame struct {
	URI    int32
	Status int16
	Body   []byte
}

type liveEvent struct {
	URI      int32
	AppID    int32
	Sequence int32
	RouteID  int32
	Content  string
	Fields   map[string]string
}

type linkdClient struct {
	conn          net.Conn
	sendCipher    *rc4.Cipher
	recvCipher    *rc4.Cipher
	recvPlain     []byte
	pendingEvents []liveEvent
	writeOnce     sync.Once
	writeGate     chan struct{}
	seq           uint32
	lastReceived  time.Time
	nextPing      time.Time
	session       Session

	queryMu sync.Mutex
	queries map[int32][]*pendingQuery
	closed  bool
	done    chan struct{}
}

type queryResult struct {
	frame linkdFrame
	err   error
}

type pendingQuery struct {
	uri    int32
	match  func([]byte) bool
	result chan queryResult
}

type wireWriter struct{ bytes.Buffer }

func (w *wireWriter) i8(value int8)       { _ = w.WriteByte(byte(value)) }
func (w *wireWriter) u16(value uint16)    { _ = binary.Write(&w.Buffer, binary.LittleEndian, value) }
func (w *wireWriter) i16(value int16)     { _ = binary.Write(&w.Buffer, binary.LittleEndian, value) }
func (w *wireWriter) u32(value uint32)    { _ = binary.Write(&w.Buffer, binary.LittleEndian, value) }
func (w *wireWriter) i32(value int32)     { _ = binary.Write(&w.Buffer, binary.LittleEndian, value) }
func (w *wireWriter) i64(value int64)     { _ = binary.Write(&w.Buffer, binary.LittleEndian, value) }
func (w *wireWriter) string(value string) { w.blob([]byte(value)) }
func (w *wireWriter) blob(value []byte) {
	w.u16(uint16(len(value)))
	_, _ = w.Write(value)
}
func (w *wireWriter) emptyStringMap() { w.u32(0) }
func (w *wireWriter) integerList(values []uint32) {
	w.u32(uint32(len(values)))
	for _, value := range values {
		w.u32(value)
	}
}

type wireReader struct {
	data []byte
	off  int
}

func (r *wireReader) remaining() int { return len(r.data) - r.off }
func (r *wireReader) take(size int) ([]byte, error) {
	if size < 0 || r.remaining() < size {
		return nil, io.ErrUnexpectedEOF
	}
	value := r.data[r.off : r.off+size]
	r.off += size
	return value, nil
}
func (r *wireReader) u16() (uint16, error) {
	value, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(value), nil
}
func (r *wireReader) i32() (int32, error) {
	value, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(value)), nil
}
func (r *wireReader) blob() ([]byte, error) {
	size, err := r.u16()
	if err != nil {
		return nil, err
	}
	value, err := r.take(int(size))
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), value...), nil
}
func (r *wireReader) string() (string, error) {
	value, err := r.blob()
	return string(value), err
}
func (r *wireReader) stringMap() (map[string]string, error) {
	count, err := r.i32()
	if err != nil {
		return nil, err
	}
	if count < 0 || count > 4096 {
		return nil, fmt.Errorf("invalid BIGO map size %d", count)
	}
	result := make(map[string]string, count)
	for i := int32(0); i < count; i++ {
		key, err := r.string()
		if err != nil {
			return nil, err
		}
		value, err := r.string()
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

func marshalFrame(uri int32, body []byte) []byte {
	frame := make([]byte, 10+len(body))
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(frame)))
	binary.LittleEndian.PutUint32(frame[4:8], uint32(uri))
	binary.LittleEndian.PutUint16(frame[8:10], protocolStatusOK)
	copy(frame[10:], body)
	return frame
}

func parseFrame(frame []byte) (linkdFrame, error) {
	if len(frame) < 10 {
		return linkdFrame{}, errors.New("BIGO frame is shorter than its header")
	}
	length := int(binary.LittleEndian.Uint32(frame[:4]))
	if length != len(frame) || length > maxFrameSize {
		return linkdFrame{}, fmt.Errorf("invalid BIGO frame length %d (received %d)", length, len(frame))
	}
	return linkdFrame{
		URI:    int32(binary.LittleEndian.Uint32(frame[4:8])),
		Status: int16(binary.LittleEndian.Uint16(frame[8:10])),
		Body:   append([]byte(nil), frame[10:]...),
	}, nil
}

func generateLegacyRSAKey(random io.Reader) (*rsa.PrivateKey, error) {
	e := big.NewInt(legacyRSAExponent)
	for attempts := 0; attempts < 128; attempts++ {
		p, err := rand.Prime(random, legacyRSAKeyBits/2)
		if err != nil {
			return nil, err
		}
		q, err := rand.Prime(random, legacyRSAKeyBits/2)
		if err != nil {
			return nil, err
		}
		if p.Cmp(q) == 0 {
			continue
		}
		n := new(big.Int).Mul(p, q)
		if n.BitLen() != legacyRSAKeyBits {
			continue
		}
		phi := new(big.Int).Mul(new(big.Int).Sub(p, big.NewInt(1)), new(big.Int).Sub(q, big.NewInt(1)))
		d := new(big.Int).ModInverse(e, phi)
		if d == nil {
			continue
		}
		key := &rsa.PrivateKey{
			PublicKey: rsa.PublicKey{N: n, E: legacyRSAExponent},
			D:         d,
			Primes:    []*big.Int{p, q},
		}
		key.Precompute()
		if key.Validate() == nil {
			return key, nil
		}
	}
	return nil, errors.New("could not generate BIGO legacy RSA key")
}

func javaPositiveBigIntegerBytes(value *big.Int) []byte {
	result := value.Bytes()
	if len(result) > 0 && result[0]&0x80 != 0 {
		result = append([]byte{0}, result...)
	}
	return result
}

func legacyExchangeRequest(key *rsa.PrivateKey, random io.Reader) ([]byte, error) {
	if key == nil || key.N == nil {
		return nil, errors.New("BIGO exchange RSA key is nil")
	}
	seed := make([]byte, 4)
	if _, err := io.ReadFull(random, seed); err != nil {
		return nil, err
	}
	seed[3] = (seed[3] & 0x0f) | 0x40
	body := &wireWriter{}
	body.blob(javaPositiveBigIntegerBytes(key.N))
	body.blob(javaPositiveBigIntegerBytes(big.NewInt(int64(key.E))))
	paddingSize := []byte{0}
	if _, err := io.ReadFull(random, paddingSize); err != nil {
		return nil, err
	}
	padding := make([]byte, int(paddingSize[0]&3))
	if _, err := io.ReadFull(random, padding); err != nil {
		return nil, err
	}
	body.blob(padding)
	plain := marshalFrame(uriEncryptExchangeKey, body.Bytes())
	cipher, err := rc4.NewCipher(seed)
	if err != nil {
		return nil, err
	}
	encrypted := make([]byte, len(plain))
	cipher.XORKeyStream(encrypted, plain)
	return append(seed, encrypted...), nil
}

func readLegacyExchangeResponse(conn net.Conn, key *rsa.PrivateKey) ([]byte, error) {
	seed := make([]byte, 4)
	if _, err := io.ReadFull(conn, seed); err != nil {
		return nil, fmt.Errorf("read BIGO exchange response seed: %w", err)
	}
	cipher, err := rc4.NewCipher(seed)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, 0, 256)
	scratch := make([]byte, 256)
	want := 0
	for want == 0 || len(plain) < want {
		readSize := len(scratch)
		if want > 0 && want-len(plain) < readSize {
			readSize = want - len(plain)
		}
		n, err := conn.Read(scratch[:readSize])
		if err != nil {
			return nil, fmt.Errorf("read BIGO exchange response: %w", err)
		}
		decrypted := make([]byte, n)
		cipher.XORKeyStream(decrypted, scratch[:n])
		plain = append(plain, decrypted...)
		if want == 0 && len(plain) >= 4 {
			want = int(binary.LittleEndian.Uint32(plain[:4]))
			if want < 10 || want > maxFrameSize {
				return nil, fmt.Errorf("invalid BIGO exchange response length %d", want)
			}
		}
	}
	frame, err := parseFrame(plain)
	if err != nil {
		return nil, err
	}
	if frame.URI != uriEncryptExchangeKeyRes || frame.Status != protocolStatusOK {
		return nil, fmt.Errorf("unexpected BIGO exchange response uri=%d status=%d", frame.URI, frame.Status)
	}
	reader := &wireReader{data: frame.Body}
	encryptedKey, err := reader.blob()
	if err != nil || len(encryptedKey) == 0 {
		return nil, errors.New("BIGO exchange response omitted the RC4 session key")
	}
	sessionKey, err := decryptLegacyPKCS1v15(key, encryptedKey)
	if err != nil || len(sessionKey) == 0 {
		return nil, fmt.Errorf("decrypt BIGO exchange session key: %w", err)
	}
	return sessionKey, nil
}

// Go rejects RSA keys below 1024 bits in its high-level helpers. BIGO's
// legacy exchange uses an ephemeral 512-bit key, so perform the private
// operation and PKCS#1 v1.5 unpadding directly.
func decryptLegacyPKCS1v15(key *rsa.PrivateKey, ciphertext []byte) ([]byte, error) {
	if key == nil || key.N == nil || key.D == nil {
		return nil, errors.New("invalid BIGO RSA private key")
	}
	keyBytes := (key.N.BitLen() + 7) / 8
	if len(ciphertext) > keyBytes || keyBytes < 11 {
		return nil, errors.New("invalid BIGO RSA ciphertext length")
	}
	cipherInt := new(big.Int).SetBytes(ciphertext)
	if cipherInt.Cmp(key.N) >= 0 {
		return nil, errors.New("BIGO RSA ciphertext is out of range")
	}
	encoded := new(big.Int).Exp(cipherInt, key.D, key.N).FillBytes(make([]byte, keyBytes))
	if encoded[0] != 0 || encoded[1] != 2 {
		return nil, errors.New("invalid BIGO RSA PKCS#1 block type")
	}
	separator := -1
	for index := 2; index < len(encoded); index++ {
		if encoded[index] == 0 {
			separator = index
			break
		}
	}
	if separator < 10 || separator == len(encoded)-1 {
		return nil, errors.New("invalid BIGO RSA PKCS#1 padding")
	}
	return append([]byte(nil), encoded[separator+1:]...), nil
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		written, err := conn.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[written:]
	}
	return nil
}

func dialLinkd(ctx context.Context, session Session) (*linkdClient, string, error) {
	if err := session.Validate(); err != nil {
		return nil, "", err
	}
	var failures []string
	for _, rawAddress := range session.LinkdAddresses {
		address, err := parseLinkdAddress(rawAddress)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		client, err := dialOneLinkd(ctx, address, session)
		if err == nil {
			return client, address, nil
		}
		if errors.Is(err, errProtocolRejected) {
			return nil, "", err
		}
		failures = append(failures, address+": "+err.Error())
	}
	return nil, "", fmt.Errorf("connect BIGO Linkd: %s", strings.Join(failures, "; "))
}

func dialOneLinkd(ctx context.Context, address string, session Session) (*linkdClient, error) {
	dialer := &net.Dialer{Timeout: linkdIOTimeout, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	keep := false
	defer func() {
		stopContextClose()
		if !keep {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(linkdIOTimeout))
	key, err := generateLegacyRSAKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	exchange, err := legacyExchangeRequest(key, rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writeAll(conn, exchange); err != nil {
		return nil, fmt.Errorf("send BIGO exchange request: %w", err)
	}
	sessionKey, err := readLegacyExchangeResponse(conn, key)
	if err != nil {
		return nil, err
	}
	sendCipher, err := rc4.NewCipher(sessionKey)
	if err != nil {
		return nil, err
	}
	recvCipher, err := rc4.NewCipher(sessionKey)
	if err != nil {
		return nil, err
	}
	client := &linkdClient{
		conn:       conn,
		sendCipher: sendCipher,
		recvCipher: recvCipher,
		seq:        uint32(time.Now().UnixNano()),
		session:    session,
	}
	if err := client.login(ctx); err != nil {
		return nil, err
	}
	if !stopContextClose() {
		return nil, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	keep = true
	return client, nil
}

func (c *linkdClient) Close() error {
	if c == nil {
		return nil
	}
	c.queryMu.Lock()
	if c.closed {
		c.queryMu.Unlock()
		return nil
	}
	c.closed = true
	if c.done != nil {
		close(c.done)
	}
	for _, requests := range c.queries {
		for _, request := range requests {
			request.result <- queryResult{err: ErrNoConnection}
		}
	}
	clear(c.queries)
	c.queryMu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *linkdClient) nextSequence() uint32 {
	c.seq++
	if c.seq == 0 {
		c.seq++
	}
	return c.seq
}

func (c *linkdClient) sendFrame(uri int32, body []byte) error {
	return c.sendFrameContext(context.Background(), uri, body)
}

func (c *linkdClient) sendFrameContext(ctx context.Context, uri int32, body []byte) error {
	c.queryMu.Lock()
	if c.closed {
		c.queryMu.Unlock()
		return ErrNoConnection
	}
	if c.done == nil {
		c.done = make(chan struct{})
	}
	done := c.done
	c.queryMu.Unlock()
	c.writeOnce.Do(func() { c.writeGate = make(chan struct{}, 1) })
	select {
	case c.writeGate <- struct{}{}:
		defer func() { <-c.writeGate }()
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ErrNoConnection
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.queryMu.Lock()
	closed := c.closed
	c.queryMu.Unlock()
	if closed {
		return ErrNoConnection
	}
	plain := marshalFrame(uri, body)
	encrypted := make([]byte, len(plain))
	c.sendCipher.XORKeyStream(encrypted, plain)
	deadline := time.Now().Add(linkdIOTimeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = c.conn.SetWriteDeadline(deadline)
	cancelFinished := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = c.conn.SetWriteDeadline(time.Now())
		close(cancelFinished)
	})
	defer func() {
		if !stopCancel() {
			<-cancelFinished
		}
		_ = c.conn.SetWriteDeadline(time.Time{})
	}()
	if err := writeAll(c.conn, encrypted); err != nil {
		// Advancing the stream cipher without writing every encrypted byte
		// makes this connection unusable, even if cancellation caused the write.
		_ = c.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if limit, ok := ctx.Deadline(); ok && !time.Now().Before(limit) {
			return context.DeadlineExceeded
		}
		return fmt.Errorf("send BIGO Linkd uri=%d: %w", uri, err)
	}
	return nil
}

// query never reads from the connection. The hub remains its sole reader and
// routes matched replies before either setup or passive event handling sees them.
func (c *linkdClient) query(ctx context.Context, requestURI, responseURI int32, body []byte, match func([]byte) bool) (linkdFrame, error) {
	if err := ctx.Err(); err != nil {
		return linkdFrame{}, err
	}
	request := &pendingQuery{uri: responseURI, match: match, result: make(chan queryResult, 1)}
	c.queryMu.Lock()
	if c.closed {
		c.queryMu.Unlock()
		return linkdFrame{}, ErrNoConnection
	}
	for _, pending := range c.queries[responseURI] {
		if match == nil || pending.match == nil {
			c.queryMu.Unlock()
			return linkdFrame{}, errors.New("BIGO query response requires sequence correlation")
		}
	}
	count := 0
	for _, pending := range c.queries {
		count += len(pending)
	}
	if count >= 64 {
		c.queryMu.Unlock()
		return linkdFrame{}, errors.New("too many pending BIGO queries")
	}
	if c.queries == nil {
		c.queries = make(map[int32][]*pendingQuery)
	}
	c.queries[responseURI] = append(c.queries[responseURI], request)
	c.queryMu.Unlock()
	sent := false
	defer func() {
		// An uncorrelated canceled reply must be discarded before another
		// request for that URI can safely run on the same connection.
		if !sent || match != nil || ctx.Err() == nil {
			c.removeQuery(request)
		}
	}()
	if err := c.sendFrameContext(ctx, requestURI, body); err != nil {
		return linkdFrame{}, err
	}
	sent = true
	select {
	case result := <-request.result:
		return result.frame, result.err
	case <-ctx.Done():
		return linkdFrame{}, ctx.Err()
	}
}

func (c *linkdClient) removeQuery(request *pendingQuery) {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()
	requests := c.queries[request.uri]
	for i, pending := range requests {
		if pending == request {
			copy(requests[i:], requests[i+1:])
			requests[len(requests)-1] = nil
			requests = requests[:len(requests)-1]
			if len(requests) == 0 {
				delete(c.queries, request.uri)
			} else {
				c.queries[request.uri] = requests
			}
			return
		}
	}
}

func (c *linkdClient) routeQuery(frame linkdFrame) bool {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()
	requests := c.queries[frame.URI]
	for i, request := range requests {
		if request.match != nil && !request.match(frame.Body) {
			continue
		}
		copy(requests[i:], requests[i+1:])
		requests[len(requests)-1] = nil
		requests = requests[:len(requests)-1]
		if len(requests) == 0 {
			delete(c.queries, frame.URI)
		} else {
			c.queries[frame.URI] = requests
		}
		request.result <- queryResult{frame: frame}
		return true
	}
	return false
}

func (c *linkdClient) readFrame(ctx context.Context) (linkdFrame, error) {
	for {
		if len(c.recvPlain) >= 4 {
			length := int(binary.LittleEndian.Uint32(c.recvPlain[:4]))
			if length < 10 || length > maxFrameSize {
				return linkdFrame{}, fmt.Errorf("invalid BIGO Linkd frame length %d", length)
			}
			if len(c.recvPlain) >= length {
				raw := append([]byte(nil), c.recvPlain[:length]...)
				c.recvPlain = c.recvPlain[length:]
				frame, err := parseFrame(raw)
				if err == nil {
					c.lastReceived = time.Now()
					if c.routeQuery(frame) {
						continue
					}
				}
				return frame, err
			}
		}
		if err := ctx.Err(); err != nil {
			return linkdFrame{}, err
		}
		deadline := time.Now().Add(time.Second)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		_ = c.conn.SetReadDeadline(deadline)
		encrypted := make([]byte, 32<<10)
		n, err := c.conn.Read(encrypted)
		if n > 0 {
			plain := make([]byte, n)
			c.recvCipher.XORKeyStream(plain, encrypted[:n])
			c.recvPlain = append(c.recvPlain, plain...)
			continue
		}
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return linkdFrame{}, err
		}
	}
}

func (c *linkdClient) awaitURI(ctx context.Context, uri int32) (linkdFrame, error) {
	for {
		frame, err := c.readFrame(ctx)
		if err != nil {
			return linkdFrame{}, err
		}
		if frame.URI == uri {
			return frame, nil
		}
		if err := c.preserveLiveEvent(frame); err != nil {
			return linkdFrame{}, err
		}
	}
}

// awaitURISequence ignores delayed or duplicate replies for an earlier request
// that uses the same response URI. Setup requests are sent serially, so a
// mismatched sequence cannot belong to the command currently awaiting a reply.
func (c *linkdClient) awaitURISequence(ctx context.Context, uri int32, sequenceOffset int, want uint32) (linkdFrame, error) {
	for {
		frame, err := c.readFrame(ctx)
		if err != nil {
			return linkdFrame{}, err
		}
		if frame.URI == uri {
			if sequenceOffset < 0 || len(frame.Body) < sequenceOffset+4 {
				return linkdFrame{}, fmt.Errorf("decode BIGO response uri=%d sequence", uri)
			}
			got := binary.LittleEndian.Uint32(frame.Body[sequenceOffset : sequenceOffset+4])
			if got == want {
				return frame, nil
			}
			continue
		}
		if err := c.preserveLiveEvent(frame); err != nil {
			return linkdFrame{}, err
		}
	}
}

func (c *linkdClient) preserveLiveEvent(frame linkdFrame) error {
	// Preserve a push delivered while follow/notice setup is awaiting a
	// command response; otherwise a go-live at startup could be lost.
	if event, ok := decodeLiveEvent(frame); ok {
		if err := c.acknowledgeLiveEvent(event); err != nil {
			return err
		}
		c.pendingEvents = append(c.pendingEvents, event)
	}
	return nil
}

func (c *linkdClient) login(ctx context.Context) error {
	body := &wireWriter{}
	body.u32(c.session.UID)
	body.blob(c.session.Cookie)
	// Web-issued tickets require the web login secret, not the Android secret.
	body.string("0")
	body.string(c.session.Username)
	body.string(c.session.DeviceID)
	body.i32(0)
	body.i16(linkdForegroundStatus)
	body.i16(0)
	body.i32(0)
	body.i8(0)
	body.i32(0)
	body.i16(0)
	body.i8(0)
	body.i16(0)
	// Native client type 1 was verified receiving a target-attributed live start
	// with a web-issued ticket. Web client type 7 only proved authentication.
	body.i8(1)
	body.i16(0)
	body.i32(0)
	body.i16(0)
	body.i32(0)
	body.string("")
	body.string("")
	body.string("CN")
	body.blob(nil)
	body.u32(0)
	body.emptyStringMap()
	if err := c.sendFrame(uriPrepareLogin, body.Bytes()); err != nil {
		return err
	}
	loginCtx, cancel := context.WithTimeout(ctx, linkdIOTimeout)
	defer cancel()
	response, err := c.awaitURI(loginCtx, uriPrepareLoginRes)
	if err != nil {
		return fmt.Errorf("wait for BIGO prepare-login response: %w", err)
	}
	reader := &wireReader{data: response.Body}
	responseUID, err := reader.i32()
	if err != nil {
		return errors.New("decode BIGO prepare-login response uid")
	}
	result, err := reader.i32()
	if err != nil {
		return errors.New("decode BIGO prepare-login response result")
	}
	if result != protocolStatusOK {
		return fmt.Errorf("%w: web-issued ticket uid=%d prepare-login result=%d", errProtocolRejected, uint32(responseUID), result)
	}
	if uint32(responseUID) != c.session.UID {
		return errors.New("BIGO prepare-login returned a different account uid")
	}
	login := &wireWriter{}
	login.string("CN")
	if err := c.sendFrame(uriLogin, login.Bytes()); err != nil {
		return err
	}
	response, err = c.awaitURI(loginCtx, uriLoginRes)
	if err != nil {
		return fmt.Errorf("wait for BIGO login response: %w", err)
	}
	reader = &wireReader{data: response.Body}
	result, err = reader.i32()
	if err != nil {
		return errors.New("decode BIGO login response")
	}
	if result != 0 && result != protocolStatusOK {
		return fmt.Errorf("%w: login result=%d", errProtocolRejected, result)
	}
	return nil
}

func (c *linkdClient) addFollow(ctx context.Context, targetUID uint32) error {
	uid := c.session.UID
	seq := c.nextSequence()
	body := &wireWriter{}
	body.i32(protocolAppID)
	body.u32(seq)
	body.u32(uid)
	body.integerList([]uint32{targetUID})
	body.i32(0)
	body.i32(0)
	body.i32(0)
	body.emptyStringMap()
	if err := c.sendFrame(uriAddFollow, body.Bytes()); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, linkdIOTimeout)
	defer cancel()
	response, err := c.awaitURISequence(waitCtx, uriAddFollowRes, 4, seq)
	if err != nil {
		return fmt.Errorf("wait for BIGO follow response: %w", err)
	}
	reader := &wireReader{data: response.Body}
	if _, err := reader.i32(); err != nil {
		return errors.New("decode BIGO follow app id")
	}
	if _, err := reader.i32(); err != nil {
		return errors.New("decode BIGO follow sequence")
	}
	// awaitURISequence already correlated this response to seq. Decode it again
	// here to validate the complete response shape before reading the result.
	if _, err := reader.i32(); err != nil {
		return errors.New("decode BIGO follow uid")
	}
	result, err := reader.i32()
	if err != nil {
		return errors.New("decode BIGO follow result")
	}
	if !followResultAccepted(result) {
		return fmt.Errorf("%w: follow result=%d", errProtocolRejected, result)
	}
	return nil
}

func followResultAccepted(result int32) bool {
	return result == 0 || result == protocolFollowAlreadyActive
}

func (c *linkdClient) enableLiveNotice(ctx context.Context, targetUID uint32) error {
	uid := c.session.UID
	seq := c.nextSequence()
	body := &wireWriter{}
	body.i32(protocolAppID)
	body.u32(seq)
	body.u32(uid)
	body.u32(targetUID)
	body.i32(1)
	if err := c.sendFrame(uriSetLiveNotice, body.Bytes()); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, linkdIOTimeout)
	defer cancel()
	response, err := c.awaitURISequence(waitCtx, uriSetLiveNoticeRes, 0, seq)
	if err != nil {
		return fmt.Errorf("wait for BIGO live-notice response: %w", err)
	}
	reader := &wireReader{data: response.Body}
	if _, err := reader.i32(); err != nil {
		return errors.New("decode BIGO live-notice sequence")
	}
	result, err := reader.i32()
	if err != nil {
		return errors.New("decode BIGO live-notice result")
	}
	if result != 0 {
		return fmt.Errorf("%w: live-notice result=%d", errProtocolRejected, result)
	}
	return nil
}

// getWebAuthToken mirrors PCS_GetAuthTokenExternal in BIGO's official Android
// client. The WebView bridge returns the token and response sequence as
// separate callback fields; the website sends only the token to HTTPS.
func (c *linkdClient) getWebAuthToken(ctx context.Context, pageURL string) (string, error) {
	seq := c.nextSequence()
	body := &wireWriter{}
	body.i32(protocolAppID)
	body.u32(seq)
	body.string(pageURL)
	body.string("")
	if err := c.sendFrame(uriGetAuthToken, body.Bytes()); err != nil {
		return "", err
	}
	waitCtx, cancel := context.WithTimeout(ctx, linkdIOTimeout)
	defer cancel()
	response, err := c.awaitURISequence(waitCtx, uriGetAuthTokenRes, 4, seq)
	if err != nil {
		return "", fmt.Errorf("wait for BIGO web-auth response: %w", err)
	}
	reader := &wireReader{data: response.Body}
	appID, err := reader.i32()
	if err != nil {
		return "", errors.New("decode BIGO web-auth app id")
	}
	responseSequence, err := reader.i32()
	if err != nil {
		return "", errors.New("decode BIGO web-auth sequence")
	}
	token, err := reader.string()
	if err != nil {
		return "", errors.New("decode BIGO web-auth token")
	}
	if _, err := reader.i32(); err != nil { // token lifetime in seconds
		return "", errors.New("decode BIGO web-auth lifetime")
	}
	result, err := reader.i32()
	if err != nil {
		return "", errors.New("decode BIGO web-auth result")
	}
	if appID != protocolAppID || uint32(responseSequence) != seq {
		return "", fmt.Errorf("unexpected BIGO web-auth response app=%d sequence=%d", appID, uint32(responseSequence))
	}
	if result != 0 || token == "" {
		return "", fmt.Errorf("%w: web-auth result=%d", errProtocolRejected, result)
	}
	return token, nil
}

func (c *linkdClient) sendPing() error {
	body := &wireWriter{}
	body.i16(0)
	body.u32(c.nextSequence())
	return c.sendFrame(uriPing, body.Bytes())
}

func (c *linkdClient) waitLiveEvent(ctx context.Context) (liveEvent, error) {
	if len(c.pendingEvents) > 0 {
		event := c.pendingEvents[0]
		c.pendingEvents = c.pendingEvents[1:]
		return event, nil
	}
	if c.nextPing.IsZero() {
		c.nextPing = time.Now().Add(linkdPingEvery)
	}
	if c.lastReceived.IsZero() {
		c.lastReceived = time.Now()
	}
	for {
		if time.Since(c.lastReceived) >= linkdIdleTimeout {
			return liveEvent{}, errors.New("BIGO connection stopped responding to heartbeats")
		}
		readFor := time.Until(c.nextPing)
		if readFor <= 0 {
			if err := c.sendPing(); err != nil {
				return liveEvent{}, err
			}
			c.nextPing = time.Now().Add(linkdPingEvery)
			continue
		}
		readCtx, cancelRead := context.WithTimeout(ctx, readFor)
		frame, err := c.readFrame(readCtx)
		cancelRead()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				continue
			}
			return liveEvent{}, err
		}
		if event, ok := decodeLiveEvent(frame); ok {
			if err := c.acknowledgeLiveEvent(event); err != nil {
				return liveEvent{}, err
			}
			return event, nil
		}
	}
}

func decodeLiveEvent(frame linkdFrame) (liveEvent, bool) {
	var (
		event liveEvent
		err   error
	)
	switch frame.URI {
	case uriLiveStartNotify:
		event, err = parseLiveStartFrame(frame)
	case uriNewLiveNotify:
		event, err = parseNewLiveFrame(frame)
	default:
		return liveEvent{}, false
	}
	return event, err == nil
}

func parseLiveStartFrame(frame linkdFrame) (liveEvent, error) {
	reader := &wireReader{data: frame.Body}
	appID, err := reader.i32()
	if err != nil {
		return liveEvent{}, err
	}
	sequence, err := reader.i32()
	if err != nil {
		return liveEvent{}, err
	}
	routeID, err := reader.i32()
	if err != nil {
		return liveEvent{}, err
	}
	content, err := reader.string()
	if err != nil {
		return liveEvent{}, err
	}
	return liveEvent{
		URI: frame.URI, AppID: appID, Sequence: sequence,
		RouteID: routeID, Content: content,
	}, nil
}

func parseNewLiveFrame(frame linkdFrame) (liveEvent, error) {
	reader := &wireReader{data: frame.Body}
	appID, err := reader.i32()
	if err != nil {
		return liveEvent{}, err
	}
	sequence, err := reader.i32()
	if err != nil {
		return liveEvent{}, err
	}
	event := liveEvent{URI: frame.URI, AppID: appID, Sequence: sequence}
	if reader.remaining() > 0 {
		event.Fields, err = reader.stringMap()
		if err != nil {
			return liveEvent{}, err
		}
	}
	return event, nil
}

func (c *linkdClient) acknowledgeLiveEvent(event liveEvent) error {
	if event.URI != uriLiveStartNotify {
		return nil
	}
	uid := c.session.UID
	body := &wireWriter{}
	body.i32(event.AppID)
	body.i32(event.Sequence)
	body.i32(event.RouteID)
	body.u32(uid)
	body.i64(liveMessageID(event.Content))
	body.u32(uint32(time.Now().Unix()))
	if err := c.sendFrame(uriLiveStartNotifyAck, body.Bytes()); err != nil {
		return fmt.Errorf("acknowledge BIGO live-start push: %w", err)
	}
	return nil
}

func liveMessageID(content string) int64 {
	var outer map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &outer) != nil {
		return 0
	}
	reserved := outer["key_reserved"]
	// Accept both the object and JSON-in-a-string forms. Preserve large IDs.
	var quoted string
	if json.Unmarshal(reserved, &quoted) == nil {
		reserved = json.RawMessage(quoted)
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(reserved, &values) != nil {
		return 0
	}
	seqRaw := values["seqid"]
	var seqText string
	if json.Unmarshal(seqRaw, &seqText) == nil {
		value, _ := strconv.ParseInt(strings.TrimSpace(seqText), 10, 64)
		return value
	}
	var seqNumber json.Number
	if json.Unmarshal(seqRaw, &seqNumber) == nil {
		value, _ := strconv.ParseInt(seqNumber.String(), 10, 64)
		return value
	}
	return 0
}
