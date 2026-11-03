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
	"bigo-live-recorder/internal/fault"
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
	Live                            bool
	LiveCheckedAt                   time.Time
	RoomKind                        bigo.RoomKind
}
type Snapshot struct {
	Rows                                         []Row
	Listener                                     bigolistener.RuntimeStatus
	Recording, MonitoringPaused, RecordingPaused int
	QueueTargets                                 int
}
type Event struct{ Name, StreamerID, Path string }

type streamClient interface {
	Configure(string, time.Duration)
	Check(context.Context, string) (bigo.Stream, error)
	Presence(context.Context, string) (bigo.Stream, error)
	Messaging(context.Context, string) (bigo.MessagingCredentials, error)
}

type nativeStreamClient interface {
	OriginStream(context.Context, bigo.Stream, *bigolistener.Hub) (bigo.Stream, error)
}

type activeRecording interface {
	File() string
	Started() time.Time
	Stop()
	Done() <-chan struct{}
	Result() recording.Result
}
type startRecording func(context.Context, state.Streamer, bigo.Stream, state.Config) (activeRecording, error)

type item struct {
	row               Row
	initial           bool
	pending           bool
	retryUntil        time.Time
	lastSignal        time.Time
	signalVersion     uint64
	lastAttributed    time.Time
	handle            activeRecording
	recordCancel      context.CancelFunc
	unsubscribe       func()
	subUID            uint32
	bootstrapFailures int
	config            state.Config
	audioRoom         string
	recordNotified    bool
	broadcastID       string
}

type Engine struct {
	verifyingBroadcasts map[string]bool
	verificationSlots   chan struct{}
	store               *state.Store
	log                 *logging.Log
	client              streamClient
	start               startRecording
	hub                 *bigolistener.Hub
	ctx                 context.Context
	cancel              context.CancelFunc
	mu                  sync.Mutex
	items               map[string]*item
	wake                chan struct{}
	events              chan Event
	wg                  sync.WaitGroup
	processing          chan struct{}
	process             func(context.Context, string, state.Config) (string, error)
	replayMu            sync.Mutex
	replayJobs          map[string]bool
	replaySlot          chan struct{}
	replay              func(context.Context, state.Broadcast, state.Config) (string, error)
}

func New(store *state.Store, log *logging.Log) *Engine {
	return newEngine(store, log, bigo.NewClient(), nil)
}

func newEngine(store *state.Store, log *logging.Log, client streamClient, start startRecording) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: store, log: log, client: client, start: start, ctx: ctx, cancel: cancel, items: make(map[string]*item), wake: make(chan struct{}, 1), events: make(chan Event, 64), processing: make(chan struct{}, 2), process: recording.Process,
		replayJobs: make(map[string]bool), replaySlot: make(chan struct{}, 1), replay: recording.Replay}
	e.recoverHistory()
	e.hub = bigolistener.NewHub(ctx, e.messagingSession, func(s string) { log.Write("INFO", "listener", s) }, func(s string) { log.Write("WARN", "listener", s) }, func(s string, err error) { log.WriteError("WARN", "listener", s, err) })
	e.configure(store.Config())
	if e.start == nil {
		e.start = e.startRecording
	}
	e.wg.Add(1)
	go e.run()
	e.resumeReplays()
	return e
}

