package bigo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"bigo-live-recorder/internal/fault"
)

const (
	nativeMediaClientVersion = "6.53.0"
	nativeMediaAudienceRole  = 3
)

type nativeMediaConn struct {
	net.Conn
	mu           sync.Mutex
	viewer, sid  uint32
	owner        uint32
	video        bool
	guest        bool
	subscribed   bool
	subscription uint64
	confirmed    bool
	report       func(string, string)
	availability []byte
	setupStarted time.Time
	mediaEpoch   time.Time
	// Setup-relative nanoseconds plus one. Zero means the event has not happened.
	lastPlayable     atomic.Int64
	confirmedAfter   atomic.Int64
	keepaliveFailure atomic.Pointer[nativeKeepaliveFailure]
}

func dialNativeMedia(ctx context.Context, routes []mediaProxy, viewer, sid uint32, video bool) (*nativeMediaConn, error) {
	var last error
	for _, route := range routes {
		var ip [4]byte
		binary.LittleEndian.PutUint32(ip[:], route.ip)
		for _, port := range route.tcp {
			if port == 0 {
				continue
			}
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(net.IP(ip[:]).String(), strconv.Itoa(int(port))))
			if err == nil {
				return &nativeMediaConn{Conn: conn, viewer: viewer, sid: sid, video: video}, nil
			}
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	if last == nil {
		last = ErrOriginUnavailable
	}
	return nil, fmt.Errorf("connect BIGO native media: %w", last)
}

func (c *nativeMediaConn) send(uri uint32, body []byte) error {
	packet := binary.LittleEndian.AppendUint32(nil, uint32(len(body)+10))
	packet = binary.LittleEndian.AppendUint32(packet, uri)
	packet = binary.LittleEndian.AppendUint16(packet, 200)
	packet = append(packet, body...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	for len(packet) > 0 {
		n, err := c.Write(packet)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		packet = packet[n:]
	}
	return nil
}

func appendNativeBlob(body, data []byte) []byte {
	body = binary.LittleEndian.AppendUint16(body, uint16(len(data)))
	return append(body, data...)
}
func appendNativeBytes(body, data []byte) []byte {
	body = binary.LittleEndian.AppendUint32(body, uint32(len(data)))
	return append(body, data...)
}

func nativeLoginBody(channel *pJoinChannelRes, owner uint32, video bool) []byte {
	b := binary.LittleEndian.AppendUint32(nil, channel.source)
	b = binary.LittleEndian.AppendUint32(b, channel.sid)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = appendNativeBlob(b, channel.cookie)
	b = binary.LittleEndian.AppendUint32(b, channel.timestamp)
	if video {
		b = binary.LittleEndian.AppendUint32(b, 0x94)
		b = binary.LittleEndian.AppendUint32(b, owner)
		b = binary.LittleEndian.AppendUint64(b, uint64(time.Now().UnixMilli()))
		b = append(b, 1) // audience fetch
		b = appendNativeBytes(b, nil)
		b = appendNativeBytes(b, []byte{0}) // primary playback channel
		b = append(b, 0)
		for range 3 {
			b = appendNativeBlob(b, nil)
		}
		b = binary.LittleEndian.AppendUint32(b, 10)
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = append(b, 0x8e, 0, 0, 1) // mode, Origin resolution, SDK type
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, 0x17)
		b = appendNativeBytes(b, []byte{0, 1, 2, 3, 4})
		// This protocol identity negotiates source encodings. Omitting it can
		// silently select a lower rendition. No device fingerprint is needed.
		b = binary.LittleEndian.AppendUint32(b, 2)
		b = appendNativeBlob(b, []byte("client_version"))
		b = appendNativeBlob(b, []byte(nativeMediaClientVersion))
		b = appendNativeBlob(b, []byte("os"))
		b = appendNativeBlob(b, []byte("0"))
		b = append(b, nativeMediaAudienceRole, 0)
	} else {
		b = binary.LittleEndian.AppendUint32(b, 0x9b7)
		b = append(b, 0)
		b = append(b, make([]byte, 12)...)
		b = appendNativeBytes(b, nil)
		b = appendNativeBytes(b, nil)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, 0)
		b = appendNativeBlob(b, nil)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = appendNativeBlob(b, nil)
		b = appendNativeBlob(b, nil)
		b = append(b, make([]byte, 20)...)
		b = append(b, 0)
	}
	return b
}

