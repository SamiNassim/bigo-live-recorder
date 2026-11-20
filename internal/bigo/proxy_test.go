package bigo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bigo-live-recorder/internal/network"
)

func TestAccountRequestsHonorConfiguredAndPinnedRoutes(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "CONNECT" || r.Host != "account.example:80" {
					t.Error("account bypassed the proxy tunnel")
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				rw.WriteString("HTTP/1.1 200 OK\r\n\r\n")
				rw.Flush()
				request, err := http.ReadRequest(rw.Reader)
				if err != nil {
					t.Error(err)
					return
				}
				cookie, err := request.Cookie("uniqid")
				if err != nil || cookie.Value != "synthetic-cookie" || request.Header.Get("Proxy-Authorization") != "" {
					t.Error("account cookie missing or proxy authentication forwarded")
					return
				}
				body := `{"code":0,"data":{"bigo_id":"900000001","nick_name":"Demo Viewer"}}`
				fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
				rw.Flush()
			}))
			defer proxy.Close()
			route, err := network.New(network.Config{Enabled: true, URL: proxy.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer route.CloseIdleConnections()
			client := NewClient()
			client.base = "http://account.example"
			client.Configure("synthetic-cookie", time.Millisecond)
			ctx := t.Context()
			if pinned {
				// Even if general requests have been reconfigured, an in-flight
				// listener bootstrap must use its captured authentication route.
				ctx = network.WithRoute(ctx, route)
				client.SetRoute(nil)
			} else {
				client.SetRoute(route)
			}
			account, err := client.Account(ctx)
			if err != nil || account.ID != "900000001" {
				t.Fatal("account failed through proxy", account, err)
			}
		})
	}
}

func TestNativeMediaUsesConfiguredProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "192.0.2.10:2345" {
			t.Error("incorrect native media tunnel")
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		rw.WriteString("HTTP/1.1 200 OK\r\n\r\nsynthetic-frame")
		rw.Flush()
		io.Copy(io.Discard, conn)
	}))
	defer proxy.Close()
	route, _ := network.New(network.Config{Enabled: true, URL: proxy.URL})
	defer route.CloseIdleConnections()
	conn, err := dialNativeMedia(network.WithRoute(t.Context(), route), []mediaProxy{{ip: 0x0a0200c0, tcp: []uint16{2345}}}, 123, 42, true)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	packet := make([]byte, len("synthetic-frame"))
	if _, err := io.ReadFull(conn, packet); err != nil || string(packet) != "synthetic-frame" {
		t.Fatal(string(packet), err)
	}
}

func TestRouteReplacementIsSafeDuringRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"bigo_id":"900000001"}}`)
	}))
	defer server.Close()
	client := NewClient()
	client.base = server.URL
	client.Configure("synthetic", time.Nanosecond)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 10 {
			route, _ := network.New(network.Config{})
			client.SetRoute(route)
		}
	})
	for range 10 {
		if _, err := client.Account(t.Context()); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
}

func TestGuestAudioDoesNotBypassHTTPProxy(t *testing.T) {
	route, _ := network.New(network.Config{Enabled: true, URL: "http://127.0.0.1:1"})
	_, err := dialGuestAudio(network.WithRoute(context.Background(), route), []mediaProxy{{ip: 0x0100007f, udp: []uint16{2345}}}, 123, 42, 0)
	if err == nil || !strings.Contains(err.Error(), "SOCKS5") {
		t.Fatal("guest UDP bypassed the configured HTTP proxy", err)
	}
}
