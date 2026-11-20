package network

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

func (r *Route) connectHTTP(ctx context.Context, conn net.Conn, address string) (net.Conn, error) {
	if r.proxy.Scheme == "https" {
		secure := tls.Client(conn, &tls.Config{ServerName: r.proxy.Hostname(), MinVersion: tls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("establish TLS to configured proxy: %w", err)
		}
		conn = secure
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if r.config.Username != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(r.config.Username+":"+r.config.Password)))
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("send proxy CONNECT request: %w", err)
	}
	reader := bufio.NewReader(io.LimitReader(conn, 64<<10))
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		// Do not echo a proxy's response; it can contain credentials or an
		// unbounded, server-controlled status message.
		return nil, fmt.Errorf("read proxy CONNECT response: %w", safeHTTPError(err))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusProxyAuthRequired {
			return nil, fmt.Errorf("proxy authentication failed (HTTP %d); check the proxy username and password", response.StatusCode)
		}
		return nil, fmt.Errorf("proxy refused the TCP tunnel (HTTP %d); check whether the destination port is allowed", response.StatusCode)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func safeHTTPError(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return err
	}
	if e, ok := err.(net.Error); ok {
		return e
	}
	return errors.New("invalid or oversized HTTP response")
}

// Preserve tunnel bytes read along with the response headers, then bypass the
// header-size limit. Otherwise early server frames can be silently discarded.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader.Buffered() > 0 {
		return c.reader.Read(p[:min(len(p), c.reader.Buffered())])
	}
	return c.Conn.Read(p)
}
