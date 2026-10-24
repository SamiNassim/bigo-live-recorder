// Package monitor coordinates one rate-limited check queue and shared native
// subscriptions. Network and recorder work never run on the window thread.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/bigolistener"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/recording"
	"bigo-live-recorder/internal/state"
)

type Row struct {
	Streamer                        state.Streamer
	Status                          string
	Error                           string
	LastCheck, NextCheck, StartedAt time.Time
	Path                            string
}
type Snapshot struct {
	Rows              []Row
	Listener          bigolistener.RuntimeStatus
	Recording, Paused int
}
type Event struct{ Name, StreamerID, Path string }

type streamClient interface {
	Configure(string, time.Duration)
	Check(context.Context, string) (bigo.Stream, error)
	Presence(context.Context, string) (bigo.Stream, error)
}
type activeRecording interface {
	File() string
	Started() time.Time
	Stop()
	Done() <-chan struct{}
	Result() recording.Result
}
type startRecording func(context.Context, state.Streamer, string, state.Config) (activeRecording, error)

type item struct {
	row               Row
	initial           bool
	pending           bool
	retryUntil        time.Time
	lastSignal        time.Time
	holdRoom          string
	handle            activeRecording
	unsubscribe       func()
	subUID            uint32
	bootstrapFailures int
}

type Engine struct {
	store  *state.Store
	log    *logging.Log
	client streamClient
	start  startRecording
	hub    *bigolistener.Hub
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	items  map[string]*item
	wake   chan struct{}
	events chan Event
	wg     sync.WaitGroup
}

func New(store *state.Store, log *logging.Log) *Engine {
	return newEngine(store, log, bigo.NewClient(), func(ctx context.Context, s state.Streamer, source string, c state.Config) (activeRecording, error) {
		return recording.Start(ctx, s, source, c)
	})
}

func newEngine(store *state.Store, log *logging.Log, client streamClient, start startRecording) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: store, log: log, client: client, start: start, ctx: ctx, cancel: cancel, items: make(map[string]*item), wake: make(chan struct{}, 1), events: make(chan Event, 64)}
	e.recoverHistory()
	e.hub = bigolistener.NewHub(ctx, func(s string) { log.Write("INFO", "listener", s) }, func(s string) { log.Write("WARN", "listener", s) })
	e.configure(store.Config())
	e.wg.Add(1)
	go e.run()
	return e
}
func (e *Engine) Events() <-chan Event { return e.events }
func (e *Engine) wakeUp() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) configure(c state.Config) {
	e.client.Configure(c.Cookie, time.Duration(c.RequestDelaySeconds)*time.Second)
	e.hub.Configure(c.NativeListening, c.NativeSession)
}

func (e *Engine) SaveConfig(c state.Config) error {
	var err error
	c.Cookie, err = bigo.ParseCookie(c.Cookie)
	if err != nil {
		return err
	}
	if c.NativeListening && strings.TrimSpace(c.NativeSession) != "" {
		if _, err = bigolistener.ParseSessionJSON(c.NativeSession); err != nil {
			return fmt.Errorf("native session: %w", err)
		}
	}
	if err = e.store.SaveConfig(c); err != nil {
		return err
	}
	e.configure(c)
	e.mu.Lock()
	for _, it := range e.items {
		it.initial = false
		it.bootstrapFailures = 0
		it.retryUntil = time.Time{}
		it.pending = false
		it.row.NextCheck = time.Now()
		it.row.Error = ""
	}
	e.mu.Unlock()
	e.wakeUp()
	e.log.Write("INFO", "", "Settings saved")
	return nil
}

