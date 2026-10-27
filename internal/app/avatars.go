package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/egoist/mygo/ui"
)

const maxCachedAvatars = 256
const maxAvatarDownloads = 4

var avatarClient = &http.Client{Timeout: 10 * time.Second}

// avatar requests only thumbnails actually displayed by a virtualized row or
// the verified account badge. Preview views never make network requests.
func (a *application) avatar(c *ui.Context, address string) *ui.Bitmap {
	if bitmap := a.avatars[address]; bitmap != nil {
		a.avatarClock++
		a.avatarUsed[address] = a.avatarClock
		return bitmap
	}
	if a.preview || address == "" {
		return nil
	}
	if retry := a.avatarRetry[address]; time.Now().Before(retry) {
		c.After(time.Until(retry))
		return nil
	}
	a.loadAvatar(address)
	return nil
}

func (a *application) avatarNeeded(address string) bool {
	if a.account != nil && a.account.Avatar == address {
		return true
	}
	for _, row := range a.snapshot.Rows {
		if row.Streamer.Avatar == address {
			return true
		}
	}
	return false
}

// Drop thumbnails for removed streamers and replaced avatar URLs.
func (a *application) pruneAvatars() {
	for address := range a.avatars {
		if !a.avatarNeeded(address) {
			delete(a.avatars, address)
			delete(a.avatarUsed, address)
		}
	}
	for address := range a.avatarRetry {
		if !a.avatarNeeded(address) {
			delete(a.avatarRetry, address)
		}
	}
}

func (a *application) keepAvatar(address string, bitmap *ui.Bitmap) {
	if !a.avatarNeeded(address) {
		return // A removed account's in-flight download must not restore its image.
	}
	for len(a.avatars) >= maxCachedAvatars {
		oldest := ""
		for candidate := range a.avatars {
			if a.account != nil && candidate == a.account.Avatar {
				continue
			}
			if oldest == "" || a.avatarUsed[candidate] < a.avatarUsed[oldest] {
				oldest = candidate
			}
		}
		if oldest == "" {
			return
		}
		delete(a.avatars, oldest)
		delete(a.avatarUsed, oldest)
	}
	a.avatarClock++
	a.avatars[address] = bitmap
	a.avatarUsed[address] = a.avatarClock
}

func (a *application) loadAvatar(address string) {
	if a.avatars[address] != nil || a.avatarLoading[address] || len(a.avatarLoading) >= maxAvatarDownloads {
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
		bitmap, err := fetchAvatar(ctx, avatarClient, address)
		if a.ctx.Err() != nil {
			return
		}
		a.update(func() {
			delete(a.avatarLoading, address)
			if !a.avatarNeeded(address) {
				return
			}
			if err != nil {
				if len(a.avatarRetry) >= maxCachedAvatars {
					for candidate := range a.avatarRetry {
						delete(a.avatarRetry, candidate)
						break
					}
				}
				a.avatarRetry[address] = time.Now().Add(time.Minute)
				return
			}
			delete(a.avatarRetry, address)
			a.keepAvatar(address, bitmap)
		})
	}()
}

// No playback cookies or API headers are sent to public image CDNs.
func fetchAvatar(ctx context.Context, client *http.Client, address string) (*ui.Bitmap, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("avatar download failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, errors.New("avatar exceeds the download limit")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width > 1024 || cfg.Height > 1024 {
		return nil, errors.New("avatar dimensions exceed the limit")
	}
	return decodeThumbnail(data, avatarPixels)
}
