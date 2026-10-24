package monitor

import (
	"bigo-live-recorder/internal/state"
	"testing"
	"time"
)

func TestExplicitPollingPermission(t *testing.T) {
	c := state.Defaults()
	if eligible(c, true, false) {
		t.Fatal("implicit polling in native-only mode")
	}
	if !eligible(c, false, false) || !eligible(c, true, true) {
		t.Fatal("bootstrap or signal not checked")
	}
	c.PollingBackup = true
	if !eligible(c, true, false) {
		t.Fatal("explicit backup not checked")
	}
	c.PollingBackup = false
	c.NativeListening = false
	if !eligible(c, true, false) {
		t.Fatal("polling-only mode not checked")
	}
}
func TestQueueCalculator(t *testing.T) {
	c := state.Defaults()
	normal, cold := EffectiveIntervals(c, 100)
	if normal != 200*time.Second || cold != 300*time.Second {
		t.Fatalf("%v %v", normal, cold)
	}
	normal, _ = EffectiveIntervals(c, 0)
	if normal != 75*time.Second {
		t.Fatal(normal)
	}
}
