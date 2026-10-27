// Package app implements the desktop interface entirely with mygo NativeUI.
package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bigo-live-recorder/internal/assets"
	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

const Name = "BIGO Live Recorder"

var Version = "dev"

type application struct {
	store  *state.Store
	log    *logging.Log
	engine *monitor.Engine
	window *mygo.Window
	ctx    context.Context
	cancel context.CancelFunc

	logo          *ui.Bitmap
	avatars       map[string]*ui.Bitmap
	avatarLoading map[string]bool
	cfg           state.Config
	snapshot      monitor.Snapshot
	data          state.Data
	logs          []logging.Entry

	page                            string
	query, status, priority, order  string
	logQuery, level                 string
	streamList, logList, recordList ui.ListState
	settingsScroll                  ui.ScrollState
	accountSettingsRequested        bool
	clearHistoryOpen                bool
	toasts                          []string

	addOpen, renameOpen, removeOpen              bool
	addID, addName                               string
	renameID, renameName, removeID               string
	message, accountMessage                      string
	account                                      *bigo.Account
	accountCookie                                string
	accountError                                 error
	busy, validating                             bool
	ffmpeg, preview                              bool
	ffmpegOpen, ffmpegInstalling, ffmpegChecking bool
	ffmpegProgress, ffmpegError                  string
	ffmpegCancel                                 context.CancelFunc

	normalSeconds, coldSeconds, delaySeconds float64
	cookieEditedAt                           time.Time
	cookieLastChecked                        string
}

func newApplication(store *state.Store, log *logging.Log) *application {
	ctx, cancel := context.WithCancel(context.Background())
	a := &application{store: store, log: log, ctx: ctx, cancel: cancel, cfg: store.Config(), data: store.Data(), page: "streamers", status: "All statuses", priority: "All priorities", order: "Last recorded", level: "All levels"}
	a.logo, _ = ui.DecodeBitmap(assets.Icon)
	a.avatars = make(map[string]*ui.Bitmap)
	a.avatarLoading = make(map[string]bool)
	a.loadNumbers()
	_, err := recording.FindFFmpeg(a.cfg.FFmpegPath)
	a.ffmpeg = err == nil
	return a
}
func (a *application) loadNumbers() {
	a.normalSeconds = float64(a.cfg.PollSeconds)
	a.coldSeconds = float64(a.cfg.ColdSeconds)
	a.delaySeconds = float64(a.cfg.RequestDelaySeconds)
}
func (a *application) update(fn func()) {
	if a.window != nil {
		a.window.Update(fn)
	} else {
		fn()
	}
}
func (a *application) refresh() {
	if a.engine != nil {
		a.snapshot = a.engine.Snapshot()
	}
	a.data = a.store.Data()
	a.logs = a.log.Entries()
	a.loadAvatars()
}
func (a *application) work(fn func() error) {
	if a.busy {
		return
	}
	a.busy = true
	a.message = ""
	go func() {
		err := fn()
		a.update(func() {
			a.busy = false
			if err != nil {
				a.message = err.Error()
			}
			a.refresh()
		})
	}()
}
func (a *application) openFolder(path string) {
	go func() {
		err := os.MkdirAll(path, 0755)
		if err == nil {
			err = mygo.Shell.OpenPath(path)
		}
		if err != nil {
			a.update(func() { a.message = err.Error() })
		}
	}()
}

