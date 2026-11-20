package bigolistener

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bigo-live-recorder/internal/network"
)

func TestNativeNotificationDialUsesProxy(t *testing.T) {
	requests := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.Host
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	route, _ := network.New(network.Config{Enabled: true, URL: proxy.URL})
	defer route.CloseIdleConnections()
	_, err := dialOneLinkd(network.WithRoute(t.Context(), route), "notifications.example:1234", validTestSession())
	if err == nil || !strings.Contains(err.Error(), "proxy authentication failed") {
		t.Fatal("native connection ignored proxy failure", err)
	}
	if got := <-requests; got != "CONNECT notifications.example:1234" {
		t.Fatal(got)
	}
}

func TestProxyChangeCancelsBootstrapAndPinsNewRoute(t *testing.T) {
	first, _ := network.New(network.Config{Enabled: true, URL: "socks5://127.0.0.1:1080", Password: "synthetic", Username: "synthetic"})
	second, _ := network.New(network.Config{Enabled: true, URL: "http://127.0.0.1:8080"})
	entered, stopped := make(chan *network.Route, 2), make(chan struct{}, 2)
	h := NewHub(context.Background(), func(ctx context.Context, _ string) (Session, error) {
		entered <- network.FromContext(ctx)
		<-ctx.Done()
		stopped <- struct{}{}
		return Session{}, ctx.Err()
	}, nil, nil, nil)
	defer h.Close()
	h.Configure(true, "synthetic-cookie", "synthetic-device", first)
	_, off, err := h.Subscribe("synthetic-target", 123)
	if err != nil {
		t.Fatal(err)
	}
	defer off()
	waitRoute := func(want *network.Route) {
		t.Helper()
		select {
		case got := <-entered:
			if got != want {
				t.Fatal("bootstrap used a different route")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("bootstrap did not start")
		}
	}
	waitRoute(first)
	h.Configure(true, "synthetic-cookie", "synthetic-device", second)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("proxy change did not interrupt the old bootstrap")
	}
	waitRoute(second)
	snapshot, ready := h.snapshot()
	if !ready || snapshot.route != second || strings.Contains(snapshot.configKey, "synthetic") {
		t.Fatal("route snapshot missing or credentials exposed")
	}
}
