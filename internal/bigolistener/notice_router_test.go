package bigolistener

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func hintFixture(sequence int32, image string) liveEvent {
	event := liveEvent{URI: uriNewLiveNotify, AppID: protocolAppID, Sequence: sequence}
	if image != "" {
		event.Fields = map[string]string{"avatar": "http://img.example.test/" + image + ".jpg?signature=hint"}
	}
	return event
}

func detailFixture(uid uint32, image string) liveEvent {
	return liveEvent{URI: uriLiveStartNotify, AppID: protocolAppID, Content: fmt.Sprintf(`{"key_extra":"bigolive://livevideoshow?roomid=8589934634&uid=%d","key_reserved":{"owner_uid":%d,"loc-avatar":"https://img.example.test/%s.jpg?signature=detail"}}`, uid, uid, image)}
}

func TestNoticePairDispatchesOnlyIdentifiedTarget(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			var r noticeRouter
			now := time.Now()
			events := []liveEvent{hintFixture(1, "second"), detailFixture(222, "second")}
			if reverse {
				events[0], events[1] = events[1], events[0]
			}
			var targets []noticeIdentity
			for i, event := range events {
				if id, ok := r.accept(now.Add(time.Duration(i)*1800*time.Millisecond), event); ok {
					targets = append(targets, id)
				}
			}
			if len(targets) != 1 || targets[0].uid != 222 {
				t.Fatal("paired alert did not target exactly one account", targets)
			}
			if r.expire(now.Add(2*noticePairGrace)) || r.unresolved() || !r.deadline().IsZero() {
				t.Fatal("paired hint left a broad fallback or retained state")
			}
		})
	}
}

func TestNoticeInterleavedPairsDoNotCancelUnrelatedHint(t *testing.T) {
	var r noticeRouter
	now := time.Now()
	r.accept(now, hintFixture(1, "first"))
	r.accept(now, hintFixture(2, "second"))
	if _, ok := r.accept(now.Add(time.Second), detailFixture(222, "second")); !ok {
		t.Fatal("identified alert was delayed")
	}
	if !r.expire(now.Add(noticePairGrace)) {
		t.Fatal("another account's detail canceled the unmatched fallback")
	}
	if r.expire(now.Add(2 * noticePairGrace)) {
		t.Fatal("fallback repeated without a new signal")
	}
}

func TestNoticeInterleavedPairsResolveIndependently(t *testing.T) {
	var r noticeRouter
	now := time.Now()
	events := []liveEvent{detailFixture(111, "first"), hintFixture(1, "second"), detailFixture(222, "second"), hintFixture(2, "first")}
	var targets []uint32
	for i, event := range events {
		if id, ok := r.accept(now.Add(time.Duration(i)*100*time.Millisecond), event); ok {
			targets = append(targets, id.uid)
		}
	}
	if len(targets) != 2 || targets[0] != 111 || targets[1] != 222 || r.expire(now.Add(2*noticePairGrace)) {
		t.Fatal("interleaved notifications were attributed to the wrong stream", targets)
	}
}

func TestLateIdentifiedNoticeStillRoutesAfterFallback(t *testing.T) {
	var r noticeRouter
	now := time.Now()
	r.accept(now, hintFixture(1, "second"))
	if !r.expire(now.Add(noticePairGrace)) {
		t.Fatal("missing-detail fallback was lost")
	}
	if id, ok := r.accept(now.Add(2*noticePairGrace), detailFixture(222, "second")); !ok || id.uid != 222 {
		t.Fatal("late detail was discarded after the generic fallback")
	}
}

func TestNoticeUnidentifiedHintsWaitThenFallBack(t *testing.T) {
	for _, image := range []string{"", "first"} {
		var r noticeRouter
		now := time.Now()
		if _, ok := r.accept(now, hintFixture(1, image)); ok {
			t.Fatal("hint dispatched before attribution")
		}
		if r.expire(now.Add(noticePairGrace-time.Nanosecond)) || !r.unresolved() {
			t.Fatal("hint was lost or fallback ran early")
		}
		if !r.expire(now.Add(noticePairGrace)) || r.unresolved() {
			t.Fatal("unidentified hint did not fall back once")
		}
	}
}

func TestNoticeSharedAvatarStaysAmbiguous(t *testing.T) {
	var r noticeRouter
	now := time.Now()
	r.accept(now, hintFixture(1, "shared"))
	r.accept(now, hintFixture(2, "shared"))
	r.accept(now.Add(time.Second), detailFixture(222, "shared"))
	if !r.expire(now.Add(noticePairGrace)) {
		t.Fatal("ambiguous image identity canceled all pending hints")
	}
}

func TestNoticeDuplicateHintDoesNotCreateExtraFallback(t *testing.T) {
	var r noticeRouter
	now := time.Now()
	hint := hintFixture(1, "second")
	r.accept(now, hint)
	r.accept(now.Add(time.Second), detailFixture(222, "second"))
	r.accept(now.Add(2*time.Second), hint)
	if r.expire(now.Add(2 * noticePairGrace)) {
		t.Fatal("duplicate generic event left an unmatched fallback")
	}
}

func TestNoticeCacheCapacityKeepsFallbackAndBoundsMemory(t *testing.T) {
	var r noticeRouter
	now := time.Now()
	for i := range 2 * maxNoticeHints {
		r.accept(now, hintFixture(int32(i+1), fmt.Sprint(i)))
		r.accept(now, detailFixture(uint32(i+1000), "unrelated"))
	}
	if len(r.hints) > maxNoticeHints || len(r.details) > maxNoticeHints {
		t.Fatal("notification burst exceeded cache bounds")
	}
	if !r.expire(now.Add(noticePairGrace)) || r.unresolved() || !r.deadline().IsZero() {
		t.Fatal("overflow lost its fallback or retained expired data")
	}
}

func TestIdentifiedUnmonitoredNoticeDoesNotWakeWatchers(t *testing.T) {
	h := NewHub(context.Background(), nil, nil, nil, nil)
	defer h.Close()
	events, off, err := h.Subscribe("public-one", 111)
	if err != nil {
		t.Fatal(err)
	}
	defer off()
	var r noticeRouter
	now := time.Now()
	r.accept(now, hintFixture(1, "other"))
	id, ok := r.accept(now.Add(time.Second), detailFixture(999, "other"))
	if !ok {
		t.Fatal("unmonitored account was not identified")
	}
	h.dispatch(id)
	if r.expire(now.Add(2*noticePairGrace)) || len(events) != 0 {
		t.Fatal("unmonitored alert triggered profile checks")
	}
}

func TestIdentifiedNoticeUpgradesBufferedFallback(t *testing.T) {
	h := NewHub(context.Background(), nil, nil, nil, nil)
	defer h.Close()
	events, off, err := h.Subscribe("public-one", 111)
	if err != nil {
		t.Fatal(err)
	}
	defer off()
	h.dispatch(noticeIdentity{})
	h.dispatch(noticeIdentity{uid: 111})
	if n := <-events; !n.Attributed || len(events) != 0 {
		t.Fatal("buffered fallback displaced the identified alert")
	}
}