func Run(dir string, preview bool) error {
	store, err := state.Open(dir)
	if err != nil {
		return err
	}
	log, err := logging.Open(dir)
	if err != nil {
		return err
	}
	mygo.App.SetName(Name)
	if packagedVersion := mygo.App.Version(); packagedVersion != "" {
		Version = packagedVersion
	}
	mygo.App.SetVersion(Version)
	if !mygo.App.RequestSingleInstanceLock() {
		log.Close()
		return nil
	}
	a := newApplication(store, log)
	mygo.App.SetWindowStateStore(windowStore{store})
	a.preview = preview
	if preview {
		a.seedPreview()
	} else {
		a.engine = monitor.New(store, log)
	}
	a.refresh()
	var stopping sync.Once
	var finished atomic.Bool
	closed := make(chan struct{})
	shutdown := func() {
		stopping.Do(func() {
			go func() {
				defer close(closed)
				a.cancel()
				if a.engine != nil {
					a.engine.Close()
				}
				log.Close()
				finished.Store(true)
				mygo.App.Quit()
			}()
		})
	}
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		if !finished.Load() {
			e.PreventDefault()
			shutdown()
		}
	})
	mygo.App.OnWindowAllClosed(func() { mygo.App.Quit() })
	mygo.App.OnSecondInstance(func(_ []string, _ string) {
		if a.window != nil {
			a.window.Show()
			a.window.Focus()
		}
	})
	mygo.App.WhenReady(func() {
		mygo.Theme.SetSource(mygo.ThemeSource(a.cfg.Theme))
		a.window = mygo.NewWindow(mygo.WindowOptions{Title: Name, StateKey: "main", Width: 1400, Height: 900, MinWidth: 1050, MinHeight: 650, Content: ui.View(a.view), BackgroundColor: "light-dark(#eef1f5, #14171d)"})
		_ = a.window.SetIcon(assets.Icon)
		mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
			{Role: mygo.RoleAppMenu},
			{Label: "File", Submenu: []*mygo.MenuItem{
				{Label: "Add streamer…", Accelerator: "CmdOrCtrl+N", Click: func(_ *mygo.MenuItem, _ *mygo.Window) { a.addOpen = true; a.window.Invalidate() }},
				{Label: "Open recordings", Accelerator: "CmdOrCtrl+O", Click: func(_ *mygo.MenuItem, _ *mygo.Window) { a.openFolder(a.cfg.RecordingPath) }},
				{Label: "Settings…", Accelerator: "CmdOrCtrl+,", Click: func(_ *mygo.MenuItem, _ *mygo.Window) { a.page = "settings"; a.window.Invalidate() }},
				mygo.Separator(), {Role: mygo.RoleQuit},
			}},
			{Role: mygo.RoleEditMenu}, {Role: mygo.RoleWindowMenu},
		}))
		go a.updates()
		if !a.preview {
			a.checkFFmpeg(true)
		}
	})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			mygo.App.Quit()
		case <-a.ctx.Done():
		}
	}()
	err = mygo.App.Run()
	shutdown()
	<-closed
	return err
}

func (a *application) updates() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	maintenance := time.NewTicker(5 * time.Second)
	defer maintenance.Stop()
	var events <-chan monitor.Event
	if a.engine != nil {
		events = a.engine.Events()
	}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-maintenance.C:
			if !a.preview {
				if _, err := a.store.PruneMissingRecordings(); err != nil {
					a.log.Write("WARN", "recordings", "Reconcile deleted recordings: "+err.Error())
				}
			}
		case <-tick.C:
			a.update(a.refresh)
		case event := <-events:
			a.update(func() {
				a.refresh()
				if a.store.Config().Notifications && mygo.NotificationsSupported() {
					n := mygo.NewNotification(mygo.NotificationOptions{Title: "Recording started", Body: event.Name + " is live. Your recording has started.", Group: "recordings"})
					n.OnClick(func() { a.window.Show(); a.window.Focus(); a.page = "streamers"; a.window.Invalidate() })
					if err := n.Show(); err != nil {
						a.log.Write("WARN", "notifications", err.Error())
					}
				}
			})
		}
	}
}

