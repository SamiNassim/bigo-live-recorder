package bigo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessagingUsesAuthenticatedRealUserAndWebIdentity(t *testing.T) {
	for _, username := range []string{"account", "0"} {
		t.Run(username, func(t *testing.T) {
			testMessagingIdentity(t, username)
		})
	}
}

func testMessagingIdentity(t *testing.T, username string) {
	t.Helper()
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/studio/getWebSocketLink" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		cookie, err := r.Cookie("uniqid")
		if err != nil || cookie.Value != "web-account" {
			t.Error("web cookie missing")
		}
		if r.Header.Get("Origin") != Origin || r.Referer() != Origin+"/" || !strings.Contains(r.UserAgent(), "Chrome/") {
			t.Error("incoherent web request identity")
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("deviceId") != "web-device" {
			t.Error("device ID missing")
		}
		fmt.Fprintf(w, `{"code":0,"data":{"userId":"999","userName":"guest","uidToken":"Z3Vlc3Q=","wssLinkd":["127.0.0.1:1299"],"realUser":{"userId":"123456789","userName":%q,"uidToken":"dGlja2V0###VER2","deviceId":"web-device"}}}`, username)
	}))
	defer srv.Close()
	c := NewClient()
	c.http, c.base = srv.Client(), srv.URL
	c.Configure("web-account", time.Millisecond)
	got, err := c.Messaging(context.Background(), "web-device")
	if err != nil || got.UID != 123456789 || got.Username != username || string(got.Cookie) != "ticket" || got.DeviceID != "web-device" || len(got.Addresses) != 1 {
		t.Fatalf("unexpected messaging identity or error: uid=%d user=%s error=%v", got.UID, got.Username, err)
	}
}

func TestMessagingRejectsGuestExpiredAndInvalidTickets(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, payload string
		loginRequired bool
	}{
		{"guest", `{"code":0,"data":{"userId":123,"userName":"guest","uidToken":"dGlja2V0"}}`, true},
		{"top-level-account", `{"code":0,"data":{"userId":123,"userName":"account","uidToken":"dGlja2V0","deviceId":"web"}}`, true},
		{"expired", `{"code":700001}`, true},
		{"empty-user", `{"code":0,"data":{"realUser":{"userId":123,"userName":""}}}`, true},
		{"zero-user-missing-ticket", `{"code":0,"data":{"realUser":{"userId":123,"userName":"0","deviceId":"web"}}}`, false},
		{"zero-uid", `{"code":0,"data":{"realUser":{"userId":0,"userName":"0","uidToken":"dGlja2V0","deviceId":"web"}}}`, false},
		{"invalid-uid", `{"code":0,"data":{"realUser":{"userId":"bad","userName":"account","uidToken":"dGlja2V0","deviceId":"web"}}}`, false},
		{"invalid-ticket", `{"code":0,"data":{"realUser":{"userId":123,"userName":"account","uidToken":"***","deviceId":"web"}}}`, false},
		{"empty-ticket", `{"code":0,"data":{"realUser":{"userId":123,"userName":"account","uidToken":"","deviceId":"web"}}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.payload) }))
			defer srv.Close()
			c := NewClient()
			c.http, c.base = srv.Client(), srv.URL
			c.Configure("cookie", time.Millisecond)
			if _, err := c.Messaging(context.Background(), "web"); err == nil || (test.loginRequired && !errors.Is(err, ErrLoginRequired)) {
				t.Fatalf("invalid credentials accepted or wrong error: %v", err)
			}
		})
	}
}
