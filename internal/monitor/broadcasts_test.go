package monitor

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

type broadcastProfileClient struct {
	sourceClient
	calls atomic.Int64
}

func (c *broadcastProfileClient) Profile(_ context.Context, uid uint32, _ *bigolistener.Hub) (bigo.Profile, error) {
	c.calls.Add(1)
	return bigo.Profile{UID: uid, ID: "guest.example", Name: "Guest", IdentityVersion: bigo.ProfileIdentityVersion}, nil
}

func TestBroadcastGuestProfilesSurviveRecorderRestarts(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &broadcastProfileClient{}
	e := &Engine{store: store, log: log, client: client}
	streamer := state.Streamer{ID: "host.example"}
	source := bigo.Stream{Alive: 1, UID: 900000001, RoomID: "8589934634", RoomKind: bigo.RoomHybridMultiGuest}
	first, err := e.broadcastOptions(context.Background(), streamer, source, store.Config())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := first.GuestProfile(context.Background(), 900000002)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(d *state.Data) error {
		d.Recordings = append(d.Recordings, state.Recording{BroadcastID: first.Broadcast.ID, GuestUID: profile.UID, GuestID: profile.ID, GuestName: profile.Name, GuestIdentityVersion: profile.IdentityVersion})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	again, err := e.broadcastOptions(context.Background(), streamer, source, store.Config())
	if err != nil {
		t.Fatal(err)
	}
	cached, err := again.GuestProfile(context.Background(), profile.UID)
	if err != nil || cached != profile || client.calls.Load() != 1 {
		t.Fatal("guest identity was not retained for the broadcast", cached, err)
	}
}

func TestLegacyGuestIdentityCacheIsResolvedAgain(t *testing.T) {
	store, log := fixture(t, false, false)
	client := &broadcastProfileClient{}
	e := &Engine{store: store, log: log, client: client}
	streamer := state.Streamer{ID: "host.example"}
	source := bigo.Stream{Alive: 1, UID: 900000001, RoomID: "8589934634", RoomKind: bigo.RoomHybridMultiGuest}
	first, err := e.broadcastOptions(context.Background(), streamer, source, store.Config())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(d *state.Data) error {
		d.Recordings = append(d.Recordings, state.Recording{BroadcastID: first.Broadcast.ID, GuestUID: 900000002, GuestID: "900000002", GuestName: "Guest"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	options, err := e.broadcastOptions(context.Background(), streamer, source, store.Config())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := options.GuestProfile(context.Background(), 900000002)
	if err != nil || profile.ID != "guest.example" || profile.IdentityVersion != bigo.ProfileIdentityVersion || client.calls.Load() != 1 {
		t.Fatal("legacy routing UID was reused as a public identity", profile, err)
	}
}

func TestPreviousBroadcastVerificationDoesNotPollAnActiveRoomOrInferEndFromTimeout(t *testing.T) {
	store, log := fixture(t, true, false)
	broadcast := state.Broadcast{ID: "900000001:8589934634", StreamerID: "test", OwnerUID: 900000001, RoomID: "8589934634", Files: 1, Bytes: 10}
	if err := store.Update(func(d *state.Data) error { d.Broadcasts = append(d.Broadcasts, broadcast); return nil }); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	responseErr := error(context.DeadlineExceeded)
	client := &sourceClient{native: func(context.Context, bigo.Stream) (bigo.Stream, error) {
		calls.Add(1)
		return bigo.Stream{}, responseErr
	}}
	e := &Engine{store: store, log: log, client: client}
	e.verifyPreviousBroadcasts(context.Background(), "test", bigo.Stream{Alive: 1, RoomID: broadcast.RoomID}, store.Config())
	e.wg.Wait()
	if calls.Load() != 0 {
		t.Fatal("healthy current room was polled")
	}
	e.verifyPreviousBroadcasts(context.Background(), "test", bigo.Stream{}, store.Config())
	e.wg.Wait()
	if !store.Data().Broadcasts[0].EndedAt.IsZero() || len(log.Entries()) != 0 {
		t.Fatal("timeout was mistaken for room closure")
	}
	responseErr = bigo.ErrRoomEnded
	e.verifyPreviousBroadcasts(context.Background(), "test", bigo.Stream{}, store.Config())
	e.wg.Wait()
	e.verifyPreviousBroadcasts(context.Background(), "test", bigo.Stream{}, store.Config())
	e.wg.Wait()
	if store.Data().Broadcasts[0].EndedAt.IsZero() || calls.Load() != 2 || len(log.Entries()) != 1 {
		t.Fatal("room closure was not confirmed exactly once")
	}
}

func TestBroadcastDirectorySurvivesReconnectRenameAndRestart(t *testing.T) {
	store, log := fixture(t, false, false)
	e := &Engine{store: store, log: log}
	streamer := state.Streamer{ID: "host.example", Name: "Host"}
	source := bigo.Stream{Alive: 1, UID: 900000001, RoomID: "8589934634", RoomKind: bigo.RoomHybridMultiGuest}
	first, err := e.broadcastOptions(context.Background(), streamer, source, store.Config())
	if err != nil {
		t.Fatal(err)
	}
	if first.Broadcast.ID == "" || !strings.Contains(first.Broadcast.Directory, "room-8589934634") {
		t.Fatal("missing full room identity")
	}
	streamer.Name = "New display name"
	cfg := store.Config()
	cfg.RecordingPath = t.TempDir()
	cfg.FolderPerStreamer = false
	reopened, err := state.Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	e.store = reopened
	again, err := e.broadcastOptions(context.Background(), streamer, source, cfg)
	if err != nil || again.Broadcast.Directory != first.Broadcast.Directory || again.Broadcast.ID != first.Broadcast.ID || len(reopened.Data().Broadcasts) != 1 {
		t.Fatal("reconnect or restart split a broadcast", err)
	}
	// The two rooms deliberately share a 32-bit transport SID.
	source.RoomID = "12884901930"
	other, err := e.broadcastOptions(context.Background(), streamer, source, cfg)
	if err != nil || other.Broadcast.ID == first.Broadcast.ID || other.Broadcast.Directory == first.Broadcast.Directory {
		t.Fatal("different full room IDs were conflated", err)
	}
	copy := reopened.Data()
	copy.Broadcasts[0].Directory = "mutated"
	if reopened.Data().Broadcasts[0].Directory == "mutated" {
		t.Fatal("broadcast snapshot aliases the store")
	}
}

func TestBroadcastSuppressesTrackLogsAndFinalizesOnceAfterRoomEnd(t *testing.T) {
	store, log := fixture(t, false, false)
	broadcast := state.Broadcast{ID: "900000001:8589934634", StreamerID: "test", RoomID: "8589934634", Directory: "/fixture/room", StartedAt: time.Now()}
	if err := store.Update(func(d *state.Data) error { d.Broadcasts = append(d.Broadcasts, broadcast); return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &Engine{store: store, log: log, ctx: ctx, events: make(chan Event, 8)}
	h := &rollingRecording{fakeRecording: fakeRecording{done: make(chan struct{})}}
	it := &item{row: Row{Streamer: state.Streamer{ID: "test"}, RoomKind: bigo.RoomHybridMultiGuest}, handle: h, broadcastID: broadcast.ID}
	for i, path := range []string{"/fixture/room/a.mkv", "/fixture/room/b.mka", "/fixture/room/c.mkv"} {
		h.path, h.stamp = path, time.Now()
		h.changes = []recording.FileChange{{Path: path, StartedAt: h.stamp, BroadcastID: broadcast.ID, Guest: bigo.Profile{UID: 900000002, ID: "guest.example", Name: "Guest"}}, {Path: path, Result: &recording.Result{Path: path, EndedAt: time.Now(), Bytes: int64(i + 1)}}}
		e.fileChanges(it)
	}
	if len(log.Entries()) != 1 || len(e.events) != 1 || !e.broadcastNotified(broadcast.ID) {
		t.Fatal("guest changes spammed logs or notifications", log.Entries())
	}
	for _, saved := range store.Data().Recordings {
		if saved.BroadcastID != broadcast.ID || saved.GuestID != "guest.example" || saved.GuestName != "Guest" {
			t.Fatal("guest identity missing from recording history")
		}
	}
	// A transport ending or a pause finalizes files, not the room itself.
	h.Stop()
	e.completed(it)
	if len(log.Entries()) != 1 || !store.Data().Broadcasts[0].EndedAt.IsZero() {
		t.Fatal("a recorder stop was treated as broadcast completion")
	}
	e.finishBroadcast(broadcast.ID)
	e.finishBroadcast(broadcast.ID)
	saved := store.Data().Broadcasts[0]
	if saved.Files != 3 || saved.Bytes != 6 || saved.EndedAt.IsZero() || len(log.Entries()) != 2 {
		t.Fatal("incorrect or duplicate completion summary", saved, log.Entries())
	}
	if !strings.Contains(log.Entries()[len(log.Entries())-1].Message, "broadcast finalized") {
		t.Fatal("missing broadcast summary")
	}
}