func (e *Engine) startRecording(ctx context.Context, streamer state.Streamer, source bigo.Stream, cfg state.Config) (activeRecording, error) {
	stream, err := e.playbackSource(ctx, streamer, source, cfg, nil)
	if err != nil {
		if errors.Is(err, bigo.ErrRoomEnded) {
			e.finishRoom(streamer.ID, source.RoomID)
		}
		return nil, err
	}
	recovery := bigo.StreamRecovery{
		// Recovery is called only after a media error or stall, never while
		// recording advances normally. Always resolve the current room anew.
		Refresh: func(ctx context.Context) (bigo.Stream, error) {
			return e.refreshPlayback(ctx, streamer, cfg)
		},
		Report:      func(level, message string) { e.log.Write(level, streamer.ID, message) },
		ReportError: func(level, message string, err error) { e.log.WriteError(level, streamer.ID, message, err) },
	}
	if e.wantsNative(streamer, cfg) {
		recovery.Origin = func(ctx context.Context, current bigo.Stream) (bigo.Stream, error) {
			return e.client.(nativeStreamClient).OriginStream(ctx, current, e.hub)
		}
	}
	if native, ok := e.client.(nativeStreamClient); ok && cfg.NativeListening {
		recovery.Room = func(ctx context.Context, current bigo.Stream) (bigo.Stream, error) {
			current.Alive = 1
			return native.OriginStream(ctx, current, e.hub)
		}
	}
	options, err := e.broadcastOptions(ctx, streamer, stream, cfg)
	if err != nil {
		return nil, err
	}
	return recording.Start(ctx, streamer, stream, cfg, recovery, options)
}

func (e *Engine) refreshPlayback(ctx context.Context, streamer state.Streamer, cfg state.Config) (bigo.Stream, error) {
	if err := ctx.Err(); err != nil {
		return bigo.Stream{}, err
	}
	source, err := e.playbackInfo(ctx, streamer, cfg)
	return e.playbackSource(ctx, streamer, source, cfg, err)
}

// Web playback can report an app-only broadcast offline. Resolve its native
// room from presence only when Origin is selected and web room data is absent.
// Transport and authentication failures keep their existing retry behavior.
func (e *Engine) playbackInfo(ctx context.Context, streamer state.Streamer, cfg state.Config) (bigo.Stream, error) {
	if err := ctx.Err(); err != nil {
		return bigo.Stream{}, err
	}
	source, err := e.client.Check(ctx, streamer.ID)
	appOnly := cfg.NativeListening && source.RoomStatus == 4
	if (!e.wantsNative(streamer, cfg) && !appOnly) || (err != nil && !errors.Is(err, bigo.ErrURLWithheld)) ||
		(source.Alive == 1 && source.RoomID != "" && source.RoomID != "0") {
		return source, err
	}
	presence, presenceErr := e.client.Presence(ctx, streamer.ID)
	if presenceErr != nil {
		return source, errors.Join(err, presenceErr)
	}
	if source.UID != 0 && presence.UID != 0 && source.UID != presence.UID {
		return source, errors.New("BIGO returned conflicting streamer identities")
	}
	if source.UID == 0 {
		source.UID = presence.UID
	}
	if source.Name == "" {
		source.Name = presence.Name
	}
	if source.Avatar == "" {
		source.Avatar = presence.Avatar
	}
	if presence.Alive == 1 {
		source.Alive = 1
		source.RoomStatus = presence.RoomStatus
		if presence.RoomID != "" && presence.RoomID != "0" {
			source.RoomID = presence.RoomID
		}
	}
	return source, err
}

func (e *Engine) playbackSource(ctx context.Context, streamer state.Streamer, source bigo.Stream, cfg state.Config, webErr error) (bigo.Stream, error) {
	if err := ctx.Err(); err != nil {
		return bigo.Stream{}, err
	}
	native, supported := e.client.(nativeStreamClient)
	appOnly := cfg.NativeListening && source.RoomStatus == 4
	if (!e.wantsNative(streamer, cfg) && !appOnly) || !supported || source.Alive != 1 {
		return source, webErr
	}
	if source.UID == 0 {
		source.UID = streamer.UID
	}
	stream, err := native.OriginStream(ctx, source, e.hub)
	if canceled := ctx.Err(); canceled != nil {
		return bigo.Stream{}, canceled
	}
	if err == nil && !stream.Playable() && stream.RoomKind != bigo.RoomAudioMultiGuest {
		err = bigo.ErrOriginUnavailable
	}
	if err == nil {
		return stream, nil
	}
	if webErr != nil || source.URL() == "" {
		return source, errors.Join(webErr, err)
	}
	e.log.WriteError("WARN", streamer.ID, "Native media source unavailable: "+fault.Reason(err)+". Trying the web stream instead.", err)
	return source, nil
}

