// Package network routes BIGO traffic through an optional, explicit proxy.
package network

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is comparable so settings edits and connection changes can be detected.
// Credentials are kept separate from the address and never included in errors.
type Config struct {
	Enabled  bool   `json:"enabled,omitempty"`
	URL      string `json:"url,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (c Config) proxyURL() (*url.URL, error) {
	if !c.Enabled {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || u == nil || u.Hostname() == "" {
		return nil, errors.New("enter a proxy URL, such as socks5://127.0.0.1:1080 or http://127.0.0.1:8080")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("proxy URL must start with http://, https://, or socks5://")
	}
	if u.User != nil {
		return nil, errors.New("enter proxy credentials in the separate username and password fields")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("proxy URL must contain only a scheme, host, and port")
	}
	if u.Port() == "" {
		return nil, errors.New("include the port in the proxy URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("proxy port must be between 1 and 65535")
	}
	if c.Password != "" && c.Username == "" {
		return nil, errors.New("enter a proxy username when using a password")
	}
	if (u.Scheme == "http" || u.Scheme == "https") && strings.Contains(c.Username, ":") {
		return nil, errors.New("HTTP proxy usernames cannot contain a colon")
	}
	if strings.ContainsAny(c.Username+c.Password, "\r\n\x00") {
		return nil, errors.New("proxy credentials cannot contain line breaks or null characters")
	}
	if strings.HasPrefix(u.Scheme, "socks5") && (len(c.Username) > 255 || len(c.Password) > 255) {
		return nil, errors.New("SOCKS5 username and password must each fit in 255 bytes")
	}
	return u, nil
}

func (c Config) Validate() error {
	_, err := c.proxyURL()
	return err
}

// Route owns a reusable HTTP transport and immutable proxy settings. An absent
// proxy preserves the standard HTTP environment handling and direct native TCP.
type Route struct {
	config    Config
	proxy     *url.URL
	transport *http.Transport
}

func New(c Config) (*Route, error) {
	u, err := c.proxyURL()
	if err != nil {
		return nil, err
	}
	r := &Route{config: c, proxy: u}
	if u != nil {
		r.transport = http.DefaultTransport.(*http.Transport).Clone()
		// Tunnel HTTP as well as native TCP. NO_PROXY and environment proxies
		// must not split an explicitly configured BIGO route across exit IPs.
		r.transport.Proxy = nil
		r.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return r.dial(ctx, network, address, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
		}
	}
	return r, nil
}

// Key identifies a route without exposing its address or credentials.
func (r *Route) Key() string {
	if r == nil || r.proxy == nil {
		return ""
	}
	b, _ := json.Marshal(r.config)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (r *Route) HTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if r != nil && r.transport != nil {
		client.Transport = r.transport
	}
	return client
}

func (r *Route) CloseIdleConnections() {
	if r != nil && r.transport != nil {
		r.transport.CloseIdleConnections()
	}
}

type routeKey struct{}

func WithRoute(ctx context.Context, r *Route) context.Context {
	return context.WithValue(ctx, routeKey{}, r)
}

func FromContext(ctx context.Context) *Route {
	r, _ := ctx.Value(routeKey{}).(*Route)
	return r
}

// DialContext preserves the caller's native dial timeout, including negotiation.
// An explicit proxy never falls back to a direct connection after an error.
func DialContext(ctx context.Context, network, address string, dialer *net.Dialer) (net.Conn, error) {
	return FromContext(ctx).dial(ctx, network, address, dialer)
}

func (r *Route) dial(ctx context.Context, network, address string, dialer *net.Dialer) (net.Conn, error) {
	if r == nil || r.proxy == nil {
		return dialer.DialContext(ctx, network, address)
	}
	if dialer.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialer.Timeout)
		defer cancel()
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" && network != "udp" {
		return nil, errors.New("the configured proxy does not support this network type")
	}
	if network == "udp" && !strings.HasPrefix(r.proxy.Scheme, "socks5") {
		return nil, errors.New("HTTP proxies cannot forward multi-guest UDP audio; use SOCKS5 with UDP support or TUN mode")
	}
	conn, err := dialer.DialContext(ctx, "tcp", r.proxy.Host)
	if err != nil {
		return nil, fmt.Errorf("connect configured proxy: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	keep := false
	defer func() {
		stop()
		if !keep {
			conn.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var result net.Conn
	switch r.proxy.Scheme {
	case "http", "https":
		result, err = r.connectHTTP(ctx, conn, address)
	default:
		result, err = r.connectSOCKS(ctx, conn, network, address)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("proxy connection interrupted: %w", ctx.Err())
		}
		return nil, err
	}
	// Stop cancellation before returning a long-lived tunnel. The caller owns
	// its lifetime; the setup context may be canceled immediately after dialing.
	if !stop() || ctx.Err() != nil {
		result.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, context.Canceled
	}
	_ = conn.SetDeadline(time.Time{})
	keep = true
	return result, nil
}