func (c *nativeMediaConn) control(p nativePacket) error {
	if p.status != 200 {
		return fmt.Errorf("BIGO native media rejected request: status=%d", p.status)
	}
	switch p.uri {
	case 0x32402, 0x37a02:
		if len(p.data) < 8 || binary.LittleEndian.Uint32(p.data) != c.viewer || binary.LittleEndian.Uint32(p.data[4:]) != c.sid {
			return errors.New("BIGO native login returned another room or viewer")
		}
		if p.uri == 0x37a02 && (len(p.data) < 10 || (binary.LittleEndian.Uint16(p.data[8:]) != 0 && binary.LittleEndian.Uint16(p.data[8:]) != 6)) {
			return errors.New("BIGO native video login rejected")
		}
	case 0x1142f02:
		if len(p.data) < 28 {
			return io.ErrUnexpectedEOF
		}
		return c.send(0x1143002, append(append([]byte(nil), p.data[:8]...), p.data[16:28]...))
	case 0x36b702:
		if len(p.data) < 20 {
			return io.ErrUnexpectedEOF
		}
		if err := c.send(0x36b802, p.data[:16]); err != nil {
			return err
		}
		if !c.video {
			return nil
		}
		count := binary.LittleEndian.Uint32(p.data[16:])
		if count == 0 {
			return nil
		}
		if count > 128 || uint64(count) > uint64(len(p.data)-20) {
			return errors.New("invalid BIGO native stream list")
		}
		available := p.data[16:]
		if c.subscribed && bytes.Equal(c.availability, available) {
			return nil
		}
		c.availability = append(c.availability[:0], available...)
		if c.subscribed && c.report != nil {
			c.report("DEBUG", "Native stream availability changed; reaffirming the Origin subscription")
		}
		// Audience playback uses the third channel list and role 3. Copying
		// availability values into a publisher list selects another rendition.
		channel, mode, multi := byte(0), byte(0), byte(0)
		fetch := uint32(1)
		if c.guest {
			channel, mode, multi, fetch = 2, 2, 1, 0
		}
		b := binary.LittleEndian.AppendUint32(nil, c.viewer)
		b = binary.LittleEndian.AppendUint32(b, fetch)
		c.subscription = max(c.subscription+1, uint64(time.Now().UnixMilli()))
		b = binary.LittleEndian.AppendUint64(b, c.subscription)
		b = appendNativeBytes(b, nil)
		b = appendNativeBytes(b, nil)
		b = appendNativeBytes(b, []byte{channel})
		b = append(b, 0x98)
		b = binary.LittleEndian.AppendUint32(b, c.sid)
		b = append(b, mode, 0)
		b = append(b, make([]byte, 32)...)
		b = append(b, 5, nativeMediaAudienceRole, multi)
		if err := c.send(0x5902, b); err != nil {
			return err
		}
		c.subscribed = true
	case 0x5a02:
		if len(p.data) < 12 {
			return io.ErrUnexpectedEOF
		}
		if binary.LittleEndian.Uint32(p.data) == c.viewer && binary.LittleEndian.Uint64(p.data[4:]) == c.subscription {
			c.confirmed = true
		}
	case 0x36b502:
		if len(p.data) >= 16 {
			return c.send(0x36b602, p.data[:16])
		}
	}
	return nil
}

type nativeReadCloser struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   <-chan struct{}
}

// NativeTimestamps identifies the normalized clock maintained across native
// media reconnects. Consumers must preserve it instead of correcting it again.
func (r *nativeReadCloser) NativeTimestamps() bool { return true }

func (r *nativeReadCloser) Close() error {
	r.cancel()
	err := r.PipeReader.Close()
	if r.done != nil {
		<-r.done
	}
	return err
}

func openNativeStream(parent context.Context, source Stream) (io.ReadCloser, error) {
	stream, _, err := openNativeStreamClock(parent, source, nil, nil)
	return stream, err
}

type nativeMediaSession struct {
	ctx          context.Context
	cleanup      func()
	workers      *sync.WaitGroup
	events       chan nativeMediaFrame
	failed       chan error
	waiting      []nativeMediaFrame
	end          error
	once         sync.Once
	setup        string
	video        *nativeMediaConn
	audioRetried bool // Owned by the capture receiver.
	mu           sync.Mutex
	audio        *nativeMediaConn
	closed       bool
}

func (s *nativeMediaSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cleanup()
		s.workers.Wait()
	})
	return nil
}

func (s *nativeMediaSession) next(ctx context.Context) (nativeMediaFrame, error) {
	f, err := s.read(ctx)
	if err != nil && s.end == nil && ctx.Err() == nil && s.ctx.Err() == nil {
		s.end = err
		s.Close()
		return s.read(ctx)
	}
	return f, err
}

func (s *nativeMediaSession) read(ctx context.Context) (nativeMediaFrame, error) {
	if len(s.waiting) > 0 {
		f := s.waiting[0]
		s.waiting[0] = nativeMediaFrame{}
		s.waiting = s.waiting[1:]
		return f, nil
	}
	if s.end != nil {
		select {
		case f := <-s.events:
			return f, nil
		default:
			return nativeMediaFrame{}, s.end
		}
	}
	select {
	case f := <-s.events:
		return f, nil
	case err := <-s.failed:
		if err == nil {
			err = io.EOF
		}
		return nativeMediaFrame{}, err
	case <-ctx.Done():
		return nativeMediaFrame{}, ctx.Err()
	case <-s.ctx.Done():
		return nativeMediaFrame{}, s.ctx.Err()
	}
}

