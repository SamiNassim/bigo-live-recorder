package app

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func TestAvatarUpgradesHTTPAndDisplaysWithOriginalCacheKey(t *testing.T) {
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 256, 256)), nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("avatar was not downloaded over HTTPS")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("avatar download included account credentials")
		}
		if r.URL.RequestURI() != "/avatar.jpg?size=small" {
			t.Error("avatar path or query changed", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(data.Bytes())
	}))
	defer server.Close()
	original := strings.Replace(server.URL, "https://", "http://", 1) + "/avatar.jpg?size=small"
	address, err := avatarDownloadURL(original)
	if err != nil {
		t.Fatal(err)
	}
	bitmap, err := fetchAvatar(context.Background(), server.Client(), address)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := bitmap.Size(); w != avatarPixels || h != avatarPixels {
		t.Fatal("avatar was not reduced to a thumbnail", w, h)
	}
	a := accountFixture(t)
	a.snapshot.Rows = []monitor.Row{{Streamer: state.Streamer{ID: "900000002", Name: "Demo Streamer", Avatar: original}}}
	a.keepAvatar(original, bitmap)
	a.pruneAvatars()
	tt := ui.NewTester(a.view, 1400, 900)
	r, ok := tt.Find("Avatar for 900000002")
	if !ok || r.W != 34 || r.H != 34 {
		t.Fatal("downloaded avatar was not displayed in the streamer row", r, ok)
	}
	if a.avatars[original] != bitmap || len(a.avatars) != 1 {
		t.Fatal("HTTPS upgrade changed the provider's avatar cache key")
	}
}

func TestAvatarDownloadURL(t *testing.T) {
	for _, address := range []string{
		"https://images.example.invalid/avatar.jpg?size=small",
		"http://images.example.invalid/avatar.jpg?size=small",
	} {
		got, err := avatarDownloadURL(address)
		if err != nil || got != "https://images.example.invalid/avatar.jpg?size=small" {
			t.Fatal("valid avatar URL was rejected or changed", got, err)
		}
	}
	for _, address := range []string{"", "file:///avatar.jpg", "https:///avatar.jpg", "https://user:password@images.example.invalid/avatar.jpg", "https://images.example.invalid/%zz"} {
		if _, err := avatarDownloadURL(address); err == nil {
			t.Fatal("invalid avatar URL was accepted", address)
		}
	}
}
