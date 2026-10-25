package bigo

import (
	"context"
	"encoding/json"
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

func TestFlexibleIdentifiers(t *testing.T) {
	for _, v := range []string{`{"uid":123,"roomId":456}`, `{"uid":"123","roomId":"456"}`} {
		var s Stream
		if err := json.Unmarshal([]byte(v), &s); err != nil || s.UID != 123 || s.RoomID != "456" {
			t.Fatalf("%+v %v", s, err)
		}
	}
}

func TestAccountIncludesAvatarInOneRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/usercenter/getUserInfo" {
			t.Errorf("unexpected account request: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"code":0,"data":{"bigo_id":"900000001","nick_name":"Demo Viewer","data5":"https://cdn.example/avatar.png"}}`)
	}))
	defer srv.Close()
	c := NewClient()
	c.http = srv.Client()
	c.base = srv.URL
	c.Configure("synthetic-cookie", time.Millisecond)
	account, err := c.Account(context.Background())
	if err != nil || account.ID != "900000001" || account.Name != "Demo Viewer" || account.Avatar != "https://cdn.example/avatar.png" {
		t.Fatalf("account response: %+v, %v", account, err)
	}
	if calls != 1 {
		t.Fatalf("account information required %d requests", calls)
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