func (s *nativeMediaSession) startTrack(conn *nativeMediaConn, channel *pJoinChannelRes) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		conn.Close()
		return false
	}
	if !conn.video {
		s.audio = conn
	}
	conn.mediaEpoch = s.video.setupStarted
	ctx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	s.workers.Go(func() {
		defer cancel()
		defer stop()
		defer conn.Close()
		err := conn.receive(ctx, s.events)
		peer := s.video
		if conn.video {
			s.mu.Lock()
			peer = s.audio
			s.mu.Unlock()
		}
		select {
		case s.failed <- conn.trackFailure(peer, err):
		case <-ctx.Done():
		}
	})
	s.workers.Go(func() { conn.keepalive(ctx, channel) })
	return true
}

func (c *nativeMediaConn) keepalive(ctx context.Context, channel *pJoinChannelRes) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b := binary.LittleEndian.AppendUint32(nil, channel.source)
			b = binary.LittleEndian.AppendUint32(b, channel.sid)
			b = binary.LittleEndian.AppendUint32(b, uint32(time.Now().UnixMilli()))
			b = binary.LittleEndian.AppendUint32(b, channel.timestamp)
			b = binary.LittleEndian.AppendUint32(b, 0)
			if err := c.send(0x32e02, appendNativeBlob(b, nil)); err != nil {
				c.keepaliveFailure.Store(&nativeKeepaliveFailure{err: err})
				c.Close()
				return
			}
		}
	}
}

func (c *nativeMediaConn) login(channel *pJoinChannelRes, owner uint32) error {
	uri := uint32(0x32302)
	if c.video {
		uri = 0x37902
	}
	return c.send(uri, nativeLoginBody(channel, owner, c.video))
}

func openNativeMediaSession(parent context.Context, source Stream, report func(string, string)) (_ *nativeMediaSession, resultErr error) {
	ctx, cancel := context.WithCancel(parent)
	channel := source.native
	if channel == nil {
		cancel()
		return nil, ErrOriginUnavailable
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 12*time.Second)
	defer setupCancel()
	setup := newNativeSetupLog()
	defer func() {
		if resultErr != nil {
			resultErr = fault.WithDetails(resultErr, setup.details())
		}
	}()
	video, err := dialNativeMedia(setupCtx, channel.video, channel.source, channel.sid, true)
	setup.mark("video TCP dial finished")
	if err != nil {
		cancel()
		return nil, fault.Wrap("could not connect to BIGO's native video server: "+fault.Reason(err), err)
	}
	audio, err := dialNativeMedia(setupCtx, channel.media, channel.source, channel.sid, false)
	setup.mark("audio TCP dial finished")
	if err != nil {
		video.Close()
		cancel()
		return nil, fault.Wrap("could not connect to BIGO's native audio server: "+fault.Reason(err), err)
	}
	audio.report = report
	video.report = report
	video.owner = source.UID
	audio.setupStarted, video.setupStarted = setup.started, setup.started
	setup.video = video
	stop := context.AfterFunc(ctx, func() { video.Close(); audio.Close() })
	defer stop()
	for _, conn := range []*nativeMediaConn{video, audio} {
		err = conn.login(channel, source.UID)
		setup.mark(conn.track() + " login write finished")
		if err != nil {
			cancel()
			video.Close()
			audio.Close()
			return nil, fault.Wrap("could not send the native "+conn.track()+" stream setup request: "+fault.Reason(err), err)
		}
	}
	s := &nativeMediaSession{ctx: ctx, cleanup: cancel, workers: &sync.WaitGroup{},
		events: make(chan nativeMediaFrame, 128), failed: make(chan error, 2), video: video, audio: audio}
	defer func() {
		if resultErr != nil {
			s.Close()
		}
	}()
	for _, conn := range []*nativeMediaConn{video, audio} {
		if !s.startTrack(conn, channel) {
			video.Close()
			audio.Close()
			return nil, ctx.Err()
		}
	}
	var first *nativeMediaFrame
	audioReady := false
	var waiting []nativeMediaFrame
	for first == nil || !audioReady {
		select {
		case <-setupCtx.Done():
			message := "timed out waiting for the first playable native video frame"
			if first != nil {
				message = "timed out waiting for native audio to start alongside the video"
			}
			if parent.Err() != nil {
				message = "native stream setup was interrupted"
			}
			return nil, fault.Wrap(message, setupCtx.Err())
		case err := <-s.failed:
			return nil, fault.Wrap("native stream setup failed: "+fault.Reason(err), err)
		case f := <-s.events:
			if f.video && f.key && first == nil {
				setup.keyframe = time.Now()
				setup.videoCodec = f.codec
				if _, err := nativeVideoType(f.codec); err != nil {
					return nil, err
				}
				first = &f
			}
			if first != nil {
				if !f.video {
					delta := int32(f.pts - first.pts)
					if delta < 0 {
						continue // Cached audio predating this video connection.
					}
					if delta > 10000 {
						return nil, fault.Wrap("BIGO's native audio and video could not be synchronized at startup", fmt.Errorf("audio PTS minus first video keyframe PTS was %d ms", delta))
					}
					setup.audio = time.Now()
					setup.audioOffsetMS = delta
					audioReady = true
				}
				waiting = append(waiting, f)
			}
			if len(waiting) > 128 {
				return nil, fault.Wrap("native video arrived, but playable audio did not start", errors.New("BIGO native startup audio buffer exceeded 128 frames"))
			}
		}
	}
	s.waiting, s.setup = waiting, setup.details()
	return s, nil
}

