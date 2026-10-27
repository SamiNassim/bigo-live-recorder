package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bigo-live-recorder/internal/assets"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
)

func TestSidebarUsesSmallArtwork(t *testing.T) {
	a := accountFixture(t)
	if w, h := a.logo.Size(); w != logoPixels || h != logoPixels {
		t.Fatalf("sidebar retained the original icon: %dx%d", w, h)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(assets.Icon))
	if err != nil || cfg.Width <= logoPixels {
		t.Fatal("the full-resolution application icon was replaced", cfg, err)
	}
}

func TestThumbnailPreservesCoverageAndTransparency(t *testing.T) {
	src := image.NewNRGBA(image.Rect(5, 7, 13, 15))
	for y := 7; y < 15; y++ {
		for x := 5; x < 13; x++ {
			c := color.NRGBA{R: 255, G: 255, B: 255}
			if (x+y)%2 == 0 {
				c = color.NRGBA{R: 255, A: 255}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	got := resizeThumbnail(src, 2)
	if got.Bounds().Dx() != 2 || got.Bounds().Dy() != 2 {
		t.Fatal(got.Bounds())
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			c := color.RGBAModel.Convert(got.At(x, y)).(color.RGBA)
			if c != (color.RGBA{R: 128, A: 128}) {
				t.Fatal("downsampling introduced a fringe or lost coverage", c)
			}
		}
	}
	small := image.NewRGBA(image.Rect(0, 0, 32, 16))
	if resizeThumbnail(small, avatarPixels) != small {
		t.Fatal("small images were enlarged")
	}
	wide := resizeThumbnail(image.NewRGBA(image.Rect(0, 0, 512, 256)), avatarPixels)
	if wide.Bounds().Dx() != 128 || wide.Bounds().Dy() != 64 {
		t.Fatal("aspect ratio changed", wide.Bounds())
	}
}

func TestAvatarFetchBoundsAndHeaders(t *testing.T) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 512, 256))); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("account credentials were sent to the avatar host")
		}
		switch r.URL.Path {
		case "/image":
			w.Write(pngData.Bytes())
		case "/large":
			w.Write(bytes.Repeat([]byte("x"), (2<<20)+1))
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer server.Close()
	bitmap, err := fetchAvatar(context.Background(), server.Client(), server.URL+"/image")
	if err != nil {
		t.Fatal(err)
	}
	if w, h := bitmap.Size(); w != 128 || h != 64 {
		t.Fatal("avatar kept full-size pixels", w, h)
	}
	for _, path := range []string{"/large", "/failure"} {
		if _, err := fetchAvatar(context.Background(), server.Client(), server.URL+path); err == nil {
			t.Fatal("invalid response accepted", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchAvatar(ctx, server.Client(), server.URL+"/image"); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestAvatarCacheEvictsOldImagesAndRemovedTargets(t *testing.T) {
	a := accountFixture(t)
	for i := range maxCachedAvatars + 1 {
		address := fmt.Sprintf("https://images.example.invalid/%d.png", i)
		a.snapshot.Rows = append(a.snapshot.Rows, monitor.Row{Streamer: state.Streamer{ID: fmt.Sprint(i), Avatar: address}})
	}
	for i := range maxCachedAvatars {
		a.keepAvatar(a.snapshot.Rows[i].Streamer.Avatar, a.logo)
	}
	first := a.snapshot.Rows[0].Streamer.Avatar
	a.avatarClock++
	a.avatarUsed[first] = a.avatarClock
	latest := a.snapshot.Rows[maxCachedAvatars].Streamer.Avatar
	a.keepAvatar(latest, a.logo)
	if len(a.avatars) != maxCachedAvatars || a.avatars[first] == nil || a.avatars[a.snapshot.Rows[1].Streamer.Avatar] != nil {
		t.Fatal("cache did not evict its least recently used thumbnail")
	}
	removed := first
	a.avatarRetry[removed] = time.Now().Add(time.Minute)
	a.snapshot.Rows = a.snapshot.Rows[maxCachedAvatars:]
	a.pruneAvatars()
	if len(a.avatars) != 1 || len(a.avatarRetry) != 0 {
		t.Fatal("removed targets retained images or retries")
	}
	a.keepAvatar(removed, a.logo)
	if len(a.avatars) != 1 {
		t.Fatal("a stale download restored a removed image")
	}
	for i := range maxAvatarDownloads {
		a.avatarLoading[fmt.Sprint(i)] = true
	}
	a.loadAvatar("https://images.example.invalid/blocked.png")
	if len(a.avatarLoading) != maxAvatarDownloads {
		t.Fatal("avatar download concurrency is unbounded")
	}
}

func TestRefreshReusesUnchangedSnapshots(t *testing.T) {
	a := accountFixture(t)
	if err := a.store.Update(func(d *state.Data) error {
		d.Recordings = []state.Recording{{ID: "synthetic-recording", Path: "recording.flv"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.log.Write("INFO", "", "Synthetic entry")
	a.refresh()
	data, entries := &a.data.Recordings[0], &a.logs[0]
	if n := testing.AllocsPerRun(100, a.refresh); n != 0 {
		t.Fatalf("unchanged refresh allocated %.1f objects", n)
	}
	if &a.data.Recordings[0] != data || &a.logs[0] != entries {
		t.Fatal("unchanged data was copied")
	}
	a.log.ClearView()
	a.refresh()
	if len(a.logs) != 0 {
		t.Fatal("cleared log entries remained visible")
	}
	if err := a.store.Update(func(d *state.Data) error { d.Recordings = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	if len(a.data.Recordings) != 0 {
		t.Fatal("cleared recording history remained visible")
	}
}

func BenchmarkIdleRefresh(b *testing.B) {
	store, err := state.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	log, err := logging.Open(store.Directory())
	if err != nil {
		b.Fatal(err)
	}
	defer log.Close()
	if err := store.Update(func(d *state.Data) error { d.Recordings = make([]state.Recording, 2000); return nil }); err != nil {
		b.Fatal(err)
	}
	for i := range logging.MaxEntries {
		log.Write("INFO", "", fmt.Sprintf("Synthetic log %d", i))
	}
	a := newApplication(store, log)
	defer a.cancel()
	a.refresh()
	b.Run("copy-every-time", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			a.data = store.Data()
			a.logs = log.Entries()
		}
	})
	b.Run("unchanged-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			a.refresh()
		}
	})
}
