package bigo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"bigo-live-recorder/internal/fault"
)

// Participant is one independently encoded source from a hybrid room.
type Participant struct {
	UID     uint32
	Capture Capture
}

// GuestRoom delivers independently encoded participants sharing one room's
// media connections. Each participant's capture owns its own file lifetimes.
type GuestRoom interface {
	Next(context.Context) (Participant, error)
	Close() error
}

type guestFrame struct {
	uid   uint32
	frame nativeMediaFrame
}

type guestRoom struct {
	parent    context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	recovery  StreamRecovery
	mu        sync.Mutex
	refreshMu sync.Mutex
	source    Stream
	err       error
	finished  sync.Once
	workers   sync.WaitGroup
	frames    chan guestFrame
	guests    chan Participant
	done      chan struct{}
	lastMedia atomic.Int64
	members   map[uint32]*guestMember // Owned by dispatch until done closes.
	epoch     time.Time
}

// OpenGuestRoom subscribes to original participant feeds using the room ticket
// already obtained through the shared native listener.
func OpenGuestRoom(parent context.Context, source Stream, recovery StreamRecovery) (GuestRoom, error) {
	if source.native == nil || (source.RoomKind != RoomHybridMultiGuest && source.RoomKind != RoomAudioMultiGuest) {
		return nil, ErrOriginUnavailable
	}
	ctx, cancel := context.WithCancel(parent)
	r := &guestRoom{parent: parent, ctx: ctx, cancel: cancel, recovery: recovery, source: source,
		frames: make(chan guestFrame, 256), guests: make(chan Participant, 64), done: make(chan struct{}), members: make(map[uint32]*guestMember), epoch: time.Now()}
	r.lastMedia.Store(time.Now().UnixNano())
	r.workers.Add(2)
	if source.RoomKind == RoomHybridMultiGuest && len(source.native.video) > 0 {
		r.workers.Add(1)
		go func() { defer r.workers.Done(); r.transport(true) }()
	}
	go func() { defer r.workers.Done(); r.transport(false) }()
	go func() { defer r.workers.Done(); r.watchMedia() }()
	go func() { r.workers.Wait(); close(r.frames) }()
	go r.dispatch()
	return r, nil
}

func (r *guestRoom) Next(ctx context.Context) (Participant, error) {
	select {
	case participant, ok := <-r.guests:
		if ok {
			return participant, nil
		}
		r.mu.Lock()
		err := r.err
		r.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return Participant{}, err
	case <-ctx.Done():
		return Participant{}, ctx.Err()
	}
}

func (r *guestRoom) Close() error {
	r.cancel()
	<-r.done
	for _, member := range r.members {
		member.capture.Close()
	}
	return nil
}

func (r *guestRoom) finish(err error) {
	r.finished.Do(func() {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		r.cancel()
	})
}

func (r *guestRoom) current() Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source
}

// Refresh is a response to failed media or a whole room without media. Healthy
// guest reception never performs presence checks or playback API requests.
func (r *guestRoom) refresh() (Stream, error) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	source := r.current()
	if r.recovery.Refresh == nil {
		return source, nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, 25*time.Second)
	defer cancel()
	// When another guest track is healthy, renew the room ticket directly.
	// Reconnecting an idle camera connection must not start web API polling.
	if r.recovery.Room != nil && time.Since(time.Unix(0, r.lastMedia.Load())) < time.Minute {
		fresh, err := r.recovery.Room(ctx, source)
		if err != nil {
			return Stream{}, err
		}
		if fresh.RoomKind != source.RoomKind {
			return fresh, ErrRoomModeChanged
		}
		if fresh.native == nil {
			return Stream{}, ErrOriginUnavailable
		}
		r.mu.Lock()
		r.source = fresh
		r.mu.Unlock()
		return fresh, nil
	}
	fresh, err := r.recovery.Refresh(ctx)
	if errors.Is(err, ErrRoomEnded) && r.recovery.Room != nil {
		fresh, err = r.recovery.Room(ctx, source)
	}
	if err != nil {
		return Stream{}, err
	}
	if fresh.Alive != 1 || fresh.RoomID != source.RoomID || fresh.RoomKind != source.RoomKind ||
		(fresh.UID != 0 && source.UID != 0 && fresh.UID != source.UID) {
		if r.recovery.Room != nil {
			fresh, err = r.recovery.Room(ctx, source)
			if err != nil {
				return Stream{}, err
			}
		} else if fresh.Alive != 1 || fresh.RoomID != source.RoomID {
			return fresh, ErrRoomEnded
		}
		if fresh.RoomKind != source.RoomKind {
			return fresh, ErrRoomModeChanged
		}
	}
	if fresh.native == nil {
		return Stream{}, ErrOriginUnavailable
	}
	r.mu.Lock()
	r.source = fresh
	r.mu.Unlock()
	return fresh, nil
}

func (r *guestRoom) transport(video bool) {
	track := "audio"
	if video {
		track = "video"
	}
	reconnect := guestReconnectLog{recovery: r.recovery, track: track, enabled: !video}
	var last error
	for attempt := 0; r.ctx.Err() == nil; attempt++ {
		if attempt >= nativeRecoveryAttempts {
			// A failed track must not discard healthy media from the other
			// connection. Retry more slowly while participants still arrive.
			if time.Since(time.Unix(0, r.lastMedia.Load())) < time.Minute {
				if waitHLS(r.ctx, 30*time.Second) != nil {
					return
				}
				attempt = 1
			} else {
				r.finish(fault.Wrap("could not restore the room's shared "+track+" connection after 3 attempts: "+fault.Reason(last), reconnect.cause(last)))
				return
			}
		}
		if attempt > 0 {
			if waitHLS(r.ctx, nativeRecoveryDelay) != nil {
				return
			}
			if _, err := r.refresh(); err != nil {
				if errors.Is(err, ErrRoomEnded) || errors.Is(err, ErrRoomModeChanged) {
					r.finish(err)
					return
				}
				last = err
				if r.ctx.Err() == nil {
					reconnect.failed(time.Now(), err)
				}
				continue
			}
		}
		source := r.current()
		started := time.Now()
		last = r.receiveTrack(source, video, attempt, &reconnect)
		if r.ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			attempt = -1
		}
		reconnect.failed(time.Now(), last)
	}
}