func (e *Engine) Add(id, name string) error {
	id = strings.TrimSpace(id)
	if !bigo.ValidID(id) {
		return errors.New("enter a BIGO ID or alias, without @ or a URL")
	}
	err := e.store.Update(func(d *state.Data) error {
		if slices.ContainsFunc(d.Streamers, func(s state.Streamer) bool { return strings.EqualFold(s.ID, id) }) {
			return errors.New("this streamer is already in the list")
		}
		d.Streamers = append(d.Streamers, state.Streamer{ID: id, CustomName: strings.TrimSpace(name), AddedAt: time.Now()})
		return nil
	})
	if err == nil {
		e.log.Write("INFO", id, "Streamer added")
		e.wakeUp()
	}
	return err
}
func (e *Engine) Edit(id string, change func(*state.Streamer)) error {
	err := e.store.Update(func(d *state.Data) error {
		for i := range d.Streamers {
			if d.Streamers[i].ID == id {
				change(&d.Streamers[i])
				return nil
			}
		}
		return errors.New("streamer no longer exists")
	})
	e.wakeUp()
	return err
}
func (e *Engine) Remove(id string) error {
	err := e.store.Update(func(d *state.Data) error {
		d.Streamers = slices.DeleteFunc(d.Streamers, func(s state.Streamer) bool { return s.ID == id })
		return nil
	})
	e.wakeUp()
	return err
}
func (e *Engine) Stop(id string) {
	e.mu.Lock()
	if it := e.items[id]; it != nil && it.handle != nil {
		it.row.Status = "Finalizing"
		it.handle.Stop()
	}
	e.mu.Unlock()
}
func (e *Engine) Snapshot() Snapshot {
	s := Snapshot{Listener: e.hub.Status()}
	cfg := e.store.Config()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range e.items {
		s.Rows = append(s.Rows, it.row)
		if it.handle != nil {
			s.Recording++
		}
		if it.row.Streamer.Paused {
			s.Paused++
		}
	}
	if cfg.NativeListening {
		s.Listener.TotalTargets = len(s.Rows) - s.Paused
		if s.Listener.Connected && s.Listener.ReadyTargets < s.Listener.TotalTargets {
			s.Listener.State = "partial"
			s.Listener.Message = fmt.Sprintf("Native connection active; %d/%d subscriptions ready. Some targets are still resolving or need attention.", s.Listener.ReadyTargets, s.Listener.TotalTargets)
		}
	}
	return s
}
func (e *Engine) Close() {
	e.cancel()
	e.hub.Close()
	e.wg.Wait()
}

func (e *Engine) run() {
	defer e.wg.Done()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		e.mu.Lock()
		var handles []activeRecording
		for _, it := range e.items {
			if it.unsubscribe != nil {
				it.unsubscribe()
			}
			if it.handle != nil {
				it.handle.Stop()
				handles = append(handles, it.handle)
			}
		}
		e.mu.Unlock()
		for _, h := range handles {
			<-h.Done()
		}
		e.mu.Lock()
		for _, it := range e.items {
			if it.handle != nil {
				e.completed(it)
			}
		}
		e.mu.Unlock()
	}()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-tick.C:
		case <-e.wake:
		}
		e.reconcile()
		for {
			id := e.next()
			if id == "" || e.ctx.Err() != nil {
				break
			}
			e.check(id)
			e.reconcile()
		}
	}
}

