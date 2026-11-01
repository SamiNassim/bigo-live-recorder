package monitor

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/state"
)

// Opt-in real-server test of the production cookie-to-native path. This follows
// the requested target and enables its live notices, just like the app does.
// Ordinary tests never contact BIGO or change account state.
func TestLiveCookieListener(t *testing.T) {
	path, publicID, uidText := os.Getenv("BIGO_LISTENER_COOKIE_FILE"), os.Getenv("BIGO_LISTENER_ID"), os.Getenv("BIGO_LISTENER_UID")
	if path == "" || publicID == "" || uidText == "" {
		t.Skip("set BIGO_LISTENER_COOKIE_FILE, BIGO_LISTENER_ID and BIGO_LISTENER_UID to opt in")
	}
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 {
		t.Fatal("invalid target UID")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := bigo.ParseCookie(string(raw))
	if err != nil || cookie == "" {
		t.Fatalf("invalid web cookie: %v", err)
	}
	wait := time.Duration(0)
	if value := os.Getenv("BIGO_LISTENER_WAIT"); value != "" {
		wait, err = time.ParseDuration(value)
		if err != nil || wait <= 0 || wait > 5*time.Minute {
			t.Fatal("BIGO_LISTENER_WAIT must be positive and no more than 5m")
		}
	}
	client := bigo.NewClient()
	client.Configure(cookie, 2*time.Second)
	e := &Engine{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second+wait)
	defer cancel()
	hub := bigolistener.NewHub(ctx, e.messagingSession, func(v string) { t.Log(v) }, func(v string) { t.Log(v) }, nil)
	defer hub.Close()
	hub.Configure(true, cookie, state.Defaults().NativeDeviceID)
	notifications, unsubscribe, err := hub.Subscribe(publicID, uint32(uid))
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !hub.Status().Connected || hub.Status().ReadyTargets != 1 {
		select {
		case <-ctx.Done():
			t.Fatalf("native connection did not become ready: %+v", hub.Status())
		case <-tick.C:
		}
	}
	t.Log("READY: production listener authenticated and subscription confirmed using only the cookie")
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for {
			select {
			case n := <-notifications:
				t.Logf("notification: attributed=%t", n.Attributed)
				if n.Attributed {
					return
				}
			case <-timer.C:
				t.Fatal("no target-attributed notification within observation window")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
}
