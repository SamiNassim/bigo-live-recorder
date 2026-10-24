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
	legacyRSAKeyBits            = 512
	legacyRSAExponent           = 65537
	legacySecret                = "YjRkNjkyMzEtNDg1ZC00NWYxLTkxNzgtZmRlOWY3ZmFkZTQ0"
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
	writeMu       sync.Mutex
	seq           uint32
	session       Session
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
	if c == nil || c.conn == nil {
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
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	plain := marshalFrame(uri, body)
	encrypted := make([]byte, len(plain))
	c.sendCipher.XORKeyStream(encrypted, plain)
	_ = c.conn.SetWriteDeadline(time.Now().Add(linkdIOTimeout))
	if err := writeAll(c.conn, encrypted); err != nil {
		return fmt.Errorf("send BIGO Linkd uri=%d: %w", uri, err)
	}
	_ = c.conn.SetWriteDeadline(time.Time{})
	return nil
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
				return parseFrame(raw)
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
	uid, _ := c.session.accountUID()
	uuid, _ := c.session.accountUUID()
	cookie, _ := c.session.cookie()
	securityPacket, _ := c.session.securityPacket()
	body := &wireWriter{}
	body.u32(uid)
	body.blob(cookie)
	body.string(legacySecret)
	body.string(c.session.Username)
	body.string(c.session.DeviceID)
	body.i32(c.session.ComposedClientVersion)
	body.i16(linkdForegroundStatus)
	body.i16(0)
	body.i32(c.session.SDKVersion)
	body.i8(0)
	body.i32(0)
	body.i16(c.session.LanguageCode)
	body.i8(0)
	body.i16(c.session.ClientVersionCode)
	body.i8(c.session.ClientType)
	body.i16(c.session.AndroidSDK)
	body.i32(0)
	body.i16(0)
	body.i32(0)
	body.string("")
	body.string("")
	body.string(c.session.CountryCode)
	body.blob(securityPacket)
	body.u32(uuid)
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
		return fmt.Errorf("%w: captured session uid=%d prepare-login result=%d", errProtocolRejected, uint32(responseUID), result)
	}
	login := &wireWriter{}
	login.string(c.session.CountryCode)
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
	uid, _ := c.session.accountUID()
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
	uid, _ := c.session.accountUID()
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
	nextPing := time.Now().Add(linkdPingEvery)
	for {
		readFor := time.Until(nextPing)
		if readFor <= 0 {
			if err := c.sendPing(); err != nil {
				return liveEvent{}, err
			}
			nextPing = time.Now().Add(linkdPingEvery)
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
	uid, err := c.session.accountUID()
	if err != nil {
		return err
	}
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
	var reserved string
	if json.Unmarshal(outer["key_reserved"], &reserved) != nil || strings.TrimSpace(reserved) == "" {
		return 0
	}
	var values map[string]json.RawMessage
	if json.Unmarshal([]byte(reserved), &values) != nil {
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
