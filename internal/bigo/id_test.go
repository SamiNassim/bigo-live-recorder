package bigo

import "testing"

func TestParseProfileID(t *testing.T) {
	for _, raw := range []string{"123456789", " 123456789 ", "https://www.bigo.tv/123456789", "https://bigo.tv/123456789/", "https://WWW.BIGO.TV/123456789?ref=share#live"} {
		got, err := ParseID(raw)
		if err != nil || got != "123456789" {
			t.Errorf("%q: %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "@alias", "https://bigo.tv.evil.test/123", "https://evil.test/123", "https://bigo.tv@evil.test/123", "https://bigo.tv:8000/123", "https://bigo.tv/", "https://bigo.tv/a/b", "https://bigo.tv/a%2Fb", "ftp://bigo.tv/123", "https://bigo.tv/..", "//bigo.tv/123"} {
		if id, err := ParseID(raw); err == nil {
			t.Errorf("accepted %q as %q", raw, id)
		}
	}
	if id, err := ParseID("example_alias"); err != nil || id != "example_alias" {
		t.Fatal(id, err)
	}
}
