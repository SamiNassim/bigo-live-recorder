package bigo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPresenceResolvesNativeRoomAndProfileInOneRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/OUserCenter/getUserInfoStudio" || r.URL.Query().Get("bigoId") != "demo.streamer" {
			t.Error("unexpected presence request", r.URL.Path)
		}
		fmt.Fprint(w, `{"code":0,"data":{"alive":1,"uid":"123","nickname":"Demo Streamer","ogImage":"https://images.example/avatar.jpg","aliOSUrl":"bigolive://livevideoshow?roomid=8589934634&uid=123&visitor=0"}}`)
	}))
	defer server.Close()
	client := NewClient()
	client.base, client.http = server.URL, server.Client()
	source, err := client.Presence(context.Background(), "demo.streamer")
	if err != nil || source.Alive != 1 || source.UID != 123 || source.RoomID != "8589934634" || source.Name != "Demo Streamer" || source.Avatar != "https://images.example/avatar.jpg" {
		t.Fatal("presence lost native room or account metadata", source, err)
	}
	if calls != 1 || source.Playable() {
		t.Fatal("presence fetched or invented a playback URL", calls, source)
	}
}

func TestPresenceAppLinkRequiresMatchingIdentityAndValidRoom(t *testing.T) {
	for _, link := range []string{
		"bigolive://livevideoshow?roomid=8589934634&uid=456",
		"bigolive://livevideoshow?roomid=8589934634",
		"bigolive://livevideoshow?roomid=0&uid=123",
		"bigolive://livevideoshow?roomid=4294967296&uid=123",
		"bigolive://livevideoshow?roomid=invalid&uid=123",
		"bigolive://livevideoshow?roomid=18446744073709551616&uid=123",
		"https://livevideoshow?roomid=8589934634&uid=123",
		"bigolive://other?roomid=8589934634&uid=123",
		"bigolive://user@livevideoshow?roomid=8589934634&uid=123",
	} {
		t.Run(link, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"alive": 1, "uid": 123, "aliOSUrl": link})
			if err != nil {
				t.Fatal(err)
			}
			var source Stream
			if err := json.Unmarshal(data, &source); err != nil || source.RoomID != "" {
				t.Fatal("invalid or mismatched room link was accepted", source.RoomID, err)
			}
		})
	}
}

func TestPresenceDoesNotReplaceExplicitRoomOrUseOfflineLink(t *testing.T) {
	for _, test := range []struct {
		alive int
		room  string
	}{
		{alive: 1, room: "12884901931"},
		{alive: 0, room: ""},
	} {
		data, _ := json.Marshal(map[string]any{"alive": test.alive, "uid": 123, "roomId": test.room, "aliOSUrl": "bigolive://livevideoshow?roomid=8589934634&uid=123"})
		var source Stream
		if err := json.Unmarshal(data, &source); err != nil || source.RoomID != test.room {
			t.Fatal("explicit room or offline status was ignored", source, err)
		}
	}
}
