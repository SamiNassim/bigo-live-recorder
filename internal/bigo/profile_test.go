package bigo

import (
	"encoding/binary"
	"testing"
)

func profileFixture(uid uint32, values map[string]string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 900000010)
	for _, value := range []uint32{60, 7, 1, uid, 12, uint32(len(values))} {
		b = binary.LittleEndian.AppendUint32(b, value)
	}
	for key, value := range values {
		b = appendNativeBlob(b, []byte(key))
		b = appendNativeBlob(b, []byte(value))
	}
	return binary.LittleEndian.AppendUint32(b, 200)
}

func TestGuestProfileUsesPublicIdentityAndChecksOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		id     string
	}{
		{"public alias", map[string]string{"pd_bigo_id": "guest.example", "user_name": "900000001", "nick_name": "Guest 🌙"}, "guest.example"},
		{"bound ID", map[string]string{"user_name": "guest.example", "bind_status": "1", "nick_name": "Guest"}, "guest.example"},
		{"default numeric ID differs from routing UID", map[string]string{"yyuid": "1100000001", "user_name": "900000001", "bind_status": "16", "nick_name": "Guest"}, "1100000001"},
		{"default name is not the routing UID", map[string]string{"yyuid": "1100000001", "user_name": "900000001", "bind_status": "16", "nick_name": "ID:1100000001"}, "1100000001"},
		{"default numeric ID is authoritative even when equal", map[string]string{"yyuid": "900000001", "nick_name": "Guest"}, "900000001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := profileFixture(900000001, test.values)
			profile, err := decodeProfile(body, 900000010, 7, 900000001)
			if err != nil || profile.UID != 900000001 || profile.ID != test.id || profile.Name != test.values["nick_name"] || profile.IdentityVersion != ProfileIdentityVersion {
				t.Fatal("lost guest identity", profile, err)
			}
			if _, err := decodeProfile(body, 900000010, 7, 900000002); err == nil {
				t.Fatal("accepted another guest")
			}
			if _, err := decodeProfile(body, 900000011, 7, 900000001); err == nil {
				t.Fatal("accepted another viewer")
			}
			if _, err := decodeProfile(body, 900000010, 8, 900000001); err == nil {
				t.Fatal("accepted another sequence")
			}
			for n := 0; n < len(body)-4; n++ {
				if _, err := decodeProfile(body[:n], 900000010, 7, 900000001); err == nil {
					t.Fatal("accepted truncated profile", n)
				}
			}
		})
	}
	for _, values := range []map[string]string{
		{"user_name": "private-login", "bind_status": "0", "nick_name": "Guest"},
		{"pd_bigo_id": "../unsafe", "nick_name": "Guest"},
	} {
		if _, err := decodeProfile(profileFixture(900000001, values), 900000010, 7, 900000001); err == nil {
			t.Fatal("accepted a non-public or unsafe ID")
		}
	}
}

func TestGuestPublicIDNeverComesFromRoutingUIDOrNickname(t *testing.T) {
	profile, err := decodeProfile(profileFixture(900000001, map[string]string{"user_name": "900000001", "bind_status": "16", "nick_name": "ID:1100000001"}), 900000010, 7, 900000001)
	if err == nil || profile.ID != "" || profile.UID != 900000001 || profile.Name != "ID:1100000001" || profile.IdentityVersion != 0 {
		t.Fatal("invented a public identity or discarded the known guest name", profile, err)
	}
}
