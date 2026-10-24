package recording

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/state"
)

// Opt-in only: ordinary tests never contact BIGO or use saved credentials.
func TestLiveCookieRecording(t *testing.T) {
	path, id := os.Getenv("BIGO_LIVE_SESSION_FILE"), os.Getenv("BIGO_LIVE_ID")
	if path == "" || id == "" {
		t.Skip("set BIGO_LIVE_SESSION_FILE and BIGO_LIVE_ID for a live smoke test")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Uniqid string `json:"uniqid"`
	}
	if err = json.Unmarshal(content, &session); err != nil {
		t.Fatal(err)
	}
	client := bigo.NewClient()
	client.Configure(session.Uniqid, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	account, err := client.Account(ctx)
	if err != nil || account.ID == "" {
		t.Fatalf("account validation: %v", err)
	}
	stream, err := client.Check(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Alive != 1 {
		t.Fatal("test ID is offline")
	}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	h, err := Start(ctx, state.Streamer{ID: id, Name: stream.Name}, stream.URL(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(8 * time.Second):
	case <-h.Done():
		t.Fatalf("recording ended prematurely: %v", h.Result().Err)
	}
	start := time.Now()
	h.Stop()
	r := h.Result()
	if r.Err != nil || r.Bytes == 0 {
		t.Fatalf("recording result: %+v", r)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("slow stop")
	}
	if probe, err := exec.LookPath("ffprobe"); err == nil {
		out, err := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_name,width,height", "-of", "json", r.Path).Output()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Media verified: %s; stop took %s", out, time.Since(start))
	}
}
