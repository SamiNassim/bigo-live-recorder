package bigolistener

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNoticeUsesExplicitTargetFields(t *testing.T) {
	for _, content := range []string{
		`{"uid":222}`,
		`{"uid":"222"}`,
		`{"owner_uid":222}`,
		`{"key_extra":"bigolive://livevideoshow?roomid=8589934634&uid=222"}`,
		`{"key_reserved":{"owner_uid":222}}`,
		`{"key_reserved":"{\"owner_uid\":\"222\"}"}`,
		`{"key_extra":"bigolive://livevideoshow?uid=222","key_reserved":{"owner_uid":222,"poster_uid":333}}`,
	} {
		n := describeLiveNotice(liveEvent{Content: content})
		if n.identity.uid != 222 || n.conflict {
			t.Fatalf("target not resolved from %s: %+v", content, n)
		}
	}
	n := describeLiveNotice(liveEvent{Fields: map[string]string{"owner_uid": "222"}})
	if n.identity.uid != 222 {
		t.Fatal("extension target was ignored", n)
	}
	n = describeLiveNotice(liveEvent{Content: `{"bigo_id":"public-two"}`})
	if n.identity.publicID != "public-two" {
		t.Fatal("public identifier was ignored", n)
	}
}

func TestNoticeDoesNotIdentifyIncidentalOrConflictingDigits(t *testing.T) {
	for _, content := range []string{
		`{"key_msg":"222","key_title":"public-two","key_reserved":{"seqid":"222","poster_uid":222}}`,
		`{"avatar":"https://img.example.test/222.jpg?uid=222"}`,
		`{"key_extra":"https://example.test/?uid=222"}`,
		`{"key_extra":"bigolive://other?uid=222"}`,
		`{"key_extra":"bigolive://livevideoshow?uid=222&uid=333"}`,
		`{"uid":0}`,
		`{"uid":-1}`,
		`{"uid":4294967296}`,
		`{"uid":222.5}`,
		`{"uid":111,"key_reserved":{"owner_uid":222}}`,
		`{"key_extra":"bigolive://livevideoshow?uid=111","key_reserved":{"owner_uid":222}}`,
		`not JSON`,
	} {
		if n := describeLiveNotice(liveEvent{Content: content}); n.identity.known() {
			t.Fatalf("unreliable identity accepted from %s: %+v", content, n)
		}
	}
}

func TestNoticeCorrelatesAvatarPathsWithoutSignedQueries(t *testing.T) {
	content := `{"key_reserved":{"loc-avatar":"https://img.example.test/second.jpg?signature=detail","big-loc-avatar":"https://img.example.test/large.jpg?signature=large"}}`
	detail := describeLiveNotice(liveEvent{Content: content})
	hint := describeLiveNotice(liveEvent{Fields: map[string]string{"avatar": "http://IMG.EXAMPLE.TEST/second.jpg?signature=hint"}})
	if !sameNoticeAvatar(hint.avatars, detail.avatars) || len(detail.avatars) != 2 {
		t.Fatal("paired signed URLs did not correlate", hint, detail)
	}
	for _, address := range []string{"", "https://img.example.test/", "file:///second.jpg", "https://user@img.example.test/second.jpg", "https://img.example.test/" + strings.Repeat("a", 4096)} {
		if key := noticeAvatarKey(address); key != "" {
			t.Fatal("unsuitable image identity accepted", key)
		}
	}
	reserved, _ := json.Marshal(map[string]string{"loc-avatar": "https://img.example.test/second.jpg?signature=quoted"})
	quoted, _ := json.Marshal(map[string]string{"key_reserved": string(reserved)})
	if n := describeLiveNotice(liveEvent{Content: string(quoted)}); !sameNoticeAvatar(n.avatars, hint.avatars) {
		t.Fatal("quoted reserved metadata lost its image identity")
	}
}