func (e *Engine) reconcile() {
	cfg, data := e.store.Config(), e.store.Data()
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := make(map[string]bool, len(data.Streamers))
	for _, s := range data.Streamers {
		seen[s.ID] = true
		it := e.items[s.ID]
		if it == nil {
			it = &item{row: Row{Status: "Waiting", NextCheck: time.Now()}}
			e.items[s.ID] = it
		}
		it.row.Streamer = s
		if it.handle != nil {
			select {
			case <-it.handle.Done():
				e.completed(it)
			default:
			}
		}
		if s.Paused {
			if it.handle != nil {
				it.handle.Stop()
				it.row.Status = "Finalizing"
			} else {
				it.row.Status = "Paused"
			}
			if it.unsubscribe != nil {
				it.unsubscribe()
				it.unsubscribe = nil
				it.subUID = 0
			}
			continue
		}
		if !cfg.NativeListening && it.unsubscribe != nil {
			it.unsubscribe()
			it.unsubscribe = nil
			it.subUID = 0
		}
		if cfg.NativeListening && s.UID != 0 && it.subUID != s.UID {
			if it.unsubscribe != nil {
				it.unsubscribe()
			}
			ch, off, err := e.hub.Subscribe(s.ID, s.UID)
			if err == nil {
				watchCtx, stop := context.WithCancel(e.ctx)
				it.unsubscribe = func() { off(); stop() }
				it.subUID = s.UID
				go e.listen(watchCtx, s.ID, ch)
			}
		}
		if it.row.Status == "Paused" {
			it.row.Status = "Waiting"
			it.initial = false
			it.bootstrapFailures = 0
			it.holdRoom = ""
			it.row.NextCheck = time.Now()
		}
		if cfg.NativeListening && !cfg.PollingBackup && strings.TrimSpace(cfg.NativeSession) == "" && it.handle == nil {
			it.row.Status = "Setup needed"
			it.row.NextCheck = time.Time{}
		}
		if cfg.NativeListening && !cfg.PollingBackup && it.initial && !it.pending && it.handle == nil {
			it.row.NextCheck = time.Time{}
		}
	}
	for id, it := range e.items {
		if !seen[id] {
			if it.unsubscribe != nil {
				it.unsubscribe()
				it.unsubscribe = nil
			}
			if it.handle != nil {
				it.handle.Stop()
				select {
				case <-it.handle.Done():
					e.completed(it)
				default:
					continue
				}
			}
			delete(e.items, id)
		}
	}
}

func (e *Engine) next() string {
	cfg := e.store.Config()
	// A selected-but-unconfigured native listener is not permission to poll.
	canBootstrap := !cfg.NativeListening || cfg.PollingBackup || strings.TrimSpace(cfg.NativeSession) != ""
	if !canBootstrap {
		return ""
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var chosen *item
	var id string
	for key, it := range e.items {
		if it.row.Streamer.Paused || it.handle != nil {
			continue
		}
		if !eligible(cfg, it.initial, it.pending) {
			continue
		}
		if it.row.NextCheck.IsZero() {
			it.row.NextCheck = now
		}
		if it.row.NextCheck.After(now) {
			continue
		}
		if chosen == nil || it.row.NextCheck.Before(chosen.row.NextCheck) || (it.row.NextCheck.Equal(chosen.row.NextCheck) && key < id) {
			chosen = it
			id = key
		}
	}
	return id
}

func eligible(c state.Config, initial, pending bool) bool {
	return !initial || pending || !c.NativeListening || c.PollingBackup
}

func (e *Engine) listen(ctx context.Context, id string, ch <-chan bigolistener.Notification) {
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-ch:
			if !ok {
				return
			}
			e.mu.Lock()
			it := e.items[id]
			if it != nil && it.handle == nil && !it.row.Streamer.Paused && time.Since(it.lastSignal) > 30*time.Second {
				it.lastSignal = time.Now()
				it.pending = true
				it.retryUntil = time.Now().Add(2 * time.Minute)
				it.row.NextCheck = time.Now()
				if n.Attributed {
					e.log.Write("INFO", id, "Native live signal received; verifying playback")
				} else {
					e.log.Write("DEBUG", id, "Unattributed native activity; verifying this subscription (not a confirmed live start)")
				}
			}
			e.mu.Unlock()
			e.wakeUp()
		}
	}
}

