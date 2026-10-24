package app

import (
	"bytes"
	"context"
	"image"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/egoist/mygo/ui"
)

// Cache public thumbnails only; no playback cookie is sent to image CDNs.
func (a *application) loadAvatars() {
	for _, row := range a.snapshot.Rows {
		a.loadAvatar(row.Streamer.Avatar)
	}
	if a.account != nil {
		a.loadAvatar(a.account.Avatar)
	}
}
func (a *application) loadAvatar(address string) {
	if address == "" || a.avatars[address] != nil || a.avatarLoading[address] || len(a.avatarLoading) >= 256 {
		return
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return
	}
	a.avatarLoading[address] = true
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
		if err != nil || len(data) > 2<<20 {
			return
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width > 1024 || cfg.Height > 1024 {
			return
		}
		bitmap, err := ui.DecodeBitmap(data)
		if err != nil {
			return
		}
		a.update(func() { a.avatars[address] = bitmap })
	}()
}
