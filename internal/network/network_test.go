package network

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func proxyFixture(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				serve(conn)
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); wg.Wait() })
	return listener.Addr().String()
}

func testRoute(t *testing.T, cfg Config) *Route {
	t.Helper()
	route, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(route.CloseIdleConnections)
	return route
}

func TestProxyValidationAndDefaultBehavior(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1:8080", "https://proxy.example:443", "socks5://[::1]:1080", "socks5h://proxy.example:1080"} {
		if err := (Config{Enabled: true, URL: address}).Validate(); err != nil {
			t.Fatal(address, err)
		}
	}
	for _, address := range []string{"", "127.0.0.1:1080", "ftp://proxy.example:21", "http://proxy.example", "http://proxy.example:0", "http://proxy.example:65536", "http://proxy.example:bad", "http://private-user:private-password@proxy.example:8080", "http://proxy.example:8080/path", "http://proxy.example:8080?token=private-password", "http://%private-password"} {
		err := (Config{Enabled: true, URL: address}).Validate()
		if err == nil || strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), "private-user") {
			t.Fatalf("invalid proxy was accepted or exposed credentials: %v", err)
		}
	}
	if err := (Config{Enabled: true, URL: "socks5://proxy.example:1080", Username: strings.Repeat("x", 256)}).Validate(); err == nil {
		t.Fatal("oversized SOCKS5 credentials accepted")
	}
	if err := (Config{Enabled: true, URL: "http://proxy.example:8080", Password: "synthetic"}).Validate(); err == nil {
		t.Fatal("password without username accepted")
	}
	route := testRoute(t, Config{URL: "invalid but disabled"})
	if route.Key() != "" || route.HTTPClient(time.Second).Transport != nil {
		t.Fatal("disabled proxy changed default HTTP transport behavior")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "direct") }))
	defer server.Close()
	response, err := route.HTTPClient(time.Second).Get(server.URL)
	if err != nil {
		t.Fatal("default connection no longer works", err)
	}
	response.Body.Close()
	conn, err := DialContext(WithRoute(t.Context(), route), "tcp", server.Listener.Addr().String(), &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatal("direct native connection no longer works", err)
	}
	conn.Close()
}

func TestHTTPTunnelKeepsBufferedBytesAndCredentialsPrivate(t *testing.T) {
	seen := make(chan *http.Request, 1)
	address := proxyFixture(t, func(conn net.Conn) {
		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			t.Error(err)
			return
		}
		seen <- request
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\nready")
		io.Copy(conn, conn)
	})
	route := testRoute(t, Config{Enabled: true, URL: "http://" + address, Username: "synthetic-user", Password: "synthetic-password"})
	ctx, cancel := context.WithCancel(WithRoute(t.Context(), route))
	conn, err := DialContext(ctx, "tcp", "native.example:1234", &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Setup cancellation must not close an established native connection.
	cancel()
	conn.SetDeadline(time.Now().Add(time.Second))
	ready := make([]byte, 5)
	if _, err := io.ReadFull(conn, ready); err != nil || string(ready) != "ready" {
		t.Fatal("buffered server frame lost", string(ready), err)
	}
	if _, err := conn.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 7)
	if _, err := io.ReadFull(conn, payload); err != nil || string(payload) != "payload" {
		t.Fatal("tunnel ended with its setup context", err)
	}
	request := <-seen
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-user:synthetic-password"))
	if request.Method != "CONNECT" || request.Host != "native.example:1234" || request.Header.Get("Proxy-Authorization") != wantAuth {
		t.Fatal("incorrect CONNECT destination or authentication")
	}
}

