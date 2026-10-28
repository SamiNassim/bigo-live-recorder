package bigo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"bigo-live-recorder/internal/bigolistener"
)

const (
	pjoinChannel2URI         = 5064
	pjoinChannel2ResURI      = 1224
	pjoinChannel2AppID       = 60
	pjoinChannel2DefaultFlag = 4283
	pjoinChannel2Version     = 10
	pjoinMediaServerTimeout  = 10 * time.Second
	maxNativeCollectionItems = 4096
)

var pjoinClientSeq atomic.Uint32

// ErrOriginUnavailable means native playback cannot be opened for this room.
var ErrOriginUnavailable = errors.New("BIGO supplied no usable native media source")

type mediaProxy struct {
	ip       uint32
	tcp, udp []uint16
}

type pJoinChannelRes struct {
	code                                  uint16
	sequence, source, sid, uid, timestamp uint32
	cookie                                []byte
	media, video                          []mediaProxy
	extra                                 map[string]string
}

// OriginStream queries the current room using the listener's existing login.
// Streamer UIDs and room channel IDs are different provider identifiers.
func (c *Client) OriginStream(ctx context.Context, source Stream, hub *bigolistener.Hub) (Stream, error) {
	if hub == nil {
		return Stream{}, bigolistener.ErrNoConnection
	}
	room, err := strconv.ParseUint(source.RoomID, 10, 64)
	if err != nil || uint32(room) == 0 {
		return Stream{}, errors.New("BIGO supplied no valid room channel ID")
	}
	sid := uint32(room)
	queryCtx, cancel := context.WithTimeout(ctx, pjoinMediaServerTimeout)
	defer cancel()
	sequence := nextPJoinSeq()
	body, err := queryMediaChannel(queryCtx, hub, sequence, sid)
	if err != nil {
		return Stream{}, fmt.Errorf("query BIGO media channel: %w", err)
	}
	response, err := unmarshalPJoinChannelRes(body)
	if err != nil {
		return Stream{}, fmt.Errorf("decode BIGO media channel: %w", err)
	}
	if response.code != 200 {
		return Stream{}, fmt.Errorf("BIGO media channel rejected: code=%d", response.code)
	}
	if response.sequence != sequence || response.sid != sid {
		return Stream{}, errors.New("BIGO media response belongs to a different request or room")
	}
	if len(response.cookie) == 0 || len(response.cookie) > 65535 || len(response.video) == 0 || len(response.media) == 0 {
		return nativePlayback(source, response.extra["hls"])
	}
	stream := source
	stream.native = &response
	stream.Quality = "origin"
	return stream, nil
}

