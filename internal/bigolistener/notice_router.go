package bigolistener

import (
	"slices"
	"time"
)

const (
	noticePairGrace = 3 * time.Second
	maxNoticeHints  = 128
)

type noticeHint struct {
	appID, sequence int32
	uri             int32
	avatars         []string
	deadline        time.Time
	resolved        bool
}

type noticeDetail struct {
	avatars  []string
	deadline time.Time
	paired   bool
}

// Each connection owns its router. It uses the passive read's deadline, with
// no extra goroutine, connection, or API request to wait for a paired detail.
type noticeRouter struct {
	hints    []noticeHint
	details  []noticeDetail
	overflow time.Time
}

func (r *noticeRouter) accept(now time.Time, event liveEvent) (noticeIdentity, bool) {
	notice := describeLiveNotice(event)
	if notice.conflict {
		notice.avatars = nil
	}
	if notice.identity.known() {
		matches, match := 0, 0
		for i, hint := range r.hints {
			if !hint.resolved && now.Before(hint.deadline) && sameNoticeAvatar(hint.avatars, notice.avatars) {
				matches++
				match = i
			}
		}
		if matches == 1 {
			r.hints[match].resolved = true
		}
		if len(notice.avatars) > 0 {
			// Bound the reverse-order cache as well as pending generic hints.
			if len(r.details) == maxNoticeHints {
				copy(r.details, r.details[1:])
				r.details = r.details[:len(r.details)-1]
			}
			r.details = append(r.details, noticeDetail{avatars: notice.avatars, deadline: now.Add(noticePairGrace), paired: matches > 0})
		}
		return notice.identity, true
	}
	// Duplicate generic frames must not leave an extra unresolved fallback
	// behind after the corresponding detailed message has already arrived.
	for _, hint := range r.hints {
		if event.Sequence != 0 && hint.appID == event.AppID && hint.sequence == event.Sequence && hint.uri == event.URI {
			return noticeIdentity{}, false
		}
	}
	resolved := false
	if !notice.conflict {
		matches, match := 0, 0
		for i, detail := range r.details {
			if !detail.paired && now.Before(detail.deadline) && sameNoticeAvatar(detail.avatars, notice.avatars) {
				matches++
				match = i
			}
		}
		if matches == 1 {
			r.details[match].paired = true
			resolved = true
		}
	}
	if len(r.hints) == maxNoticeHints {
		if !resolved && r.overflow.IsZero() {
			r.overflow = now.Add(noticePairGrace)
		}
		return noticeIdentity{}, false
	}
	r.hints = append(r.hints, noticeHint{appID: event.AppID, sequence: event.Sequence, uri: event.URI, avatars: notice.avatars, deadline: now.Add(noticePairGrace), resolved: resolved})
	return noticeIdentity{}, false
}

func sameNoticeAvatar(a, b []string) bool {
	for _, key := range a {
		if slices.Contains(b, key) {
			return true
		}
	}
	return false
}

func (r *noticeRouter) expire(now time.Time) bool {
	fallback := !r.overflow.IsZero() && !now.Before(r.overflow)
	if fallback {
		r.overflow = time.Time{}
	}
	r.hints = slices.DeleteFunc(r.hints, func(h noticeHint) bool {
		if now.Before(h.deadline) {
			return false
		}
		fallback = fallback || !h.resolved
		return true
	})
	r.details = slices.DeleteFunc(r.details, func(d noticeDetail) bool { return !now.Before(d.deadline) })
	return fallback
}

func (r *noticeRouter) deadline() time.Time {
	next := r.overflow
	for _, hint := range r.hints {
		if next.IsZero() || hint.deadline.Before(next) {
			next = hint.deadline
		}
	}
	for _, detail := range r.details {
		if next.IsZero() || detail.deadline.Before(next) {
			next = detail.deadline
		}
	}
	return next
}

func (r *noticeRouter) unresolved() bool {
	return !r.overflow.IsZero() || slices.ContainsFunc(r.hints, func(h noticeHint) bool { return !h.resolved })
}
