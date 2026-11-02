// Package bigo implements authenticated BIGO HTTP playback and HLS transport.
package bigo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"bigo-live-recorder/internal/fault"
)

const Origin = "https://www.bigo.tv"
const apiBase = "https://ta.bigo.tv/official_website"

var ErrLoginRequired = fault.Action("BIGO could not authenticate the saved account; check BIGO Account in Settings", nil)
var ErrURLWithheld = errors.New("streamer is live but BIGO supplied no playback URL")

type Account struct {
	ID     string `json:"bigo_id"`
	Name   string `json:"nick_name"`
	Avatar string `json:"data5"`
}
type Stream struct {
	native     *pJoinChannelRes
	Alive      int      `json:"alive"`
	UID        uint32   `json:"uid"`
	Name       string   `json:"nick_name"`
	Avatar     string   `json:"avatar"`
	RoomID     string   `json:"roomId"`
	RoomStatus int      `json:"roomStatus"`
	RoomKind   RoomKind `json:"-"`
	HLS        string   `json:"hls_src"`
	CDN        []string `json:"cdn_src"`
	NeedLogin  bool     `json:"needLogin"`
	Quality    string   `json:"quality,omitempty"`
}

// BIGO uses both JSON strings and numbers for provider identifiers.
func (s *Stream) UnmarshalJSON(b []byte) error {
	type plain Stream
	var fields struct {
		*plain
		UID      json.RawMessage `json:"uid"`
		RoomID   json.RawMessage `json:"roomId"`
		Nickname string          `json:"nickname"`
		Image    string          `json:"ogImage"`
		AppURL   string          `json:"aliOSUrl"`
	}
	fields.plain = (*plain)(s)
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	text := func(v json.RawMessage) string {
		if len(v) == 0 || string(v) == "null" {
			return ""
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		return string(v)
	}
	if v := text(fields.UID); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid BIGO uid: %w", err)
		}
		s.UID = uint32(n)
	}
	s.RoomID = text(fields.RoomID)
	if s.Name == "" {
		s.Name = fields.Nickname
	}
	if s.Avatar == "" {
		s.Avatar = fields.Image
	}
	if s.Alive == 1 && (s.RoomID == "" || s.RoomID == "0") {
		if room := roomFromAppLink(fields.AppURL, s.UID); room != "" {
			s.RoomID = room
		}
	}
	return nil
}

// Presence includes the native room in an app link even when web playback is
// unavailable. Accept it only when its target matches the response's account.
func roomFromAppLink(address string, uid uint32) string {
	u, err := url.Parse(address)
	if err != nil || uid == 0 || u.Scheme != "bigolive" || u.Host != "livevideoshow" || u.User != nil {
		return ""
	}
	query := u.Query()
	target, err := strconv.ParseUint(query.Get("uid"), 10, 32)
	if err != nil || uint32(target) != uid {
		return ""
	}
	room, err := strconv.ParseUint(query.Get("roomid"), 10, 64)
	if err != nil || uint32(room) == 0 {
		return ""
	}
	return strconv.FormatUint(room, 10)
}

func (s Stream) URL() string {
	urls := s.URLs()
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

// Playable includes native media tickets, which need no web playback URL.
func (s Stream) Playable() bool { return s.native != nil || s.URL() != "" }

// URLs returns the preferred playback URL followed by distinct CDN alternatives.
func (s Stream) URLs() []string {
	var result []string
	seen := make(map[string]bool)
	for _, v := range append([]string{s.HLS}, s.CDN...) {
		u, err := url.Parse(strings.TrimSpace(v))
		if err == nil && u.Host != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http") && !seen[u.String()] {
			result = append(result, u.String())
			seen[u.String()] = true
			if len(result) == 8 {
				break
			}
		}
	}
	return result
}

type HTTPError struct {
	Status     int
	RetryAfter time.Duration
	Endpoint   string
}

func (e *HTTPError) Error() string {
	message := fmt.Sprintf("BIGO refused the request (HTTP %d %s)", e.Status, http.StatusText(e.Status))
	if e.Status == http.StatusTooManyRequests {
		message = "BIGO is limiting requests (HTTP 429)"
	} else if e.Status >= 500 {
		message = fmt.Sprintf("BIGO's service is temporarily unavailable (HTTP %d %s)", e.Status, http.StatusText(e.Status))
	}
	if e.RetryAfter > 0 {
		message += fmt.Sprintf("; requests paused for at least %s", e.RetryAfter.Round(time.Second))
	}
	return message
}

func (e *HTTPError) DiagnosticDetails() string {
	return fmt.Sprintf("BIGO endpoint %s: %s", e.Endpoint, e.Error())
}

