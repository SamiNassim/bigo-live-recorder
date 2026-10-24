package bigolistener

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hubRestartDebounce  = 250 * time.Millisecond
	hubMinRetryDelay    = 2 * time.Second
	hubMaxRetryDelay    = 30 * time.Second
	targetMinRetryDelay = 30 * time.Second
	targetMaxRetryDelay = 5 * time.Minute
)

var errHubReconfigured = errors.New("BIGO native listener configuration changed")

type watcher struct {
	publicID  string
	targetUID uint32
	events    chan Notification
}

// Notification tells a monitor why the shared Linkd listener woke it. BIGO's
// compact NewLive frame sometimes omits the target identity; those frames must
// be verified against the monitored accounts but must not be presented as a
// confirmed live-start notification for each account.
type Notification struct {
	Attributed bool
}

type target struct {
	publicID string
	uid      uint32
}

type targetRejection struct {
	err     error
	retryAt time.Time
	delay   time.Duration
}

// RuntimeStatus is a local snapshot of the native listener. Reading it never
// performs a BIGO API request or changes the connection.
type RuntimeStatus struct {
	State        string `json:"state"`
	Message      string `json:"message"`
	Connected    bool   `json:"connected"`
	ReadyTargets int    `json:"ready_targets"`
	TotalTargets int    `json:"total_targets"`
}

type hubSnapshot struct {
	version   uint64
	configKey string
	deviceID  string
	targets   []target
}

// SessionSource obtains a fresh messaging ticket on each connection attempt.
// It must honor cancellation; no ticket is periodically refreshed while connected.
type SessionSource func(context.Context, string) (Session, error)

type hubClient interface {
	Close() error
	addFollow(context.Context, uint32) error
	enableLiveNotice(context.Context, uint32) error
	waitLiveEvent(context.Context) (liveEvent, error)
}

type hubDialFunc func(context.Context, Session) (hubClient, string, error)

// Hub owns one shared authenticated Linkd connection for every monitored BIGO
// streamer. Parallel native logins using the same account can displace each
// other, so individual monitors subscribe to this shared connection.
type Hub struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu               sync.Mutex
	enabled          bool
	usable           bool
	configKey        string
	deviceID         string
	source           SessionSource
	watchers         map[uint64]*watcher
	nextWatcher      uint64
	version          uint64
	wake             chan struct{}
	connectionCancel context.CancelFunc
	waitCancel       context.CancelFunc
	lastError        string
	dial             hubDialFunc
	status           RuntimeStatus

	info    func(string)
	warning func(string)
	wg      sync.WaitGroup
}

// NewHub starts a dormant shared listener. Source is invoked only when a
// connection is needed; every monitored target uses that same connection.
func NewHub(parent context.Context, source SessionSource, info, warning func(string)) *Hub {
	ctx, cancel := context.WithCancel(parent)
	h := &Hub{
		ctx: ctx, cancel: cancel,
		source:   source,
		watchers: make(map[uint64]*watcher),
		wake:     make(chan struct{}, 1),
		info:     info,
		warning:  warning,
		status: RuntimeStatus{
			State:   "initializing",
			Message: "Initializing the BIGO native listener.",
		},
		dial: func(ctx context.Context, session Session) (hubClient, string, error) {
			return dialLinkd(ctx, session)
		},
	}
	h.wg.Add(1)
	go h.run()
	return h
}

// Close stops the shared connection and waits for its worker to exit.
func (h *Hub) Close() {
	h.cancel()
	h.mu.Lock()
	if h.connectionCancel != nil {
		h.connectionCancel()
	}
	if h.waitCancel != nil {
		h.waitCancel()
	}
	h.status = RuntimeStatus{State: "stopped", Message: "The BIGO native listener is stopped."}
	h.mu.Unlock()
	h.wg.Wait()
}

