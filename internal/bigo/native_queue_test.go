package bigo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
)

func TestNativeCaptureBufferSpillsAndPreservesFrameOrder(t *testing.T) {
	q := newNativeFrameQueue(16)
	t.Cleanup(q.close)
	var expected []nativeMediaFrame
	for i := range 400 {
		f := nativeMediaFrame{data: bytes.Repeat([]byte{byte(i)}, 9), pts: uint32(i), dts: uint32(i), video: i%2 == 0, key: i%7 == 0, codec: 2, reset: i == 55, cut: i == 56}
		expected = append(expected, f)
		if err := q.push(f); err != nil {
			t.Fatal(err)
		}
	}
	if q.bytes > 16 || q.file == nil {
		t.Fatal("capture buffer did not bound memory")
	}
	name := q.file.Name()
	wantErr := errors.New("fixture receive failure")
	q.finish(wantErr)
	for i, want := range expected {
		got, err := q.next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.pts != want.pts || got.dts != want.dts || got.video != want.video || got.key != want.key || got.codec != want.codec || got.reset != want.reset || got.cut != want.cut || !bytes.Equal(got.data, want.data) {
			t.Fatal("capture frame changed", i)
		}
	}
	if _, err := q.next(context.Background()); !errors.Is(err, wantErr) {
		t.Fatal("capture error was lost", err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("drained capture spool was retained", err)
	}
}

func TestNativeCaptureBufferConcurrentSlowConsumer(t *testing.T) {
	q := newNativeFrameQueue(64)
	defer q.close()
	var wg sync.WaitGroup
	wg.Go(func() {
		defer q.finish(nil)
		for i := range 1000 {
			if err := q.push(nativeMediaFrame{pts: uint32(i), data: []byte{byte(i)}}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for i := range 1000 {
		f, err := q.next(context.Background())
		if err != nil || f.pts != uint32(i) {
			t.Fatal("buffer reordered reception", i, f.pts, err)
		}
	}
	wg.Wait()
	if _, err := q.next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestNativeCaptureBufferCloseRemovesPendingSpool(t *testing.T) {
	q := newNativeFrameQueue(0)
	if err := q.push(nativeMediaFrame{data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	name := q.file.Name()
	q.close()
	q.close()
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := q.push(nativeMediaFrame{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newNativeFrameQueue(0).next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
