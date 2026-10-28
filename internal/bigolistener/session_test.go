package bigolistener

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func validTestSession() Session {
	return Session{UID: 123, Username: "viewer-account", Cookie: []byte("ticket"),
		DeviceID: "web-device", LinkdAddresses: []string{"127.0.0.1:1299"}}
}

func testSessionSource(context.Context, string) (Session, error) {
	return validTestSession(), nil
}

func TestSessionValidation(t *testing.T) {
	t.Parallel()
	for _, username := range []string{"viewer-account", "0"} {
		session := validTestSession()
		session.Username = username
		if err := session.Validate(); err != nil {
			t.Fatalf("valid username %q: %v", username, err)
		}
	}
	for _, test := range []struct {
		name string
		edit func(*Session)
	}{
		{"uid", func(s *Session) { s.UID = 0 }},
		{"username", func(s *Session) { s.Username = "" }},
		{"cookie", func(s *Session) { s.Cookie = nil }},
		{"device", func(s *Session) { s.DeviceID = "" }},
		{"oversized", func(s *Session) { s.Username = strings.Repeat("x", 65536) }},
		{"address", func(s *Session) { s.LinkdAddresses = []string{"bad"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := validTestSession()
			test.edit(&session)
			if err := session.Validate(); err == nil {
				t.Fatal("accepted invalid messaging identity")
			}
		})
	}
}

func TestLinkdRoutingHints(t *testing.T) {
	t.Parallel()
	got := LinkdAddresses([]string{"bad", "127.0.0.1:1299", "/127.0.0.1:1299"})
	if len(got) < 2 || got[0] != "127.0.0.1:1299" || slices.Contains(got, "bad") || slices.Contains(got[1:], got[0]) {
		t.Fatalf("routes=%v", got)
	}
}