func openNativeStreamClock(parent context.Context, source Stream, clock *nativeTSClock, report func(string, string)) (io.ReadCloser, string, error) {
	session, err := openNativeMediaSession(parent, source, report)
	if err != nil {
		return nil, "", err
	}
	codec, _ := nativeVideoType(session.waiting[0].codec)
	if clock != nil {
		clock.mu.Lock()
		changed := clock.started && clock.codec != codec
		clock.mu.Unlock()
		if changed {
			session.Close()
			return nil, "", fault.WithDetails(errors.New("BIGO native video codec changed; a new recording file is required"), session.setup)
		}
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(ctx, func() { session.Close(); writer.CloseWithError(ctx.Err()) })
	go func() {
		defer close(done)
		defer cancel()
		defer stop()
		defer session.Close()
		mux := nativeTSMux{out: writer, clock: clock}
		for {
			f, err := session.next(ctx)
			if err == nil {
				err = mux.writeFrame(f)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				writer.CloseWithError(err)
				return
			}
		}
	}()
	return &nativeReadCloser{PipeReader: reader, cancel: cancel, done: done}, session.setup, nil
}

func (c *nativeMediaConn) receive(ctx context.Context, out chan<- nativeMediaFrame) error {
	router := nativeVideoRouter{owner: c.owner}
	track := c.track()
	var invalidAudio error
	lastMedia := time.Now()
	for {
		if err := c.SetReadDeadline(lastMedia.Add(20 * time.Second)); err != nil {
			return fault.WithDetails(err, "could not set the native media read deadline")
		}
		packet, err := readNativePacket(c.Conn)
		if err != nil {
			if invalidAudio != nil {
				return fault.Wrap("playable native audio stopped arriving", errors.Join(err, invalidAudio))
			}
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return fault.Wrap("no playable native "+track+" arrived for 20 seconds", err)
			}
			return fault.Wrap("the native "+track+" connection was interrupted: "+fault.Reason(err), err)
		}
		var frames []nativeMediaFrame
		if c.video && packet.uri == 5 {
			if !c.confirmed {
				continue
			}
			p, e := decodeNativeVideo(packet.data)
			if e != nil {
				return fault.Wrap("BIGO sent unreadable native video data", e)
			}
			frames, e = router.add(p)
			if e != nil {
				return fault.Wrap("native video fragments could not be assembled into playable frames", e)
			}
		} else if !c.video && packet.uri == 2 {
			frames, err = decodeNativeAudio(packet.data)
			if errors.Is(err, errNativeAACFrame) {
				invalidAudio = err
				// Do not tear down healthy video for one bad audio sample. A
				// continuing invalid stream still hits the media deadline, which
				// advances only when usable audio is delivered.
			} else if err != nil {
				return fault.Wrap("BIGO sent unreadable native audio data", err)
			}
		} else {
			confirmed := c.confirmed
			if err = c.control(packet); err != nil {
				return fault.Wrap("BIGO could not maintain the native media subscription", fmt.Errorf("native %s control message %#x: %w", track, packet.uri, err))
			}
			if !confirmed && c.confirmed && !c.setupStarted.IsZero() {
				c.confirmedAfter.Store(int64(time.Since(c.setupStarted)) + 1)
			}
		}
		for _, f := range frames {
			lastMedia = time.Now()
			if !c.setupStarted.IsZero() {
				c.lastPlayable.Store(int64(lastMedia.Sub(c.setupStarted)) + 1)
			}
			if !c.mediaEpoch.IsZero() {
				f.received = int64(lastMedia.Sub(c.mediaEpoch)) + 1
			}
			if !f.video {
				invalidAudio = nil
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