func TestHTTPRequestsUseExplicitTunnelDespiteNoProxy(t *testing.T) {
	t.Setenv("NO_PROXY", "*")
	var calls atomic.Int32
	address := proxyFixture(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil || request.Method != "CONNECT" || request.Host != "api.example:80" {
			t.Errorf("wrong tunnel: %v", err)
			return
		}
		calls.Add(1)
		io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		request, err = http.ReadRequest(reader)
		if err != nil || request.Host != "api.example" || request.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("origin request missing or proxy credentials forwarded: %v", err)
			return
		}
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	})
	route := testRoute(t, Config{Enabled: true, URL: "http://" + address, Username: "synthetic", Password: "synthetic"})
	response, err := route.HTTPClient(time.Second).Get("http://api.example/account")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "ok" || calls.Load() != 1 {
		t.Fatal("request bypassed the configured proxy", string(body), err)
	}
}

func TestHTTPSOriginUsesTLSInsideProxyTunnel(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("origin TLS or credential isolation was lost")
		}
		fmt.Fprint(w, "authenticated")
	}))
	defer origin.Close()
	address := proxyFixture(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil || request.Method != "CONNECT" || request.Host != origin.Listener.Addr().String() {
			t.Errorf("incorrect HTTPS tunnel: %v", err)
			return
		}
		upstream, err := net.DialTimeout("tcp", origin.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		go func() { io.Copy(upstream, reader); upstream.Close() }()
		io.Copy(conn, upstream)
	})
	route := testRoute(t, Config{Enabled: true, URL: "http://" + address, Username: "synthetic-user", Password: "synthetic-password"})
	// Trust only this synthetic origin certificate. Proxy TLS, when selected,
	// still uses the operating system's normal certificate verification.
	route.transport.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	response, err := route.HTTPClient(time.Second).Get(origin.URL + "/account")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "authenticated" {
		t.Fatal(string(body), err)
	}
}

func TestProxyFailureNeverFallsBackToDirect(t *testing.T) {
	var direct atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { direct.Add(1) }))
	defer origin.Close()
	address := proxyFixture(t, func(conn net.Conn) {
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		io.WriteString(conn, "HTTP/1.1 407 synthetic-password\r\nContent-Length: 0\r\n\r\n")
	})
	route := testRoute(t, Config{Enabled: true, URL: "http://" + address})
	_, err := route.HTTPClient(time.Second).Get(origin.URL)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "synthetic-password") || direct.Load() != 0 {
		t.Fatal("proxy failure leaked a response or fell back to direct", err, direct.Load())
	}
	_, err = DialContext(WithRoute(t.Context(), route), "udp", "127.0.0.1:1234", &net.Dialer{Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "SOCKS5") {
		t.Fatal("HTTP proxy silently bypassed UDP forwarding", err)
	}
}

func TestProxyNegotiationHonorsCancellationAndTimeout(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			connected := make(chan struct{}, 1)
			address := proxyFixture(t, func(conn net.Conn) { connected <- struct{}{}; io.Copy(io.Discard, conn) })
			route := testRoute(t, Config{Enabled: true, URL: scheme + "://" + address})
			ctx, cancel := context.WithCancel(WithRoute(t.Context(), route))
			result := make(chan error, 1)
			go func() {
				_, err := DialContext(ctx, "tcp", "native.example:1234", &net.Dialer{Timeout: time.Second})
				result <- err
			}()
			<-connected
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt proxy negotiation")
			}
			_, err := DialContext(WithRoute(t.Context(), route), "tcp", "native.example:1234", &net.Dialer{Timeout: 30 * time.Millisecond})
			var timeout net.Error
			if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout()) {
				t.Fatal("dial timeout did not cover proxy negotiation", err)
			}
		})
	}
}

func socksGreeting(t *testing.T, conn net.Conn, authenticated bool) bool {
	t.Helper()
	var greeting [3]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		t.Error(err)
		return false
	}
	method := byte(0)
	if authenticated {
		method = 2
	}
	if greeting != [3]byte{5, 1, method} {
		t.Error("wrong SOCKS5 greeting")
		return false
	}
	conn.Write([]byte{5, method})
	if authenticated {
		var prefix [2]byte
		io.ReadFull(conn, prefix[:])
		user := make([]byte, int(prefix[1]))
		io.ReadFull(conn, user)
		var length [1]byte
		io.ReadFull(conn, length[:])
		password := make([]byte, int(length[0]))
		io.ReadFull(conn, password)
		if prefix[0] != 1 || string(user) != "synthetic-user" || string(password) != "synthetic-password" {
			t.Error("wrong SOCKS5 credentials")
			return false
		}
		conn.Write([]byte{1, 0})
	}
	return true
}

