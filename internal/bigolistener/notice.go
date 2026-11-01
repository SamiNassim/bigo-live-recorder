package bigolistener

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type noticeIdentity struct {
	uid      uint32
	publicID string
}

func (id noticeIdentity) known() bool { return id.uid != 0 || id.publicID != "" }

type liveNotice struct {
	identity noticeIdentity
	avatars  []string
	conflict bool
}

// Read explicit identity fields, never incidental digits in titles, message
// IDs, signed image URLs, or another account's display name.
func describeLiveNotice(event liveEvent) liveNotice {
	var notice liveNotice
	if event.Content != "" {
		notice.readObject([]byte(event.Content))
	}
	if len(event.Fields) != 0 {
		data, _ := json.Marshal(event.Fields)
		notice.readObject(data)
	}
	if notice.conflict {
		notice.identity = noticeIdentity{}
	}
	return notice
}

type noticeFields struct {
	UID      json.RawMessage `json:"uid"`
	OwnerUID json.RawMessage `json:"owner_uid"`
	PublicID string          `json:"bigo_id"`
	Avatar   string          `json:"avatar"`
	Local    string          `json:"loc-avatar"`
	Large    string          `json:"big-loc-avatar"`
	Other    string          `json:"loc-avatar_2"`
}

func (n *liveNotice) readObject(data []byte) {
	var fields struct {
		noticeFields
		Extra    string          `json:"key_extra"`
		Reserved json.RawMessage `json:"key_reserved"`
	}
	if json.Unmarshal(data, &fields) != nil {
		return
	}
	n.addFields(fields.noticeFields)
	if link, err := url.Parse(fields.Extra); err == nil && link.Scheme == "bigolive" && link.Host == "livevideoshow" && link.User == nil {
		values := link.Query()["uid"]
		if len(values) == 1 {
			n.addUID(json.RawMessage(strconv.Quote(values[0])))
		}
	}
	// BIGO sends key_reserved as either an object or JSON inside a string.
	// Decode just that one nested object, not arbitrary user-controlled text.
	if len(fields.Reserved) != 0 {
		var quoted string
		if json.Unmarshal(fields.Reserved, &quoted) == nil {
			fields.Reserved = json.RawMessage(quoted)
		}
		var reserved noticeFields
		if json.Unmarshal(fields.Reserved, &reserved) == nil {
			n.addFields(reserved)
		}
	}
}

func (n *liveNotice) addFields(fields noticeFields) {
	n.addUID(fields.UID)
	n.addUID(fields.OwnerUID)
	if id := strings.TrimSpace(fields.PublicID); id != "" && len(id) <= 128 {
		if n.identity.publicID != "" && n.identity.publicID != id {
			n.conflict = true
		}
		n.identity.publicID = id
	}
	for _, avatar := range []string{fields.Avatar, fields.Local, fields.Large, fields.Other} {
		if key := noticeAvatarKey(avatar); key != "" && !slices.Contains(n.avatars, key) {
			n.avatars = append(n.avatars, key)
		}
	}
}

func (n *liveNotice) addUID(raw json.RawMessage) {
	value := string(raw)
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		value = quoted
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || uid == 0 {
		return
	}
	if n.identity.uid != 0 && n.identity.uid != uint32(uid) {
		n.conflict = true
	}
	n.identity.uid = uint32(uid)
}

func noticeAvatarKey(address string) string {
	if len(address) > 4096 {
		return ""
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path == "" || u.Path == "/" {
		return ""
	}
	// The paired messages use different signed queries, and may differ in
	// HTTP/HTTPS scheme. Only the image's host and path are stable.
	return strings.ToLower(u.Host) + u.EscapedPath()
}