// Status returns the listener's current in-memory state. It is intentionally a
// cheap local read so the settings UI can refresh it without polling BIGO.
func (h *Hub) Status() RuntimeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Configure applies the current preference and cookie identity. It returns
// true when native listening is selected, even if the cookie is not currently
// usable. Native mode never silently falls back to API polling.
func (h *Hub) Configure(enabled bool, cookie, deviceID string) bool {
	hash := sha256.Sum256([]byte(cookie + "\x00" + deviceID))
	key := fmt.Sprintf("%t:%x", enabled, hash)

	h.mu.Lock()
	if h.configKey == key {
		selected := h.enabled
		h.mu.Unlock()
		return selected
	}
	h.mu.Unlock()

	var err error
	switch {
	case strings.TrimSpace(cookie) == "":
		err = errors.New("Paste your BIGO uniqid cookie in Settings to connect")
	case strings.TrimSpace(deviceID) == "":
		err = errors.New("BIGO listener device identity is missing")
	case h.source == nil:
		err = errors.New("BIGO listener credential source is unavailable")
	}

	h.mu.Lock()
	if h.configKey == key {
		result := h.enabled
		h.mu.Unlock()
		return result
	}
	h.configKey = key
	h.enabled = enabled
	h.usable = err == nil
	h.deviceID = deviceID
	h.version++
	switch {
	case !enabled:
		h.status = RuntimeStatus{State: "disabled", Message: "Native listening is disabled; regular polling is selected."}
	case err != nil:
		h.status = RuntimeStatus{State: "error", Message: err.Error()}
	case len(h.watchers) == 0:
		h.status = RuntimeStatus{State: "idle", Message: "Waiting for an active BIGO monitor."}
	default:
		h.status = RuntimeStatus{
			State:        "connecting",
			Message:      "Connecting to BIGO Linkd.",
			TotalTargets: h.uniqueTargetCountLocked(),
		}
	}
	h.signalConfigChangeLocked()
	h.mu.Unlock()
	if enabled && err != nil && h.warning != nil {
		h.warning("BIGO native listening is inactive: " + err.Error())
	}
	return enabled
}

// Subscribe registers a public BIGO id and numeric provider uid. The returned
// channel coalesces repeated notifications until the monitor consumes one.
func (h *Hub) Subscribe(publicID string, targetUID uint32) (<-chan Notification, func(), error) {
	publicID = strings.TrimSpace(publicID)
	if publicID == "" || targetUID == 0 {
		return nil, nil, fmt.Errorf("invalid BIGO listener target id=%q uid=%d", publicID, targetUID)
	}
	h.mu.Lock()
	h.nextWatcher++
	id := h.nextWatcher
	w := &watcher{publicID: publicID, targetUID: targetUID, events: make(chan Notification, 1)}
	h.watchers[id] = w
	h.version++
	if h.enabled && h.usable {
		h.status = RuntimeStatus{
			State:        "connecting",
			Message:      "Updating BIGO native subscriptions.",
			Connected:    h.status.Connected,
			ReadyTargets: h.status.ReadyTargets,
			TotalTargets: h.uniqueTargetCountLocked(),
		}
	}
	h.signalTargetChangeLocked()
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			if _, exists := h.watchers[id]; exists {
				delete(h.watchers, id)
				h.version++
				if h.enabled && h.usable && len(h.watchers) == 0 {
					h.status = RuntimeStatus{State: "idle", Message: "Waiting for an active BIGO monitor."}
				}
				h.signalTargetChangeLocked()
			}
			h.mu.Unlock()
		})
	}
	return w.events, unsubscribe, nil
}

func (h *Hub) uniqueTargetCountLocked() int {
	targets := make(map[uint32]struct{}, len(h.watchers))
	for _, watcher := range h.watchers {
		targets[watcher.targetUID] = struct{}{}
	}
	return len(targets)
}

func (h *Hub) signalConfigChangeLocked() {
	if h.connectionCancel != nil {
		h.connectionCancel()
	}
	if h.waitCancel != nil {
		h.waitCancel()
		h.waitCancel = nil
	}
	h.signalWakeLocked()
}

