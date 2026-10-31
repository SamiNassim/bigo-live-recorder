package bigo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func nativeTestAudioPacket(samples ...[]byte) []byte {
	b := []byte{2, 0, byte(len(samples) << 5), 0xa0}
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 4000)
	b = append(b, 0, 6)
	b = binary.LittleEndian.AppendUint32(b, 0)
	for _, sample := range samples {
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint16(b, 0xfc00|uint16(len(sample)))
		b = append(b, sample...)
	}
	return b
}

func writeNativeTestPacket(w io.Writer, uri uint32, body []byte) error {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(body)+10))
	b = binary.LittleEndian.AppendUint32(b, uri)
	b = binary.LittleEndian.AppendUint16(b, 200)
	_, err := w.Write(append(b, body...))
	return err
}

func TestNativeAudioRetainsValidSamplesAroundMalformedAAC(t *testing.T) {
	adts := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0x21}
	bad := []byte{0, 1, 2}
	for _, tt := range []struct {
		name    string
		samples [][]byte
		pts     []uint32
	}{
		{"middle", [][]byte{adts, bad, adts}, []uint32{4000, 4046}},
		{"first", [][]byte{bad, adts}, []uint32{4023}},
		{"last", [][]byte{adts, bad}, []uint32{4000}},
		{"all", [][]byte{bad, nil}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			frames, err := decodeNativeAudio(nativeTestAudioPacket(tt.samples...))
			if !errors.Is(err, errNativeAACFrame) || len(frames) != len(tt.pts) {
				t.Fatal("unexpected audio rejection", len(frames), err)
			}
			for i, f := range frames {
				if f.pts != tt.pts[i] || f.dts != f.pts || !bytes.Equal(f.data, adts) {
					t.Fatal("valid AAC changed or moved on the timeline", i, f)
				}
			}
			if !strings.Contains(err.Error(), "codec=6 flags=0x0 bytes=") {
				t.Fatal("missing non-secret packet diagnostics", err)
			}
		})
	}
}

func TestNativeAudioHonorsADTSRawBlockCount(t *testing.T) {
	adts := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfe, 0x21}
	frames, err := decodeNativeAudio(nativeTestAudioPacket(adts, adts))
	if err != nil || len(frames) != 2 || frames[1].pts != 4069 {
		t.Fatal("multiple AAC raw blocks lost their duration", frames, err)
	}
}

func TestNativeAudioTruncationStillRejectsPacket(t *testing.T) {
	adts := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0x21}
	b := nativeTestAudioPacket(adts, adts)
	frames, err := decodeNativeAudio(b[:len(b)-1])
	if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errNativeAACFrame) || len(frames) != 0 {
		t.Fatal("truncated wire framing was treated as one bad sample", frames, err)
	}
}

func TestNativeAudioReceiverContinuesAfterRejectedSample(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	server.SetWriteDeadline(time.Now().Add(2 * time.Second))
	var warnings []string
	c := nativeMediaConn{Conn: client, report: func(_, message string) { warnings = append(warnings, message) }}
	out := make(chan nativeMediaFrame, 3)
	done := make(chan error, 1)
	go func() { done <- c.receive(context.Background(), out) }()
	adts := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0x21}
	for _, body := range [][]byte{
		nativeTestAudioPacket([]byte{0, 1, 2}),
		nativeTestAudioPacket(adts, nil, adts),
		nativeTestAudioPacket(adts),
	} {
		if err := writeNativeTestPacket(server, 2, body); err != nil {
			t.Fatal(err)
		}
	}
	server.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("healthy audio did not clear the malformed state", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audio receiver did not finish")
	}
	if len(out) != 3 || len(warnings) != 0 {
		t.Fatal("lost valid audio or logged a recoverable sample rejection", len(out), warnings)
	}
	for len(out) > 0 {
		if !bytes.Equal((<-out).data, adts) {
			t.Fatal("invalid AAC reached the muxer")
		}
	}
}

func TestNativeStartupWaitsForAudioOnVideoClock(t *testing.T) {
	for _, freshPTS := range []uint32{4014, 24000} {
		t.Run(map[bool]string{true: "aligned", false: "unrelated"}[freshPTS == 4014], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cached, videoSent, fresh := make(chan struct{}), make(chan struct{}), make(chan struct{})
			listen := func(serve func(net.Conn)) mediaProxy {
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { listener.Close() })
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					defer conn.Close()
					stop := context.AfterFunc(ctx, func() { conn.Close() })
					defer stop()
					conn.SetDeadline(time.Now().Add(5 * time.Second))
					if _, err := readNativePacket(conn); err != nil {
						return
					}
					serve(conn)
					io.Copy(io.Discard, conn)
				}()
				return mediaProxy{ip: 0x0100007f, tcp: []uint16{uint16(listener.Addr().(*net.TCPAddr).Port)}}
			}
			audio := listen(func(conn net.Conn) {
				body := nativeTestAudioPacket([]byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0x21})
				binary.LittleEndian.PutUint32(body[8:], 3000)
				writeNativeTestPacket(conn, 2, body)
				close(cached)
				select {
				case <-fresh:
					binary.LittleEndian.PutUint32(body[8:], freshPTS)
					writeNativeTestPacket(conn, 2, body)
				case <-ctx.Done():
				}
			})
			video := listen(func(conn net.Conn) {
				reply := binary.LittleEndian.AppendUint32(nil, 1234)
				reply = binary.LittleEndian.AppendUint64(reply, 0)
				writeNativeTestPacket(conn, 0x5a02, reply)
				select {
				case <-cached:
					writeNativeTestVideo(conn, nativeTestVideo(100, true))
					close(videoSent)
				case <-ctx.Done():
				}
			})
			type result struct {
				stream io.ReadCloser
				err    error
			}
			done := make(chan result, 1)
			go func() {
				r, err := openNativeStream(ctx, Stream{native: &pJoinChannelRes{source: 1234, sid: 42, video: []mediaProxy{video}, media: []mediaProxy{audio}}})
				done <- result{r, err}
			}()
			select {
			case <-videoSent:
			case <-time.After(2 * time.Second):
				t.Fatal("fixture did not send cached audio and video")
			}
			select {
			case r := <-done:
				if r.stream != nil {
					r.stream.Close()
				}
				t.Fatal("cached audio incorrectly satisfied startup", r.err)
			case <-time.After(50 * time.Millisecond):
			}
			close(fresh)
			select {
			case r := <-done:
				if r.stream != nil {
					r.stream.Close()
				}
				if (r.err == nil) != (freshPTS == 4014) {
					t.Fatal("incorrect startup clock validation", r.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("startup did not finish after fresh audio")
			}
		})
	}
}