func TestSOCKS5UsesRemoteDNSAndOptionalAuthentication(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprint(authenticated), func(t *testing.T) {
			address := proxyFixture(t, func(conn net.Conn) {
				if !socksGreeting(t, conn, authenticated) {
					return
				}
				var head [3]byte
				io.ReadFull(conn, head[:])
				destination, err := readAddress(conn)
				if err != nil || head != [3]byte{5, 1, 0} || destination != "unresolvable.example:1234" {
					t.Errorf("wrong SOCKS5 destination %q: %v", destination, err)
					return
				}
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
				io.Copy(conn, conn)
			})
			cfg := Config{Enabled: true, URL: "socks5://" + address}
			if authenticated {
				cfg.Username, cfg.Password = "synthetic-user", "synthetic-password"
			}
			route := testRoute(t, cfg)
			conn, err := DialContext(WithRoute(t.Context(), route), "tcp", "unresolvable.example:1234", &net.Dialer{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			conn.Write([]byte("native-frame"))
			got := make([]byte, 12)
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "native-frame" {
				t.Fatal(string(got), err)
			}
		})
	}
}

func TestSOCKS5UDPAssociationAndControlLifetime(t *testing.T) {
	relay, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	control := make(chan net.Conn, 1)
	address := proxyFixture(t, func(conn net.Conn) {
		if !socksGreeting(t, conn, false) {
			return
		}
		var head [3]byte
		io.ReadFull(conn, head[:])
		destination, err := readAddress(conn)
		if err != nil || head != [3]byte{5, 3, 0} || destination != "0.0.0.0:0" {
			t.Errorf("invalid UDP association: %v", err)
			return
		}
		_, port, _ := net.SplitHostPort(relay.LocalAddr().String())
		bound, _ := encodeAddress("0.0.0.0:" + port)
		conn.Write(append([]byte{5, 0, 0}, bound...))
		control <- conn
		io.Copy(io.Discard, conn)
	})
	route := testRoute(t, Config{Enabled: true, URL: "socks5://" + address})
	conn, err := DialContext(WithRoute(t.Context(), route), "udp", "192.0.2.10:2345", &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if n, err := conn.Write([]byte("audio")); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	relay.SetReadDeadline(time.Now().Add(time.Second))
	packet := make([]byte, 512)
	n, peer, err := relay.ReadFrom(packet)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := encodeAddress("192.0.2.10:2345")
	want := append(append([]byte{0, 0, 0}, target...), []byte("audio")...)
	if !bytes.Equal(packet[:n], want) {
		t.Fatal("UDP payload or destination was changed")
	}
	relay.WriteTo(packet[:n], peer)
	payload := make([]byte, 512)
	if n, err := conn.Read(payload); err != nil || string(payload[:n]) != "audio" {
		t.Fatal("UDP framing leaked into audio", err)
	}
	(<-control).Close()
	if _, err := conn.Read(payload); err == nil {
		t.Fatal("closing SOCKS5 control did not close UDP association")
	}
}

func TestSOCKS5RejectionDoesNotExposeCredentials(t *testing.T) {
	address := proxyFixture(t, func(conn net.Conn) {
		var greeting [3]byte
		io.ReadFull(conn, greeting[:])
		conn.Write([]byte{5, 255})
	})
	route := testRoute(t, Config{Enabled: true, URL: "socks5://" + address, Username: "synthetic-user", Password: "synthetic-password"})
	_, err := DialContext(WithRoute(t.Context(), route), "tcp", "native.example:1234", &net.Dialer{Timeout: time.Second})
	if err == nil || strings.Contains(err.Error(), "synthetic-") {
		t.Fatal("authentication failed without a safe error", err)
	}
}