func (h *Hub) signalTargetChangeLocked() {
	// Target changes interrupt only the passive event wait. Setup commands are
	// allowed to finish so an applied follow is never blindly repeated after
	// its response was lost to cancellation.
	if h.waitCancel != nil {
		h.waitCancel()
		h.waitCancel = nil
	}
	h.signalWakeLocked()
}

func (h *Hub) signalWakeLocked() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) drainWake() {
	select {
	case <-h.wake:
	default:
	}
}

func (h *Hub) snapshot() (hubSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.enabled || !h.usable || len(h.watchers) == 0 {
		return hubSnapshot{}, false
	}
	byUID := make(map[uint32]string, len(h.watchers))
	for _, w := range h.watchers {
		byUID[w.targetUID] = w.publicID
	}
	targets := make([]target, 0, len(byUID))
	for uid, publicID := range byUID {
		targets = append(targets, target{publicID: publicID, uid: uid})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].uid < targets[j].uid })
	return hubSnapshot{version: h.version, configKey: h.configKey, deviceID: h.deviceID, targets: targets}, true
}

func (h *Hub) run() {
	defer h.wg.Done()
	retryDelay := hubMinRetryDelay
	confirmedKey := ""
	followed := make(map[uint32]bool)
	subscribed := make(map[uint32]bool)
	rejected := make(map[uint32]targetRejection)
	for {
		snapshot, ready := h.snapshot()
		if !ready {
			select {
			case <-h.ctx.Done():
				return
			case <-h.wake:
				continue
			}
		}

		// Restored monitors subscribe in a short burst; coalesce them into one
		// login instead of reconnecting once for every target.
		select {
		case <-h.ctx.Done():
			return
		case <-h.wake:
			continue
		case <-time.After(hubRestartDebounce):
		}
		snapshot, ready = h.snapshot()
		if !ready {
			continue
		}
		if snapshot.configKey != confirmedKey {
			confirmedKey = snapshot.configKey
			clear(followed)
			clear(subscribed)
			clear(rejected)
		}
		forgetInactiveTargets(snapshot.targets, followed, subscribed, rejected)
		h.setStatus(RuntimeStatus{
			State:        "connecting",
			Message:      "Connecting to BIGO Linkd.",
			TotalTargets: len(snapshot.targets),
		})

		connectionCtx, connectionCancel := context.WithCancel(h.ctx)
		h.mu.Lock()
		h.connectionCancel = connectionCancel
		h.mu.Unlock()
		// Close the small race between taking the debounced snapshot and making
		// the connection cancellation visible to Configure.
		latest, stillReady := h.snapshot()
		if !stillReady || latest.configKey != snapshot.configKey {
			connectionCancel()
			h.mu.Lock()
			h.connectionCancel = nil
			h.mu.Unlock()
			continue
		}
		snapshot = latest
		err := h.connectAndListen(connectionCtx, snapshot, followed, subscribed, rejected)
		connectionCancel()
		h.mu.Lock()
		h.connectionCancel = nil
		currentConfigKey := h.configKey
		h.mu.Unlock()

		if h.ctx.Err() != nil {
			return
		}
		if errors.Is(err, errHubReconfigured) || currentConfigKey != snapshot.configKey {
			retryDelay = hubMinRetryDelay
			continue
		}
		if err != nil {
			h.reportConnectionError(err)
		}
		select {
		case <-h.ctx.Done():
			return
		case <-h.wake:
			retryDelay = hubMinRetryDelay
		case <-time.After(retryDelay):
			if retryDelay < hubMaxRetryDelay {
				retryDelay *= 2
				if retryDelay > hubMaxRetryDelay {
					retryDelay = hubMaxRetryDelay
				}
			}
		}
	}
}

func forgetInactiveTargets(targets []target, followed, subscribed map[uint32]bool, rejected map[uint32]targetRejection) {
	active := make(map[uint32]bool, len(targets))
	for _, target := range targets {
		active[target.uid] = true
	}
	for uid := range followed {
		if !active[uid] {
			delete(followed, uid)
		}
	}
	for uid := range subscribed {
		if !active[uid] {
			delete(subscribed, uid)
		}
	}
	for uid := range rejected {
		if !active[uid] {
			delete(rejected, uid)
		}
	}
}