func (e *Engine) wantsNative(streamer state.Streamer, cfg state.Config) bool {
	quality := streamer.Quality
	if quality == "" {
		quality = cfg.StreamQuality
	}
	_, supported := e.client.(nativeStreamClient)
	return cfg.NativeListening && (quality == "" || quality == "origin") && supported
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
	e.hub.Configure(c.NativeListening, c.Cookie, c.NativeDeviceID)
}

func (e *Engine) messagingSession(ctx context.Context, deviceID string) (bigolistener.Session, error) {
	credentials, err := e.client.Messaging(ctx, deviceID)
	if err != nil {
		return bigolistener.Session{}, err
	}
	return bigolistener.Session{
		UID: credentials.UID, Username: credentials.Username, Cookie: credentials.Cookie,
		DeviceID: credentials.DeviceID, LinkdAddresses: bigolistener.LinkdAddresses(credentials.Addresses),
	}, nil
}

func (e *Engine) SaveConfig(c state.Config) error {
	var err error
	c.Cookie, err = bigo.ParseCookie(c.Cookie)
	if err != nil {
		return err
	}
	if c.PostProcess {
		if _, err = recording.OutputArgs(c.FFmpegArgs); err != nil {
			return err
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
	var err error
	id, err = bigo.ParseID(id)
	if err != nil {
		return err
	}
	err = e.store.Update(func(d *state.Data) error {
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
	var before, after state.Streamer
	err := e.store.Update(func(d *state.Data) error {
		for i := range d.Streamers {
			if d.Streamers[i].ID == id {
				before = d.Streamers[i]
				change(&d.Streamers[i])
				after = d.Streamers[i]
				return nil
			}
		}
		return errors.New("streamer no longer exists")
	})
	if err == nil && (before.Paused != after.Paused || before.RecordingPaused != after.RecordingPaused) {
		e.mu.Lock()
		if it := e.items[id]; it != nil {
			it.row.Streamer.Paused = after.Paused
			it.row.Streamer.RecordingPaused = after.RecordingPaused
			it.initial = false
			it.pending = false
			it.bootstrapFailures = 0
			it.retryUntil = time.Time{}
			it.row.Error = ""
			it.row.NextCheck = time.Now()
			if after.Paused || after.RecordingPaused {
				stopRecording(it)
			}
			if it.handle == nil {
				it.row.Status = "Waiting"
				if after.Paused {
					it.row.Status = "Monitoring paused"
					it.row.NextCheck = time.Time{}
				}
			}
		}
		e.mu.Unlock()
	}
	e.wakeUp()
	return err
}

// SetRecordingPaused changes only automatic recording, not the subscription or
// polling preference. Resuming schedules a fresh check, even in native-only mode.
func (e *Engine) SetRecordingPaused(id string, paused bool) error {
	if err := e.Edit(id, func(s *state.Streamer) { s.RecordingPaused = paused }); err != nil {
		return err
	}
	message := "Recording enabled"
	if paused {
		message = "Recording paused"
	}
	e.log.Write("INFO", id, message)
	return nil
}

func (e *Engine) SetMonitoringPaused(id string, paused bool) error {
	if err := e.Edit(id, func(s *state.Streamer) { s.Paused = paused }); err != nil {
		return err
	}
	message := "Monitoring resumed"
	if paused {
		message = "Monitoring paused"
	}
	e.log.Write("INFO", id, message)
	return nil
}

func stopRecording(it *item) {
	if it.handle != nil {
		it.row.Status = "Finalizing"
		it.handle.Stop()
	} else if it.recordCancel != nil {
		it.recordCancel()
	}
}

func (e *Engine) Remove(id string) error {
	err := e.store.Update(func(d *state.Data) error {
		d.Streamers = slices.DeleteFunc(d.Streamers, func(s state.Streamer) bool { return s.ID == id })
		return nil
	})
	if err == nil {
		e.mu.Lock()
		if it := e.items[id]; it != nil {
			stopRecording(it)
		}
		e.mu.Unlock()
	}
	e.wakeUp()
	return err
}
func (e *Engine) Snapshot() Snapshot {
	s, _ := e.snapshot(nil)
	return s
}

// SnapshotIfChanged reuses an immutable snapshot while all rows and summary
// fields match. Map iteration order cannot cause a spurious change.
func (e *Engine) SnapshotIfChanged(previous Snapshot) (Snapshot, bool) {
	return e.snapshot(&previous)
}

func (e *Engine) snapshot(previous *Snapshot) (Snapshot, bool) {
	var listener bigolistener.RuntimeStatus
	if e.hub != nil {
		listener = e.hub.Status()
	}
	s := Snapshot{Listener: listener}
	cfg := e.store.Config()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range e.items {
		if it.handle != nil && it.row.Status == "Recording" {
			s.Recording++
		}
		if !it.row.Streamer.Paused && it.handle == nil {
			s.QueueTargets++
		}
		if it.row.Streamer.Paused {
			s.MonitoringPaused++
		}
		if it.row.Streamer.RecordingPaused {
			s.RecordingPaused++
		}
	}
	if cfg.NativeListening {
		s.Listener.TotalTargets = len(e.items) - s.MonitoringPaused
		if s.Listener.Connected && s.Listener.ReadyTargets < s.Listener.TotalTargets {
			s.Listener.State = "partial"
			s.Listener.Message = fmt.Sprintf("Native connection active; %d/%d subscriptions ready. Some targets are still resolving or need attention.", s.Listener.ReadyTargets, s.Listener.TotalTargets)
		}
	}
	if previous != nil && s.Listener == previous.Listener &&
		s.Recording == previous.Recording && s.QueueTargets == previous.QueueTargets &&
		s.MonitoringPaused == previous.MonitoringPaused && s.RecordingPaused == previous.RecordingPaused &&
		len(previous.Rows) == len(e.items) {
		unchanged := true
		for _, row := range previous.Rows {
			if it := e.items[row.Streamer.ID]; it == nil || it.row != row {
				unchanged = false
				break
			}
		}
		if unchanged {
			return *previous, false
		}
	}
	s.Rows = make([]Row, 0, len(e.items))
	for _, it := range e.items {
		s.Rows = append(s.Rows, it.row)
	}
	return s, true
}
func (e *Engine) Close() {
	e.cancel()
	if e.hub != nil {
		e.hub.Close()
	}
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
			e.fileChanges(it)
			select {
			case <-it.handle.Done():
				e.completed(it)
			default:
			}
		}
		if s.Paused {
			if it.handle != nil {
				stopRecording(it)
			} else {
				it.row.Status = "Monitoring paused"
			}
			if it.unsubscribe != nil {
				it.unsubscribe()
				it.unsubscribe = nil
				it.subUID = 0
			}
			continue
		}
		if s.RecordingPaused && it.handle != nil {
			stopRecording(it)
		}
		if !cfg.NativeListening && it.unsubscribe != nil {
			it.unsubscribe()
			it.unsubscribe = nil
			it.subUID = 0
		}
		if cfg.NativeListening {
			e.subscribeLocked(it)
		}
		if it.row.Status == "Monitoring paused" {
			it.row.Status = "Waiting"
			it.initial = false
			it.bootstrapFailures = 0
			it.row.NextCheck = time.Now()
		}
		if cfg.NativeListening && !cfg.PollingBackup && strings.TrimSpace(cfg.Cookie) == "" && it.handle == nil {
			it.row.Status = "Setup needed"
			it.row.NextCheck = time.Time{}
		}
		if cfg.NativeListening && !cfg.PollingBackup && it.initial && !it.pending && it.handle == nil {
			it.row.NextCheck = time.Time{}
		}
	}
	for id, it := range e.items {
		if !seen[id] {
			if it.handle == nil && it.recordCancel != nil {
				it.recordCancel()
			}
			if it.unsubscribe != nil {
				it.unsubscribe()
				it.unsubscribe = nil
			}
			if it.handle != nil {
				stopRecording(it)
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

// subscribeLocked is shared by restored monitors and newly resolved accounts.
// Registering the first target before source selection lets the one shared
// connection authenticate while a native media lookup waits for it.
func (e *Engine) subscribeLocked(it *item) {
	s := it.row.Streamer
	if e.hub == nil || s.UID == 0 || s.Paused || it.subUID == s.UID {
		return
	}
	if it.unsubscribe != nil {
		it.unsubscribe()
		it.unsubscribe, it.subUID = nil, 0
	}
	ch, off, err := e.hub.Subscribe(s.ID, s.UID)
	if err != nil {
		return
	}
	watchCtx, stop := context.WithCancel(e.ctx)
	it.unsubscribe = func() { off(); stop() }
	it.subUID = s.UID
	go e.listen(watchCtx, s.ID, ch)
}

func (e *Engine) next() string {
	cfg := e.store.Config()
	// A selected-but-unconfigured native listener is not permission to poll.
	canBootstrap := !cfg.NativeListening || cfg.PollingBackup || strings.TrimSpace(cfg.Cookie) != ""
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
			if it != nil && (it.handle == nil || it.row.Streamer.RecordingPaused) && !it.row.Streamer.Paused &&
				(time.Since(it.lastSignal) > 30*time.Second || n.Attributed && time.Since(it.lastAttributed) > 30*time.Second) {
				it.lastSignal = time.Now()
				it.signalVersion++
				if n.Attributed {
					it.lastAttributed = it.lastSignal
				}
				it.pending = true
				it.retryUntil = time.Now().Add(2 * time.Minute)
				it.row.NextCheck = time.Now()
				if n.Attributed {
					message := "Native live signal received; verifying playback"
					if it.row.Streamer.RecordingPaused {
						message = "Native live signal received; checking presence (recording paused)"
					}
					e.log.Write("INFO", id, message)
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
	if it == nil || it.handle != nil || it.row.Streamer.Paused {
		e.mu.Unlock()
		return
	}
	it.row.Status = "Checking"
	needsUID := it.row.Streamer.UID == 0
	bootstrap := !it.initial
	presenceOnly := it.row.Streamer.RecordingPaused
	target := it.row.Streamer
	signalAtStart := it.signalVersion
	e.mu.Unlock()
	var stream bigo.Stream
	var err error
	if presenceOnly {
		stream, err = e.client.Presence(e.ctx, id)
	} else {
		stream, err = e.playbackInfo(e.ctx, target, e.store.Config())
	}
	if !presenceOnly && stream.UID == 0 && needsUID && e.store.Config().NativeListening {
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
	if err == nil || errors.Is(err, bigo.ErrURLWithheld) {
		stream = e.inspectRoom(e.ctx, id, stream, cfg)
		e.verifyPreviousBroadcasts(e.ctx, id, stream, cfg)
	}
	if e.ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	it = e.items[id]
	if it == nil {
		return
	}
	current := e.store.Data()
	at := slices.IndexFunc(current.Streamers, func(s state.Streamer) bool { return s.ID == id })
	if at < 0 || current.Streamers[at].Paused {
		it.row.Status = "Monitoring paused"
		it.row.NextCheck = time.Time{}
		return
	}
	it.row.Streamer.RecordingPaused = current.Streamers[at].RecordingPaused
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
	if cfg.NativeListening {
		e.subscribeLocked(it)
	}
	// A withheld web URL does not prevent native playback. Keep all other API
	// failures on their existing retry path, including missing authentication.
	if errors.Is(err, bigo.ErrURLWithheld) && stream.Alive == 1 &&
		(stream.RoomKind == bigo.RoomAudioMultiGuest || (!it.row.Streamer.RecordingPaused && (e.wantsNative(it.row.Streamer, cfg) || cfg.NativeListening && stream.RoomStatus == 4))) {
		err = nil
	}
	if err == nil || stream.Alive == 1 {
		it.row.Live = stream.Alive == 1
		it.row.LiveCheckedAt = time.Now()
	}
	if err != nil {
		previousError := it.row.Error
		it.row.Error = "Could not check this stream: " + fault.Reason(err)
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
		if previousError != it.row.Error {
			e.log.WriteError("WARN", id, it.row.Error+". "+retryMessage(cfg, it), err)
		}
		return
	}
	it.row.Error = ""
	it.bootstrapFailures = 0
	it.row.RoomKind = stream.RoomKind
	if stream.Alive != 1 {
		it.row.RoomKind = bigo.RoomUnknown
		it.audioRoom = ""
		it.row.Status = "Offline"
		if it.pending && it.signalVersion != signalAtStart {
			// A newer signal arrived during this request. Its resolution
			// must not be cleared by the older offline response.
			it.row.NextCheck = time.Now()
		} else {
			it.pending = false
			it.retryUntil = time.Time{}
		}
		return
	}
	if stream.RoomKind == bigo.RoomAudioMultiGuest && it.row.Streamer.RecordingPaused {
		e.audioRoomLocked(it, stream.RoomID)
		return
	}
	if it.row.Streamer.RecordingPaused {
		if stream.RoomStatus != 4 {
			it.audioRoom = ""
		}
		it.row.Status = "Live"
		it.pending = false
		it.retryUntil = time.Time{}
		return
	}
	// Recording was resumed while a presence-only request was in flight. Resolve
	// playback now rather than trying to record a response with no media URL.
	if presenceOnly {
		it.initial = false
		it.row.Status = "Waiting"
		it.row.NextCheck = time.Now()
		return
	}
	it.row.Status = "Starting"
	streamer := it.row.Streamer
	recordCtx, recordCancel := context.WithCancel(e.ctx)
	it.recordCancel = recordCancel
	e.mu.Unlock()
	h, startErr := e.start(recordCtx, streamer, stream, cfg)
	e.mu.Lock()
	if startErr != nil {
		canceled := recordCtx.Err() != nil
		recordCancel()
		it.recordCancel = nil
		if canceled {
			it.initial = false
			it.row.Status = "Waiting"
			it.row.NextCheck = time.Now()
			return
		}
		if errors.Is(startErr, bigo.ErrRoomEnded) {
			it.row.Live = false
			it.row.RoomKind = bigo.RoomUnknown
			it.row.Status = "Offline"
			it.row.Error = ""
			it.pending = false
			it.retryUntil = time.Time{}
			return
		}
		it.row.Status = "Needs attention"
		it.row.Error = "Could not start recording: " + fault.Reason(startErr)
		if it.retryUntil.IsZero() {
			it.retryUntil = time.Now().Add(2 * time.Minute)
			it.pending = true
		}
		it.pending = it.pending && time.Now().Before(it.retryUntil)
		it.row.NextCheck = time.Now().Add(15 * time.Second)
		level := "WARN"
		if fault.RequiresAction(startErr) {
			level = "ERROR"
		}
		e.log.WriteError(level, id, it.row.Error+". "+retryMessage(cfg, it), startErr)
		return
	}
	it.handle = h
	it.recordNotified = false
	it.broadcastID = ""
	if source, ok := h.(interface{ Broadcast() state.Broadcast }); ok {
		it.broadcastID = source.Broadcast().ID
		it.recordNotified = e.broadcastNotified(it.broadcastID)
	}
	if source, ok := h.(interface{ RoomKind() bigo.RoomKind }); ok {
		it.row.RoomKind = source.RoomKind()
	}
	it.audioRoom = ""
	it.config = cfg
	it.pending = false
	it.retryUntil = time.Time{}
	it.row.Status = "Recording"
	it.row.StartedAt = h.Started()
	it.row.Path = h.File()
	it.row.NextCheck = time.Time{}
	if h.File() == "" {
		it.row.Status = "Waiting for guest media"
		current = e.store.Data()
		at = slices.IndexFunc(current.Streamers, func(s state.Streamer) bool { return s.ID == id })
		if at < 0 || current.Streamers[at].Paused || current.Streamers[at].RecordingPaused || recordCtx.Err() != nil {
			h.Stop()
			it.row.Status = "Finalizing"
		}
		return
	}
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
		d.Recordings = append(d.Recordings, state.Recording{ID: fmt.Sprintf("%d-%s", h.Started().UnixNano(), id), StreamerID: id, Name: it.row.Streamer.DisplayName(), Path: h.File(), StartedAt: h.Started(), BroadcastID: it.broadcastID})
	})
	current = e.store.Data()
	at = slices.IndexFunc(current.Streamers, func(s state.Streamer) bool { return s.ID == id })
	if at < 0 || current.Streamers[at].Paused || current.Streamers[at].RecordingPaused || recordCtx.Err() != nil {
		h.Stop()
		it.row.Status = "Finalizing"
		return
	}
	e.notifyRecording(it, h.File())
}

func (e *Engine) completed(it *item) {
	e.fileChanges(it)
	r := it.handle.Result()
	alreadyFinalized := slices.ContainsFunc(e.store.Data().Recordings, func(saved state.Recording) bool { return saved.Path == r.Path && !saved.EndedAt.IsZero() })
	e.persist(func(d *state.Data) {
		finishBroadcastFile(d, r.Path, r)
	})
	if r.Err != nil {
		it.row.Error = fault.Reason(r.Err)
		level, message := recordingFailure(r, false)
		if e.ctx.Err() == nil && !it.row.Streamer.Paused {
			message += " Monitoring will continue."
		}
		e.log.WriteError(level, it.row.Streamer.ID, message, r.Err)
	} else if it.broadcastID == "" && !alreadyFinalized && r.Path != "" {
		e.log.Write("INFO", it.row.Streamer.ID, fmt.Sprintf("Recording finalized (%d bytes)", r.Bytes))
	}
	if r.RoomEnded && it.broadcastID != "" {
		e.finishBroadcast(it.broadcastID)
	} else if r.RoomModeChanged && it.broadcastID != "" {
		e.log.Write("INFO", it.row.Streamer.ID, "BIGO changed the room mode. Guest files are finalized; creating a replay of the captured media while monitoring continues.")
		if err := e.QueueReplay(it.broadcastID); err != nil && e.ctx.Err() == nil {
			e.log.WriteError("WARN", it.row.Streamer.ID, "Could not queue the room replay; original recordings are preserved.", err)
		}
	} else if it.broadcastID != "" {
		for _, b := range e.store.Data().Broadcasts {
			if b.ID == it.broadcastID && b.ReplayPending {
				e.QueueReplay(b.ID)
			}
		}
	}
	if !alreadyFinalized && it.config.PostProcess && r.Err == nil && r.Bytes > 0 && e.ctx.Err() == nil {
		e.processCopy(it.row.Streamer.ID, r.Path, it.config)
	}
	immediate := it.row.Status == "Finalizing" || it.row.Streamer.RecordingPaused || it.row.Streamer.Paused
	if it.recordCancel != nil {
		it.recordCancel()
		it.recordCancel = nil
	}
	it.handle = nil
	it.broadcastID = ""
	it.row.StartedAt = time.Time{}
	it.row.Path = ""
	it.row.Status = "Waiting"
	it.row.NextCheck = time.Now().Add(5 * time.Second)
	if immediate {
		it.row.NextCheck = time.Now()
	}
	// One end-of-recording verification can recover an interrupted HLS session.
	it.initial = false
}

func (e *Engine) fileChanges(it *item) {
	files, ok := it.handle.(interface{ DrainFileChanges() []recording.FileChange })
	if !ok {
		return
	}
	changes := files.DrainFileChanges()
	if len(changes) == 0 {
		return
	}
	id := it.row.Streamer.ID
	e.persist(func(d *state.Data) {
		for _, change := range changes {
			if change.Result != nil {
				finishBroadcastFile(d, change.Path, *change.Result)
				continue
			}
			if !slices.ContainsFunc(d.Recordings, func(r state.Recording) bool { return r.Path == change.Path }) {
				d.Recordings = append(d.Recordings, state.Recording{ID: fmt.Sprintf("%d-%s", change.StartedAt.UnixNano(), id), StreamerID: id, Name: it.row.Streamer.DisplayName(), Path: change.Path, StartedAt: change.StartedAt, BroadcastID: change.BroadcastID})
			}
			for i := range d.Recordings {
				if d.Recordings[i].Path == change.Path {
					saved := &d.Recordings[i]
					saved.GuestUID, saved.GuestID, saved.GuestName = change.Guest.UID, change.Guest.ID, change.Guest.Name
					saved.GuestIdentityVersion = change.Guest.IdentityVersion
					if change.Guest.Name != "" {
						saved.Name = it.row.Streamer.DisplayName() + " · " + change.Guest.ID + " (" + change.Guest.Name + ")"
					}
				}
			}
			for i := range d.Streamers {
				if d.Streamers[i].ID == id {
					d.Streamers[i].LastRecordedAt = change.StartedAt
					if it.config.AutoPromote {
						d.Streamers[i].LowPriority = false
					}
				}
			}
		}
	})
	for _, change := range changes {
		if r := change.Result; r != nil {
			if r.Err != nil {
				level, message := recordingFailure(*r, true)
				e.log.WriteError(level, id, message, r.Err)
			} else {
				if it.broadcastID == "" {
					e.log.Write("INFO", id, fmt.Sprintf("Recording file finalized (%d bytes)", r.Bytes))
				}
				if it.config.PostProcess && r.Bytes > 0 && e.ctx.Err() == nil {
					e.processCopy(id, r.Path, it.config)
				}
			}
		} else {
			first := !it.recordNotified
			it.row.Path, it.row.StartedAt = change.Path, change.StartedAt
			it.row.Streamer.LastRecordedAt = change.StartedAt
			if it.row.Status == "Waiting for guest media" {
				it.row.Status = "Recording"
			}
			if first {
				e.notifyRecording(it, change.Path)
			} else if it.broadcastID == "" {
				e.log.Write("INFO", id, "Recording continued: "+change.Path)
			}
		}
	}
	if it.row.RoomKind == bigo.RoomAudioMultiGuest || it.row.RoomKind == bigo.RoomHybridMultiGuest {
		it.row.Path, it.row.StartedAt = it.handle.File(), it.handle.Started()
		if it.row.Status != "Finalizing" {
			it.row.Status = "Recording"
			if it.row.Path == "" {
				it.row.Status = "Waiting for guest media"
			}
		}
	}
}

func (e *Engine) persist(change func(*state.Data)) {
	if err := e.store.Update(func(d *state.Data) error { change(d); return nil }); err != nil {
		e.log.WriteError("ERROR", "storage", "Could not save streamer and recording history: "+fault.Reason(err)+". Recent changes may not survive an app restart. Check available disk space and folder permissions.", err)
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
