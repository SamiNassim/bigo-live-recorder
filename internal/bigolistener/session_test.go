package bigolistener

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func validTestSession() Session {
	return Session{
		SchemaVersion:         1,
		UID:                   "123",
		Username:              "viewer-account",
		CookieBase64:          base64.StdEncoding.EncodeToString([]byte("cookie")),
		UUID:                  "456",
		DeviceID:              "device-id",
		CountryCode:           "FR",
		LanguageCode:          1,
		ComposedClientVersion: 10,
		SDKVersion:            20,
		ClientVersionCode:     30,
		ClientType:            1,
		AndroidSDK:            35,
		LinkdAddresses:        []string{"127.0.0.1:1299"},
	}
}

func TestParseSessionJSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(validTestSession())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSessionJSON(string(raw))
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if got.UID != "123" {
		t.Fatalf("unexpected uid %q", got.UID)
	}
}

func TestParseSessionJSONRejectsGuestAndInvalidAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Session)
		want   string
	}{
		{"guest", func(s *Session) { s.Username = "0" }, "guest"},
		{"address", func(s *Session) { s.LinkdAddresses = []string{"bad"} }, "valid Linkd address"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := validTestSession()
			test.mutate(&session)
			raw, _ := json.Marshal(session)
			_, err := ParseSessionJSON(string(raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected error containing %q, got %v", test.want, err)
			}
		})
	}
}