func (h *Hub) connectAndListen(
	ctx context.Context,
	snapshot hubSnapshot,
	followed map[uint32]bool,
	subscribed map[uint32]bool,
	rejected map[uint32]targetRejection,
) error {
	session, err := h.source(ctx, snapshot.deviceID)
	if err != nil {
		return fmt.Errorf("refresh BIGO messaging ticket: %w", err)
	}
	latest, ready := h.snapshot()
	if ctx.Err() != nil || !ready || latest.configKey != snapshot.configKey {
		return errHubReconfigured
	}
	client, endpoint, err := h.dial(ctx, session)
	if err != nil {
		return err
	}
	defer client.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	activeSnapshot, ready := h.snapshot()
	if !ready || activeSnapshot.configKey != snapshot.configKey {
		return errHubReconfigured
	}
	h.drainWake()
	announcedVersion := ^uint64(0)
	for {
		forgetInactiveTargets(activeSnapshot.targets, followed, subscribed, rejected)
		readyTargetCount, err := h.setupTargets(ctx, client, activeSnapshot.targets, followed, subscribed, rejected)
		if err != nil {
			return err
		}

		latest, ready := h.snapshot()
		if !ready || latest.configKey != snapshot.configKey {
			return errHubReconfigured
		}
		if latest.version != activeSnapshot.version {
			activeSnapshot = latest
			h.drainWake()
			continue
		}

		h.mu.Lock()
		h.lastError = ""
		h.mu.Unlock()
		if announcedVersion != activeSnapshot.version && h.info != nil {
			h.info(fmt.Sprintf("BIGO native listener active for %d/%d streamer(s) via %s (web cookie; one shared connection)", readyTargetCount, len(activeSnapshot.targets), endpoint))
			announcedVersion = activeSnapshot.version
		}
		h.setConnectedStatus(readyTargetCount, len(activeSnapshot.targets))

		var waitCtx context.Context
		var waitCancel context.CancelFunc
		if retryAt, ok := nextTargetRetry(rejected); ok {
			waitCtx, waitCancel = context.WithDeadline(ctx, retryAt)
		} else {
			waitCtx, waitCancel = context.WithCancel(ctx)
		}
		h.mu.Lock()
		h.waitCancel = waitCancel
		currentVersion := h.version
		currentConfigKey := h.configKey
		h.mu.Unlock()
		if currentVersion != activeSnapshot.version || currentConfigKey != snapshot.configKey {
			waitCancel()
			h.clearWaitCancel()
			latest, ready = h.snapshot()
			if !ready || latest.configKey != snapshot.configKey {
				return errHubReconfigured
			}
			activeSnapshot = latest
			h.drainWake()
			continue
		}

		event, err := client.waitLiveEvent(waitCtx)
		waitCancel()
		h.clearWaitCancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			latest, ready = h.snapshot()
			if ready && latest.configKey == snapshot.configKey && latest.version != activeSnapshot.version && errors.Is(err, context.Canceled) {
				activeSnapshot = latest
				h.drainWake()
				continue
			}
			return err
		}
		h.dispatch(event)
	}
}

func (h *Hub) setStatus(status RuntimeStatus) {
	h.mu.Lock()
	h.status = status
	h.mu.Unlock()
}

func (h *Hub) setConnectedStatus(ready, total int) {
	status := RuntimeStatus{Connected: true, ReadyTargets: ready, TotalTargets: total}
	switch {
	case total > 0 && ready == total:
		status.State = "connected"
		status.Message = fmt.Sprintf("Listening for live notifications for %d BIGO streamer(s).", total)
	case ready > 0:
		status.State = "partial"
		status.Message = fmt.Sprintf("Linkd is connected; %d of %d stream subscriptions are active. The remaining target(s) will retry natively.", ready, total)
	default:
		status.State = "unavailable"
		status.Message = "Linkd is connected, but no stream subscriptions are active. Targets will retry natively."
	}
	h.setStatus(status)
}

