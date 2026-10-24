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

type failingTransport func(*http.Request) (*http.Response, error)

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCookieParsing(t *testing.T) {
	for _, s := range []string{"value", "uniqid=value", "Cookie: a=1; uniqid=value; b=2", `{"uniqid":"value"}`} {
		got, err := ParseCookie(s)
		if err != nil || got != "value" {
			t.Fatalf("%s: %q %v", s, got, err)
		}
	}
	if _, err := ParseCookie("bad\r\ncookie"); err == nil {
		t.Fatal("accepted header injection")
	}
}
func TestAuthenticatedClientAndGatedPresence(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprint(gated), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				cookie, _ := r.Cookie("uniqid")
				if cookie == nil || cookie.Value != "account" {
					t.Error("account missing")
				}
				if strings.Contains(r.URL.Path, "getUserInfoStudio") {
					fmt.Fprint(w, `{"code":0,"data":{"alive":1,"uid":123}}`)
					return
				}
				_ = r.ParseForm()
				if r.Form.Get("token") != "" {
					t.Error("unnecessary fingerprint token")
				}
				if gated {
					fmt.Fprint(w, `{"code":0,"data":{"alive":0,"needLogin":true}}`)
				} else {
					fmt.Fprint(w, `{"code":0,"data":{"alive":1,"uid":123,"hls_src":"https://cdn.example/live.m3u8"}}`)
				}
			}))
			defer srv.Close()
			c := NewClient()
			c.http = srv.Client()
			c.base = srv.URL
			c.Configure("account", time.Millisecond)
			s, err := c.Check(context.Background(), "alice")
			if s.Alive != 1 || s.UID != 123 {
				t.Fatalf("lost presence: %+v", s)
			}
			if gated && !errors.Is(err, ErrLoginRequired) {
				t.Fatal(err)
			}
			if !gated && (err != nil || s.URL() == "") {
				t.Fatalf("stream=%+v error=%v", s, err)
			}
			if calls != 1 && !gated {
				t.Fatal("unnecessary presence request")
			}
		})
	}
}