func (e *Engine) check(id string) {
	e.mu.Lock()
	it := e.items[id]
	if it == nil {
		e.mu.Unlock()
		return
	}
	it.row.Status = "Checking"
	needsUID := it.row.Streamer.UID == 0
	bootstrap := !it.initial
	e.mu.Unlock()
	stream, err := e.client.Check(e.ctx, id)
	if stream.UID == 0 && needsUID && e.store.Config().NativeListening {
		if presence, presenceErr := e.client.Presence(e.ctx, id); presenceErr == nil {
			stream.UID = presence.UID
		} else if err == nil {
			err = presenceErr
		}
	}
	if e.ctx.Err() != nil {
		return
	}
	cfg := e.store.Config()
	e.mu.Lock()
	defer e.mu.Unlock()
	it = e.items[id]
	if it == nil {
		return
	}
	it.initial = true
	it.row.LastCheck = time.Now()
	it.row.NextCheck = time.Now().Add(interval(cfg, it.row.Streamer.LowPriority))
	if stream.UID != 0 {
		it.row.Streamer.UID = stream.UID
		if stream.Name != "" {
			it.row.Streamer.Name = stream.Name
		}
		if stream.Avatar != "" {
			it.row.Streamer.Avatar = stream.Avatar
		}
		updated := it.row.Streamer
		e.persist(func(d *state.Data) {
			for i := range d.Streamers {
				if d.Streamers[i].ID == id {
					d.Streamers[i].UID = updated.UID
					d.Streamers[i].Name = updated.Name
					d.Streamers[i].Avatar = updated.Avatar
				}
			}
		})
	}
	if err == nil && cfg.NativeListening && it.row.Streamer.UID == 0 {
		err = fmt.Errorf("BIGO did not resolve a native UID for %s", id)
	}
	if err != nil {
		if it.row.Error != err.Error() {
			e.log.Write("WARN", id, err.Error())
		}
		it.row.Error = err.Error()
		it.row.Status = "Needs attention"
		if bootstrap && !errors.Is(err, bigo.ErrLoginRequired) && !errors.Is(err, bigo.ErrURLWithheld) {
			it.bootstrapFailures++
			if it.bootstrapFailures < 3 {
				it.initial = false
				it.row.NextCheck = time.Now().Add(time.Duration(it.bootstrapFailures) * 30 * time.Second)
			}
		}
		if errors.Is(err, bigo.ErrURLWithheld) && it.retryUntil.IsZero() {
			it.pending = true
			it.retryUntil = time.Now().Add(2 * time.Minute)
		}
		// Retry only a bounded live-signal resolution, not perpetual offline polling.
		it.pending = it.pending && time.Now().Before(it.retryUntil)
		if it.pending {
			it.row.NextCheck = time.Now().Add(15 * time.Second)
		}
		return
	}
	it.row.Error = ""
	it.bootstrapFailures = 0
	if stream.Alive != 1 {
		it.row.Status = "Offline"
		it.holdRoom = ""
		it.pending = false
		it.retryUntil = time.Time{}
		return
	}
	if it.holdRoom != "" && (stream.RoomID == "" || stream.RoomID == it.holdRoom) {
		it.row.Status = "Stopped"
		it.pending = false
		return
	}
	// A pause/remove made during the request takes precedence over its result.
	current := e.store.Data()
	at := slices.IndexFunc(current.Streamers, func(s state.Streamer) bool { return s.ID == id })
	if at < 0 || current.Streamers[at].Paused {
		return
	}
	it.row.Status = "Starting"
	streamer := it.row.Streamer
	e.mu.Unlock()
	h, startErr := e.start(e.ctx, streamer, stream.URL(), cfg)
	e.mu.Lock()
	if startErr != nil {
		it.row.Status = "Needs attention"
		it.row.Error = startErr.Error()
		e.log.Write("ERROR", id, startErr.Error())
		if it.retryUntil.IsZero() {
			it.retryUntil = time.Now().Add(2 * time.Minute)
			it.pending = true
		}
		it.pending = it.pending && time.Now().Before(it.retryUntil)
		it.row.NextCheck = time.Now().Add(15 * time.Second)
		return
	}
	it.handle = h
	it.holdRoom = stream.RoomID
	if it.holdRoom == "" {
		it.holdRoom = "unknown"
	}
	it.pending = false
	it.retryUntil = time.Time{}
	it.row.Status = "Recording"
	it.row.StartedAt = h.Started()
	it.row.Path = h.File()
	it.row.NextCheck = time.Time{}
	it.row.Streamer.LastRecordedAt = h.Started()
	e.persist(func(d *state.Data) {
		for i := range d.Streamers {
			if d.Streamers[i].ID == id {
				d.Streamers[i].LastRecordedAt = h.Started()
				if cfg.AutoPromote {
					d.Streamers[i].LowPriority = false
				}
			}
		}
		d.Recordings = append(d.Recordings, state.Recording{ID: fmt.Sprintf("%d-%s", h.Started().UnixNano(), id), StreamerID: id, Name: it.row.Streamer.DisplayName(), Path: h.File(), StartedAt: h.Started()})
	})
	e.log.Write("INFO", id, "Recording started: "+h.File())
	current = e.store.Data()
	at = slices.IndexFunc(current.Streamers, func(s state.Streamer) bool { return s.ID == id })
	if at < 0 || current.Streamers[at].Paused {
		h.Stop()
		it.row.Status = "Finalizing"
		return
	}
	select {
	case e.events <- Event{it.row.Streamer.DisplayName(), id, h.File()}:
	default:
	}
}