func (h *Hub) clearWaitCancel() {
	h.mu.Lock()
	h.waitCancel = nil
	h.mu.Unlock()
}

func (h *Hub) setupTargets(
	ctx context.Context,
	client hubClient,
	targets []target,
	followed map[uint32]bool,
	subscribed map[uint32]bool,
	rejected map[uint32]targetRejection,
) (int, error) {
	readyTargetCount := 0
	for _, target := range targets {
		if rejection, exists := rejected[target.uid]; exists {
			if time.Now().Before(rejection.retryAt) {
				continue
			}
		}
		if !followed[target.uid] {
			if err := client.addFollow(ctx, target.uid); err != nil {
				wrapped := fmt.Errorf("subscribe BIGO target %s: %w", target.publicID, err)
				if h.rejectTarget(target, wrapped, rejected) {
					continue
				}
				return readyTargetCount, wrapped
			}
			followed[target.uid] = true
		}
		if !subscribed[target.uid] {
			if err := client.enableLiveNotice(ctx, target.uid); err != nil {
				wrapped := fmt.Errorf("enable BIGO notification for %s: %w", target.publicID, err)
				if h.rejectTarget(target, wrapped, rejected) {
					continue
				}
				return readyTargetCount, wrapped
			}
			subscribed[target.uid] = true
		}
		delete(rejected, target.uid)
		readyTargetCount++
	}
	return readyTargetCount, nil
}

func (h *Hub) rejectTarget(target target, err error, rejected map[uint32]targetRejection) bool {
	if !errors.Is(err, errProtocolRejected) {
		return false
	}
	delay := targetMinRetryDelay
	if previous, exists := rejected[target.uid]; exists && previous.delay > 0 {
		delay = previous.delay * 2
		if delay > targetMaxRetryDelay {
			delay = targetMaxRetryDelay
		}
	}
	rejected[target.uid] = targetRejection{err: err, retryAt: time.Now().Add(delay), delay: delay}
	if h.warning != nil {
		h.warning(fmt.Sprintf("BIGO native subscription unavailable for %s; retrying natively in %s, and polling remains disabled while native mode is selected: %v", target.publicID, delay, err))
	}
	return true
}

func nextTargetRetry(rejected map[uint32]targetRejection) (time.Time, bool) {
	var earliest time.Time
	for _, rejection := range rejected {
		if earliest.IsZero() || rejection.retryAt.Before(earliest) {
			earliest = rejection.retryAt
		}
	}
	return earliest, !earliest.IsZero()
}

func (h *Hub) reportConnectionError(err error) {
	if err == nil || h.ctx.Err() != nil {
		return
	}
	message := err.Error()
	h.mu.Lock()
	repeated := message == h.lastError
	h.lastError = message
	h.status = RuntimeStatus{
		State:        "retrying",
		Message:      "Linkd disconnected; reconnecting: " + message,
		ReadyTargets: 0,
		TotalTargets: h.uniqueTargetCountLocked(),
	}
	h.mu.Unlock()
	if !repeated && h.warning != nil {
		h.warning("BIGO native listener disconnected; reconnecting: " + message)
	}
}

func (h *Hub) dispatch(event liveEvent) {
	payload := event.Content
	for key, value := range event.Fields {
		payload += "\n" + key + "=" + value
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	matched := make([]*watcher, 0, 1)
	for _, w := range h.watchers {
		if strings.Contains(payload, w.publicID) || strings.Contains(payload, strconv.FormatUint(uint64(w.targetUID), 10)) {
			matched = append(matched, w)
		}
	}
	// NewLive frames can be terse. An unattributed notification wakes all BIGO
	// targets for a single on-demand lookup, without re-enabling polling.
	if len(matched) == 0 {
		for _, w := range h.watchers {
			select {
			case w.events <- Notification{Attributed: false}:
			default:
			}
		}
		return
	}
	for _, w := range matched {
		select {
		case w.events <- Notification{Attributed: true}:
		default:
		}
	}
}