func (r *guestRoom) receiveTrack(source Stream, video bool, attempt int, reconnect *guestReconnectLog) error {
	channel := source.native
	var conn *nativeMediaConn
	var err error
	if video {
		conn, err = dialNativeMedia(r.ctx, channel.video, channel.source, channel.sid, true)
	} else {
		conn, err = dialGuestAudio(r.ctx, channel.media, channel.source, channel.sid, attempt)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.guest = true
	stop := context.AfterFunc(r.ctx, func() { conn.Close() })
	defer stop()
	if video {
		err = conn.send(0x37902, nativeLoginBody(channel, source.UID, true))
	} else {
		err = conn.send(0x32302, guestAudioLoginBody(channel))
		if err == nil {
			err = conn.subscribeGuestAudio()
		}
	}
	if err != nil {
		return err
	}
	pingCtx, cancelPing := context.WithCancel(r.ctx)
	defer cancelPing()
	go guestKeepalive(pingCtx, conn, channel)
	videoSources := make(map[uint32]*nativeVideoRouter)
	seen := make(map[uint32]*guestPacketWindow)
	activity := make(map[uint32]time.Time)
	remember := func(uid uint32) {
		if _, exists := activity[uid]; !exists && len(activity) >= 64 {
			for old, at := range activity {
				if time.Since(at) > 30*time.Second {
					delete(activity, old)
					delete(videoSources, old)
					delete(seen, old)
				}
			}
		}
		activity[uid] = time.Now()
	}
	for {
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		reconnect.waiting(time.Now())
		if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
			return err
		}
		packet, err := readNativePacket(conn)
		received := time.Since(r.epoch).Nanoseconds()
		if err != nil {
			return err
		}
		if packet.status != 200 {
			return fmt.Errorf("BIGO rejected guest media (status %d)", packet.status)
		}
		if !conn.confirmed {
			if err := conn.control(packet); err != nil {
				return err
			}
			continue
		}
		var uid uint32
		var frames []nativeMediaFrame
		if video && packet.uri == 5 {
			part, err := decodeNativeVideo(packet.data)
			if err != nil {
				return err
			}
			uid = part.owner
			part.received = received
			if uid == 0 {
				continue
			}
			remember(uid)
			router := videoSources[uid]
			if router == nil {
				if len(videoSources) >= 64 {
					return errors.New("BIGO advertised too many guest video sources")
				}
				router = &nativeVideoRouter{owner: uid}
				videoSources[uid] = router
			}
			frames, err = router.add(part)
			if err != nil {
				return err
			}
		} else if !video && packet.uri == 2 {
			data, err := parseNativeAudio(packet.data)
			if err != nil {
				return err
			}
			uid = data.owner
			if uid == 0 {
				continue
			}
			remember(uid)
			window := seen[uid]
			if window == nil {
				if len(seen) >= 64 {
					return errors.New("BIGO advertised too many guest audio sources")
				}
				window = &guestPacketWindow{}
				seen[uid] = window
			}
			if !window.accept(data.sequence) {
				continue
			}
			frames, err = decodeGuestAudio(data)
			if errors.Is(err, errNativeAACFrame) {
				err = nil
			}
			if err != nil {
				return err
			}
		} else {
			if err := conn.control(packet); err != nil {
				return err
			}
		}
		for _, frame := range frames {
			if !frame.video {
				frame.received = received
			}
			r.lastMedia.Store(time.Now().UnixNano())
			select {
			case r.frames <- guestFrame{uid: uid, frame: frame}:
				if r.ctx.Err() == nil {
					reconnect.resumed(time.Now())
				}
			case <-r.ctx.Done():
				return r.ctx.Err()
			}
		}
	}
}

func guestKeepalive(ctx context.Context, conn *nativeMediaConn, channel *pJoinChannelRes) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b := marshalGuestPing(channel)
			if conn.send(0x32e02, b) != nil {
				conn.Close()
				return
			}
		}
	}
}

func (r *guestRoom) watchMedia() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	attempts, last := 0, r.lastMedia.Load()
	var next time.Time
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			current := r.lastMedia.Load()
			if current != last {
				last, attempts, next = current, 0, time.Time{}
			}
			if now.Sub(time.Unix(0, current)) < time.Minute || attempts >= 3 || now.Before(next) {
				continue
			}
			attempts++
			next = now.Add(45 * time.Second)
			if _, err := r.refresh(); errors.Is(err, ErrRoomEnded) || errors.Is(err, ErrRoomModeChanged) {
				r.finish(err)
				return
			}
		}
	}
}

// A bounded replay window accepts late UDP packets while ignoring exact
// retransmissions. Packet order is restored separately for each participant.
type guestPacketWindow struct {
	values [256]uint32
	used   [256]bool
	next   int
}

func (w *guestPacketWindow) accept(sequence uint32) bool {
	for i, value := range w.values {
		if w.used[i] && value == sequence {
			return false
		}
	}
	w.values[w.next], w.used[w.next] = sequence, true
	w.next = (w.next + 1) % len(w.values)
	return true
}