func (a *application) view(c *ui.Context) {
	dark := c.Theme().Dark
	if a.cfg.Theme != "system" {
		dark = a.cfg.Theme == "dark"
	}
	c.SetTheme(palette(dark))
	a.autoValidateCookie(c)
	t := c.Theme()
	if a.message != "" {
		c.AddToast(ui.Toast{Title: a.message, Type: "error", Timeout: 6 * time.Second})
		a.message = ""
	}
	for _, message := range a.toasts {
		c.AddToast(ui.Toast{Title: message, Type: "success"})
	}
	a.toasts = nil
	ui.Row(c).Fill().AlignItems(ui.Stretch).Children(func() {
		a.sidebar(c)
		ui.Column(c).Grow(1).MinWidth(0).Padding(28).Gap(22).Children(func() {
			if a.preview {
				ui.Text(c, "DESIGN PREVIEW · monitoring is disabled").FontSize(11).TextColor(t.Warning)
			}
			switch a.page {
			case "recordings":
				a.recordings(c)
			case "logs":
				a.logsPage(c)
			case "settings":
				a.settings(c)
			default:
				a.streamers(c)
			}
		})
	})
	a.dialogs(c)
}

func (a *application) sidebar(c *ui.Context) {
	t := c.Theme()
	background := ui.Hex("#181b22")
	if !t.Dark {
		background = ui.Hex("#e7ebf1")
	}
	ui.Column(c).Width(220).Shrink(0).Padding(20, 14).Gap(24).Background(background).Border(1, t.Border).Children(func() {
		ui.Column(c).Padding(14, 10, 20, 10).Gap(14).Children(func() {
			if a.logo != nil {
				ui.Image(c, a.logo).Size(66, 66)
			}
			ui.Text(c, "BIGO LIVE").FontSize(18).FontWeight(700)
			ui.Text(c, "RECORDER").FontSize(11).FontWeight(600).TextColor(t.TextMuted)
		})
		ui.Sidebar(c, &a.page, func() {
			for _, nav := range []struct{ id, label string }{{"streamers", "Streamers"}, {"recordings", "Recordings"}, {"logs", "Logs"}, {"settings", "Settings"}} {
				navigationItem(c, nav.id, nav.label, a.page == nav.id)
			}
		}).Width(192).Grow(1).Background(background)
		ui.Column(c).Padding(14, 10).Gap(12).Children(func() {
			ui.Divider(c)
			a.sessionIndicator(c)
			ffmpeg := "FFmpeg not found"
			color := t.Warning
			if a.ffmpeg {
				ffmpeg = "FFmpeg ready"
				color = t.Success
			}
			ui.Text(c, "●  "+ffmpeg).FontSize(12).TextColor(color)
			listener := "Native listener: " + a.snapshot.Listener.State
			listenerColor := t.TextMuted
			switch a.snapshot.Listener.State {
			case "connecting", "retrying", "partial", "unavailable":
				listenerColor = t.Warning
			case "connected":
				listenerColor = t.Success
			case "error":
				listenerColor = t.Danger
			}
			if !a.store.Config().NativeListening {
				listener = "Queued polling"
				listenerColor = t.TextMuted
			}
			ui.Row(c).AlignItems(ui.Center).Gap(7).Children(func() {
				ui.Icon(c, connectionIcon).Size(15, 15).TextColor(listenerColor)
				ui.Text(c, listener).FontSize(11).TextColor(listenerColor).MaxLines(2).Grow(1)
			})
			ui.Text(c, "v"+Version).FontSize(10).TextColor(t.TextMuted)
		})
	})
}

func header(c *ui.Context, title, subtitle string, action func()) {
	ui.Row(c).AlignItems(ui.Center).Gap(16).FillWidth().Children(func() {
		ui.Column(c).Grow(1).Gap(7).Children(func() {
			ui.Text(c, title).FontSize(30).FontWeight(700)
			ui.Text(c, subtitle).TextColor(c.Theme().TextMuted).FontSize(12)
		})
		if action != nil {
			action()
		}
	})
}

func (a *application) validateCookie() {
	if a.validating {
		return
	}
	raw := a.cfg.Cookie
	a.cookieLastChecked = raw
	cookie, err := bigo.ParseCookie(raw)
	a.account = nil
	a.accountCookie = ""
	a.accountError = err
	if err != nil || cookie == "" {
		a.accountMessage = "Paste the uniqid cookie first."
		if err != nil {
			a.accountMessage = err.Error()
		}
		return
	}
	a.validating = true
	a.accountMessage = "Validating account…"
	go func() {
		client := bigo.NewClient()
		client.Configure(cookie, time.Second)
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		defer cancel()
		account, err := client.Account(ctx)
		a.update(func() {
			a.validating = false
			if a.cfg.Cookie != raw {
				a.accountMessage = "Cookie changed; validate again."
				return
			}
			if err != nil {
				a.accountError = err
				a.accountMessage = err.Error()
			} else {
				a.account = &account
				a.accountCookie = cookie
				a.accountError = nil
				a.accountMessage = "Authenticated as " + account.Name + " · BIGO ID " + account.ID
			}
		})
	}()
}