type Client struct {
	http   *http.Client
	base   string
	mu     sync.Mutex
	cookie string
	delay  time.Duration
	next   time.Time
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 20 * time.Second}, base: apiBase, delay: 2 * time.Second}
}
func ParseCookie(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.HasPrefix(raw, "{") {
		var v struct {
			Uniqid string `json:"uniqid"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return "", errors.New("paste the raw uniqid value, a Cookie header, or JSON with a uniqid field")
		}
		raw = v.Uniqid
	}
	if strings.HasPrefix(strings.ToLower(raw), "cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}
	if strings.Contains(raw, "uniqid=") {
		for _, p := range strings.Split(raw, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && k == "uniqid" {
				raw = v
				break
			}
		}
	}
	if raw == "" || len(raw) > 16000 || strings.ContainsAny(raw, ";\r\n\t ") {
		return "", errors.New("invalid uniqid value")
	}
	return raw, nil
}
func (c *Client) Configure(cookie string, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cookie = cookie
	c.delay = max(delay, time.Millisecond)
}
func (c *Client) wait(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c.mu.Lock()
		d := time.Until(c.next)
		if d <= 0 {
			c.next = time.Now().Add(c.delay)
			cookie := c.cookie
			c.mu.Unlock()
			return cookie, nil
		}
		// Never hold the configuration lock during a rate-limit wait. A long
		// Retry-After must not block settings or another caller's cancellation.
		c.mu.Unlock()
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
}
func (c *Client) Account(ctx context.Context) (Account, error) {
	var p struct {
		Code int     `json:"code"`
		Data Account `json:"data"`
	}
	err := c.request(ctx, http.MethodGet, "/usercenter/getUserInfo", nil, &p)
	if err != nil {
		return Account{}, err
	}
	if p.Code != 0 || p.Data.ID == "" {
		return Account{}, ErrLoginRequired
	}
	return p.Data, nil
}
func (c *Client) Presence(ctx context.Context, id string) (Stream, error) {
	var p struct {
		Code int    `json:"code"`
		Data Stream `json:"data"`
	}
	err := c.request(ctx, http.MethodGet, "/OUserCenter/getUserInfoStudio?bigoId="+url.QueryEscape(id), nil, &p)
	if err != nil {
		return p.Data, err
	}
	if p.Code != 0 {
		return p.Data, fmt.Errorf("BIGO presence rejected: code=%d", p.Code)
	}
	return p.Data, nil
}
func (c *Client) Check(ctx context.Context, id string) (Stream, error) {
	var p struct {
		Code int    `json:"code"`
		Data Stream `json:"data"`
	}
	form := url.Values{"siteId": {id}, "verify": {""}, "token": {""}, "isEncrypt": {"0"}, "supportHevc": {"0"}}
	if err := c.request(ctx, http.MethodPost, "/studio/getInternalStudioInfo", form, &p); err != nil {
		return p.Data, err
	}
	if p.Code != 0 {
		return p.Data, fmt.Errorf("BIGO studio rejected: code=%d", p.Code)
	}
	if p.Data.NeedLogin {
		if presence, err := c.Presence(ctx, id); err == nil {
			p.Data.Alive = presence.Alive
			p.Data.UID = presence.UID
		}
		return p.Data, ErrLoginRequired
	}
	if p.Data.Alive == 1 && p.Data.URL() == "" {
		return p.Data, ErrURLWithheld
	}
	return p.Data, nil
}
func (c *Client) request(ctx context.Context, method, path string, form url.Values, target any) error {
	cookie, err := c.wait(ctx)
	if err != nil {
		message := "waiting for a turn in the BIGO request queue was interrupted"
		if errors.Is(err, context.DeadlineExceeded) {
			message = "the BIGO request queue wait timed out before a request could be sent"
		}
		return fault.Wrap(message, err)
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	setRequestHeaders(r, Origin+"/"+url.PathEscape(form.Get("siteId")))
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "uniqid", Value: cookie})
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		var u *url.Error
		if errors.As(err, &u) {
			err = u.Err
		}
		return fault.Wrap("BIGO did not answer the request: "+fault.Reason(err), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var delay time.Duration
		if resp.StatusCode == 429 || resp.StatusCode == 403 {
			delay = retryDelay(resp.Header.Get("Retry-After"))
			c.mu.Lock()
			c.next = maxTime(c.next, time.Now().Add(max(delay, 30*time.Second)))
			delay = time.Until(c.next)
			c.mu.Unlock()
		}
		return &HTTPError{resp.StatusCode, delay, strings.Split(path, "?")[0]}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(target); err != nil {
		return fault.Wrap("BIGO returned an incomplete or unreadable response", err)
	}
	return nil
}
func retryDelay(v string) time.Duration {
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v + "s"); err == nil {
		return max(d, 0)
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
func ValidID(id string) bool {
	return id != "" && len(id) <= 64 && !strings.ContainsAny(id, " /:?#%\\\r\n\t@") && id != "." && id != ".."
}

// ParseID accepts an ID/alias or a public BIGO profile URL, never an arbitrary
// host. Only the canonical identifier is persisted and used in API requests.
func ParseID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if strings.Contains(id, "://") {
		u, err := url.Parse(id)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" ||
			(!strings.EqualFold(u.Hostname(), "www.bigo.tv") && !strings.EqualFold(u.Hostname(), "bigo.tv")) {
			return "", errors.New("enter a BIGO ID or a bigo.tv profile URL")
		}
		id = strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	}
	if !ValidID(id) {
		return "", errors.New("enter a BIGO ID or a bigo.tv profile URL")
	}
	return id, nil
}
func setRequestHeaders(r *http.Request, referer string) {
	platform, token := "Linux", "X11; Linux x86_64"
	switch runtime.GOOS {
	case "darwin":
		platform, token = "macOS", "Macintosh; Intel Mac OS X 10_15_7"
	case "windows":
		platform, token = "Windows", "Windows NT 10.0; Win64; x64"
	}
	r.Header.Set("User-Agent", "Mozilla/5.0 ("+token+") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36")
	r.Header.Set("Origin", Origin)
	r.Header.Set("Referer", referer)
	r.Header.Set("Accept", "application/json, text/plain, */*")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.Header.Set("Sec-CH-UA", `"Google Chrome";v="155", "Chromium";v="155", "Not(A:Brand";v="24"`)
	r.Header.Set("Sec-CH-UA-Mobile", "?0")
	r.Header.Set("Sec-CH-UA-Platform", `"`+platform+`"`)
	r.Header.Set("Sec-Fetch-Dest", "empty")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Sec-Fetch-Site", "same-site")
}