func queryMediaChannel(ctx context.Context, hub *bigolistener.Hub, sequence, sid uint32) ([]byte, error) {
	build := func(uid uint32) ([]byte, func([]byte) bool, error) {
		body := marshalPJoinChannel2(sequence, uid, sid)
		match := func(reply []byte) bool {
			return len(reply) >= 6 && binary.LittleEndian.Uint32(reply[2:6]) == sequence
		}
		return body, match, nil
	}
	for {
		frame, err := hub.QueryAccount(ctx, pjoinChannel2URI, pjoinChannel2ResURI, build)
		if err == nil {
			if frame.Status != 200 {
				return nil, fmt.Errorf("BIGO media channel rejected: status=%d", frame.Status)
			}
			return frame.Body, nil
		}
		if !errors.Is(err, bigolistener.ErrNoConnection) {
			return nil, err
		}
		// A new live target can resolve before the shared listener's first
		// connection is ready. Wait locally without creating another login.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func nativePlayback(source Stream, hls string) (Stream, error) {
	if hls == "" {
		return Stream{}, ErrOriginUnavailable
	}
	stream := source
	stream.HLS, stream.CDN = hls, nil
	if stream.URL() == "" {
		return Stream{}, errors.New("BIGO returned an invalid native playback URL")
	}
	// A native URL alone does not prove a particular resolution.
	stream.Quality = "native"
	return stream, nil
}

func nextPJoinSeq() uint32 {
	for {
		if sequence := pjoinClientSeq.Add(1); sequence != 0 {
			return sequence
		}
	}
}

// PJoinChannel2 uses the viewer UID as srcId and the low 32 bits of the full
// room ID as sid. A media lookup never performs another account login.
func marshalPJoinChannel2(sequence, viewer, sid uint32) []byte {
	body := make([]byte, 31)
	binary.LittleEndian.PutUint32(body[0:4], sequence)
	binary.LittleEndian.PutUint32(body[4:8], viewer)
	binary.LittleEndian.PutUint32(body[8:12], sid)
	binary.LittleEndian.PutUint16(body[12:14], pjoinChannel2DefaultFlag)
	body[18] = 1
	binary.LittleEndian.PutUint32(body[19:23], pjoinChannel2AppID)
	binary.LittleEndian.PutUint32(body[27:31], pjoinChannel2Version)
	return body
}

func unmarshalPJoinChannelRes(body []byte) (pJoinChannelRes, error) {
	r := &nativeReader{data: body}
	var result pJoinChannelRes
	result.code = r.u16()
	result.sequence = r.u32()
	if result.code != 200 {
		return result, r.err
	}
	result.source, result.sid, result.uid = r.u32(), r.u32(), r.u32()
	result.cookie = r.blob()
	result.timestamp = r.u32()
	r.u32()
	result.media, result.video = r.proxies(), r.proxies()
	// Newer versions append suffix fields. A missing suffix is allowed; a
	// partially present field or invalid collection length is rejected.
	steps := []func(){
		func() { r.take(8) },
		func() { r.skipList(8) },
		func() { r.take(4) },
		func() { r.take(2) },
		func() { r.take(1); r.skipList(8) },
		func() { r.take(8) },
		func() { r.take(8) },
		func() { r.skipList(12) },
		func() {
			for range r.count(2) {
				r.blob()
			}
		},
		func() {
			result.extra = make(map[string]string)
			for range r.count(4) {
				key, value := string(r.blob()), string(r.blob())
				result.extra[key] = value
			}
		},
		func() { r.skipList(8); r.skipList(8) },
	}
	for _, read := range steps {
		if r.err != nil || r.remaining() == 0 {
			break
		}
		read()
	}
	if r.err == nil && r.remaining() != 0 {
		r.err = errors.New("unexpected bytes after BIGO media channel response")
	}
	return result, r.err
}

// nativeReader bounds all lengths before slicing or allocating. Proxy IPs
// are uint32 and ports uint16, rather than strings and int32 port values.
type nativeReader struct {
	data []byte
	off  int
	err  error
}

func (r *nativeReader) remaining() int { return len(r.data) - r.off }
func (r *nativeReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > r.remaining() {
		r.err = io.ErrUnexpectedEOF
		return nil
	}
	value := r.data[r.off : r.off+n]
	r.off += n
	return value
}
func (r *nativeReader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}
func (r *nativeReader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}
func (r *nativeReader) blob() []byte { return r.take(int(r.u16())) }
func (r *nativeReader) count(minBytes int) int {
	n := r.u32()
	if r.err != nil {
		return 0
	}
	if n > maxNativeCollectionItems || uint64(n)*uint64(minBytes) > uint64(r.remaining()) {
		r.err = errors.New("invalid BIGO media collection length")
		return 0
	}
	return int(n)
}
func (r *nativeReader) skipList(width int) { r.take(r.count(width) * width) }
func (r *nativeReader) proxies() []mediaProxy {
	count := r.count(12)
	var proxies []mediaProxy
	for range count {
		proxy := mediaProxy{ip: r.u32()}
		for range r.count(2) {
			proxy.tcp = append(proxy.tcp, r.u16())
		}
		for range r.count(2) {
			proxy.udp = append(proxy.udp, r.u16())
		}
		proxies = append(proxies, proxy)
	}
	return proxies
}
