package app

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func startupFixture(t *testing.T) *application {
	t.Helper()
	a := accountFixture(t)
	a.cfg.Cookie = "synthetic-cookie"
	if err := a.store.SaveConfig(a.cfg); err != nil {
		t.Fatal(err)
	}
	a.snapshot.Listener = bigolistener.RuntimeStatus{State: "connected", Connected: true, ReadyTargets: 120, TotalTargets: 120}
	a.snapshot.InitialChecks = monitor.CheckProgress{Completed: 37, Total: 120}
	for i := range 120 {
		row := monitor.Row{Streamer: state.Streamer{ID: fmt.Sprintf("streamer_%03d", i)}, Status: "Waiting", Startup: true}
		if i < 37 {
			row.Status, row.LastCheck = "Offline", time.Now()
		}
		a.snapshot.Rows = append(a.snapshot.Rows, row)
	}
	a.snapshot.Rows[100].Status = "Checking"
	return a
}

func TestStartupProgressStaysAboveListAndOnlyOnStreamers(t *testing.T) {
	for _, mode := range []string{"light", "dark"} {
		for _, size := range [][2]int{{1050, 650}, {1400, 900}} {
			t.Run(fmt.Sprintf("%s-%d", mode, size[0]), func(t *testing.T) {
				a := startupFixture(t)
				a.cfg.Theme = mode
				tt := ui.NewTester(a.view, size[0], size[1])
				banner, ok := tt.Find("Monitoring startup")
				search, searchOK := tt.Find("Search streamers")
				if !ok || !searchOK || banner.Y+banner.H > search.Y || banner.X < 220 || banner.X+banner.W > float32(size[0]) {
					t.Fatal("startup progress overlaps the list or window", banner, search)
				}
				if !tt.HasText("Checking streamers…") || !tt.HasText("37 / 120 checked") {
					t.Fatal("off-screen checks have no global progress", tt.Texts())
				}
				_, last := a.streamList.Visible()
				if last >= 100 {
					t.Fatal("the checking row should be off-screen")
				}
				if dir := os.Getenv("BIGO_UI_SNAPSHOTS"); dir != "" {
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
					f, err := os.Create(filepath.Join(dir, fmt.Sprintf("startup-%s-%d.png", mode, size[0])))
					if err != nil {
						t.Fatal(err)
					}
					err = png.Encode(f, tt.Image())
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				for _, query := range []string{"streamer_000", "no matching streamer"} {
					a.query = query
					tt.Frame()
					if !tt.HasText("37 / 120 checked") {
						t.Fatal("search changed the full-list startup progress")
					}
				}
				for _, page := range []string{"Logs", "Settings", "Recordings"} {
					if err := tt.Click(page); err != nil {
						t.Fatal(err)
					}
					if _, ok := tt.Find("Monitoring startup"); ok {
						t.Fatal("startup progress escaped the Streamers tab", page)
					}
				}
				a.snapshot.InitialChecks.Completed = 60
				if err := tt.Click("Streamers"); err != nil {
					t.Fatal(err)
				}
				if !tt.HasText("60 / 120 checked") {
					t.Fatal("returning to Streamers showed stale progress")
				}
			})
		}
	}
}

func TestStartupCompletionExpiresAndLaterLiveChecksDoNotRestartIt(t *testing.T) {
	a := startupFixture(t)
	tt := ui.NewTester(a.view, 1050, 650)
	a.snapshot.InitialChecks.Completed = 120
	a.snapshot.Listener.State, a.snapshot.Listener.Connected = "connecting", false
	tt.Frame()
	if !tt.HasText("Finishing listener setup…") || tt.HasText("Monitoring ready") {
		t.Fatal("startup claimed readiness before the listener connected")
	}
	a.snapshot.Listener.State, a.snapshot.Listener.Connected = "connected", true
	tt.Frame()
	if !tt.HasText("Monitoring ready") {
		t.Fatal("successful startup was not acknowledged")
	}
	tt.Advance(4 * time.Second)
	if _, ok := tt.Find("Monitoring startup"); ok {
		t.Fatal("completed startup did not disappear")
	}
	// An event-triggered check is not another initial check.
	a.snapshot.Rows[0].Status = "Checking"
	tt.Frame()
	if _, ok := tt.Find("Monitoring startup"); ok {
		t.Fatal("a later live event restarted startup progress")
	}
	a.snapshot.Rows[0].Status = "Needs attention"
	a.snapshot.Listener.State, a.snapshot.Listener.Connected = "retrying", false
	tt.Frame()
	if _, ok := tt.Find("Monitoring startup"); ok {
		t.Fatal("a later recording error or reconnect restarted startup progress")
	}
	frames := 0
	a.page = "logs"
	tt = ui.NewTester(func(c *ui.Context) { frames++; a.view(c) }, 1050, 650)
	before := frames
	tt.Advance(5 * time.Second)
	if frames != before {
		t.Fatal("hidden startup progress kept redrawing")
	}
}

func TestStartupIssuesSettleWithoutSpinnerAndRevealAffectedStreamers(t *testing.T) {
	a := startupFixture(t)
	tt := ui.NewTester(a.view, 1050, 650)
	a.snapshot.InitialChecks.Completed = 120
	a.snapshot.Rows[100].Status = "Needs attention"
	a.query, a.priority = "hides the failed streamer", "Low priority"
	tt.Frame()
	if !tt.HasText("Initial checks complete · 1 streamer needs attention") {
		t.Fatal("startup failure left the loading state active", tt.Texts())
	}
	if _, ok := tt.Find("Checking monitored streamers"); ok {
		t.Fatal("startup failure kept a spinner active")
	}
	if err := tt.Click("View streamers"); err != nil {
		t.Fatal(err)
	}
	if a.query != "" || a.priority != "All priorities" || a.status != "Needs attention" || !tt.HasText("streamer_100") {
		t.Fatal("the issue action did not reveal the affected streamer")
	}
	a.snapshot.Rows[100].Status = "Offline"
	a.snapshot.Listener.ReadyTargets, a.snapshot.Listener.State = 119, "partial"
	tt.Frame()
	if !tt.HasText("119 / 120 subscriptions ready") || tt.HasText("Monitoring ready") {
		t.Fatal("partial subscriptions were reported as ready")
	}
	if err := tt.Click("View logs"); err != nil {
		t.Fatal(err)
	}
	if a.page != "logs" {
		t.Fatal("listener problems did not open Logs")
	}
}

func TestStartupProgressUsesSavedNativeSettingsAndSkipsEmptyQueue(t *testing.T) {
	a := startupFixture(t)
	a.cfg.NativeListening = false // An unsaved draft does not change monitoring.
	tt := ui.NewTester(a.view, 1050, 650)
	if !tt.HasText("Checking streamers…") {
		t.Fatal("an unsaved setting hid the running startup checks")
	}
	for _, mode := range []string{"polling", "missing session", "empty queue"} {
		cfg := a.store.Config()
		cfg.NativeListening, cfg.Cookie = true, "synthetic-cookie"
		a.snapshot.InitialChecks = monitor.CheckProgress{Total: 120}
		switch mode {
		case "polling":
			cfg.NativeListening = false
		case "missing session":
			cfg.Cookie = ""
		case "empty queue":
			a.snapshot.InitialChecks = monitor.CheckProgress{}
		}
		if err := a.store.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if _, ok := tt.Find("Monitoring startup"); ok {
			t.Fatal("inactive monitoring displayed startup progress", mode)
		}
	}
}

func TestAddedStreamersDoNotShowStartupProgress(t *testing.T) {
	a := startupFixture(t)
	tt := ui.NewTester(a.view, 1050, 650)
	added := monitor.Row{Streamer: state.Streamer{ID: "added_after_launch"}, Status: "Checking"}
	a.snapshot.Rows = append(a.snapshot.Rows, added)
	tt.Frame()
	if !tt.HasText("37 / 120 checked") || tt.HasText("37 / 121 checked") {
		t.Fatal("adding a streamer expanded the launch batch")
	}
	a.snapshot.InitialChecks.Completed = 120
	tt.Frame()
	if !tt.HasText("Monitoring ready") {
		t.Fatal("an added streamer's check prevented startup completion")
	}
	tt.Advance(4 * time.Second)
	a.snapshot.Rows = append(a.snapshot.Rows, monitor.Row{Streamer: state.Streamer{ID: "another_added_streamer"}, Status: "Checking"})
	a.snapshot.Listener.State, a.snapshot.Listener.ReadyTargets = "connecting", 0
	tt.Frame()
	if _, ok := tt.Find("Monitoring startup"); ok {
		t.Fatal("adding a streamer after startup brought the strip back")
	}
	// Starting with an empty list and then adding the first streamer is also
	// a normal row check, rather than a launch batch.
	a = accountFixture(t)
	a.cfg.Cookie = "synthetic-cookie"
	if err := a.store.SaveConfig(a.cfg); err != nil {
		t.Fatal(err)
	}
	a.snapshot.Rows = []monitor.Row{added}
	tt = ui.NewTester(a.view, 1050, 650)
	if _, ok := tt.Find("Monitoring startup"); ok {
		t.Fatal("adding the first streamer showed launch progress")
	}
}