func (a *application) cookieChanged() {
	a.cookieEditedAt = time.Now()
	a.cookieLastChecked = ""
	a.account = nil
	a.accountCookie = ""
	a.accountError = nil
	a.accountMessage = ""
}

func (a *application) autoValidateCookie(c *ui.Context) {
	if a.preview || a.validating || strings.TrimSpace(a.cfg.Cookie) == "" || a.cookieLastChecked == a.cfg.Cookie {
		return
	}
	if remaining := 800*time.Millisecond - time.Since(a.cookieEditedAt); remaining > 0 {
		c.After(remaining)
		return
	}
	a.validateCookie()
}

func (a *application) choosePath(directory bool, destination *string) {
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: a.window, Title: "Choose a path", Directory: directory, CreateDirectories: directory})
		a.update(func() {
			if err != nil {
				a.message = err.Error()
			} else if len(paths) > 0 {
				*destination = paths[0]
				if destination == &a.cfg.FFmpegPath {
					a.ffmpeg = false
					a.checkFFmpeg(false)
				}
			}
		})
	}()
}

func (a *application) seedPreview() {
	a.snapshot.Listener.State = "connected"
	a.snapshot.Listener.Connected = true
	a.snapshot.Listener.ReadyTargets = 6
	a.snapshot.Listener.TotalTargets = 6
	a.snapshot.Listener.Message = "Shared native listener connected · 6 subscriptions ready"
	for i, name := range []string{"Luna", "Maya", "Nora", "Clara", "Sofia", "Elena"} {
		s := state.Streamer{ID: fmt.Sprintf("100120%d", i), Name: name, AddedAt: time.Now().Add(-24 * time.Hour), LastRecordedAt: time.Now().Add(-time.Duration(i) * time.Hour)}
		r := monitor.Row{Streamer: s, Status: "Offline", LastCheck: time.Now().Add(-time.Minute), LiveCheckedAt: time.Now().Add(-time.Minute)}
		if i < 2 {
			r.Status = "Recording"
			r.Live = true
			r.StartedAt = time.Now().Add(-time.Duration(14+i*11) * time.Minute)
			a.snapshot.Recording++
		}
		if i == 4 {
			r.Status = "Monitoring paused"
			r.Streamer.Paused = true
			a.snapshot.MonitoringPaused++
		}
		if i == 3 {
			r.Status = "Live"
			r.Live = true
			r.Streamer.RecordingPaused = true
			a.snapshot.RecordingPaused++
		}
		if i == 5 {
			r.Streamer.LowPriority = true
		}
		a.snapshot.Rows = append(a.snapshot.Rows, r)
	}
	a.snapshot.QueueTargets = len(a.snapshot.Rows) - a.snapshot.MonitoringPaused - a.snapshot.Recording
}

// Keep file-system operations outside view-building callbacks.
func (a *application) reveal(path string) {
	if path != "" {
		mygo.Shell.ShowItemInFolder(filepath.Clean(path))
	}
}
func (a *application) paste(target *string) {
	*target = strings.TrimSpace(mygo.Clipboard.ReadText())
}

type windowStore struct{ store *state.Store }

func (w windowStore) LoadWindowStates() (map[string]mygo.WindowState, error) {
	return map[string]mygo.WindowState{"main": mygo.WindowState(w.store.Config().Window)}, nil
}
func (w windowStore) SaveWindowStates(states map[string]mygo.WindowState) error {
	return w.store.SaveWindow(state.Window(states["main"]))
}