func (e *Engine) completed(it *item) {
	r := it.handle.Result()
	e.persist(func(d *state.Data) {
		for i := range d.Recordings {
			if d.Recordings[i].Path == it.handle.File() {
				d.Recordings[i].Path = r.Path
				d.Recordings[i].EndedAt = r.EndedAt
				d.Recordings[i].Bytes = r.Bytes
				if r.Err != nil {
					d.Recordings[i].Error = r.Err.Error()
				}
			}
		}
	})
	if r.Err != nil {
		it.row.Error = r.Err.Error()
		e.log.Write("WARN", it.row.Streamer.ID, "Recording ended: "+r.Err.Error())
	} else {
		e.log.Write("INFO", it.row.Streamer.ID, fmt.Sprintf("Recording finalized (%d bytes)", r.Bytes))
	}
	// Natural termination may be transient. A manual stop retains the room hold.
	if it.row.Status != "Finalizing" {
		it.holdRoom = ""
	}
	it.handle = nil
	it.row.StartedAt = time.Time{}
	it.row.Path = ""
	it.row.Status = "Waiting"
	it.row.NextCheck = time.Now().Add(5 * time.Second)
	// One end-of-recording verification can recover an interrupted HLS session.
	it.initial = false
}

func (e *Engine) persist(change func(*state.Data)) {
	if err := e.store.Update(func(d *state.Data) error { change(d); return nil }); err != nil {
		e.log.Write("ERROR", "storage", err.Error())
	}
}

func (e *Engine) recoverHistory() {
	data := e.store.Data()
	if !slices.ContainsFunc(data.Recordings, func(r state.Recording) bool { return r.EndedAt.IsZero() }) {
		return
	}
	e.persist(func(d *state.Data) {
		for i := range d.Recordings {
			r := &d.Recordings[i]
			if !r.EndedAt.IsZero() {
				continue
			}
			r.EndedAt = time.Now()
			r.Error = "Application stopped before recording finalization was confirmed; existing file preserved"
			if info, err := os.Stat(r.Path); err == nil {
				r.Bytes = info.Size()
				r.EndedAt = info.ModTime()
			}
		}
	})
	e.log.Write("WARN", "recordings", "Recovered interrupted recording history; existing media files were preserved")
}

func interval(c state.Config, low bool) time.Duration {
	seconds := c.PollSeconds
	if low {
		seconds = c.ColdSeconds
	}
	return time.Duration(seconds) * time.Second
}

// EffectiveIntervals explains the serial queue floor; a request's actual
// latency may increase these estimates. Active recordings are excluded.
func EffectiveIntervals(c state.Config, eligible int) (normal, cold time.Duration) {
	sweep := time.Duration(eligible*c.RequestDelaySeconds) * time.Second
	return max(time.Duration(c.PollSeconds)*time.Second, sweep), max(time.Duration(c.ColdSeconds)*time.Second, sweep)
}
