package bigo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"bigo-live-recorder/internal/bigolistener"
)

// Profile links a transport UID to the public BIGO ID and display name.
type Profile struct {
	UID  uint32
	ID   string
	Name string
}

// Profile reads account metadata over the existing shared native connection.
func (c *Client) Profile(ctx context.Context, uid uint32, hub *bigolistener.Hub) (Profile, error) {
	if uid == 0 || hub == nil {
		return Profile{}, errors.New("BIGO guest identity is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	seq := nextPJoinSeq()
	var viewer uint32
	frame, err := hub.QueryAccount(ctx, 595485, 595741, func(account uint32) ([]byte, func([]byte) bool, error) {
		viewer = account
		b := binary.LittleEndian.AppendUint32(nil, account)
		b = binary.LittleEndian.AppendUint32(b, 60)
		b = binary.LittleEndian.AppendUint32(b, seq)
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint32(b, uid)
		columns := []string{"pd_bigo_id", "user_name", "bind_status", "nick_name"}
		b = binary.LittleEndian.AppendUint32(b, uint32(len(columns)))
		for _, column := range columns {
			b = appendNativeBlob(b, []byte(column))
		}
		b = appendNativeBlob(b, []byte("en"))
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint32(b, 0)
		return b, func(reply []byte) bool { return len(reply) >= 12 && binary.LittleEndian.Uint32(reply[8:]) == seq }, nil
	})
	if err != nil {
		return Profile{}, err
	}
	if frame.Status != 200 {
		return Profile{}, fmt.Errorf("BIGO guest profile rejected (status %d)", frame.Status)
	}
	return decodeProfile(frame.Body, viewer, seq, uid)
}

func decodeProfile(body []byte, viewer, seq, uid uint32) (Profile, error) {
	r := nativeReader{data: body}
	if r.u32() != viewer || r.u32() != 60 || r.u32() != seq {
		return Profile{}, errors.New("BIGO guest profile belongs to another request")
	}
	var profile Profile
	for range r.count(8) {
		owner := r.u32()
		r.u32() // Profile schema version.
		values := map[string]string{}
		for range r.count(4) {
			key := string(r.blob())
			values[key] = string(r.blob())
		}
		if owner == uid {
			id := values["pd_bigo_id"]
			bound, _ := strconv.ParseUint(values["bind_status"], 10, 32)
			if id == "" && (bound&1 != 0 || values["user_name"] == strconv.FormatUint(uint64(owner), 10)) {
				id = values["user_name"]
			}
			profile = Profile{UID: owner, ID: id, Name: values["nick_name"]}
		}
	}
	if r.err != nil {
		return Profile{}, r.err
	}
	if len(body)-r.off >= 4 {
		if code := r.u32(); code != 0 && code != 200 {
			return Profile{}, fmt.Errorf("BIGO guest profile rejected (code %d)", code)
		}
	}
	if profile.UID != uid || !ValidID(profile.ID) || profile.Name == "" {
		return Profile{}, errors.New("BIGO did not supply the requested guest ID and name")
	}
	return profile, nil
}
