package bigo

import (
	"net/url"
	"strconv"
)

const rememberedHLSSegments = 1024

type resetObservation struct {
	sequence uint64
	tail     string
}

// hlsCursor rejects overlapping/stale CDN windows without assuming that a
// provider's sequence counters can never restart. A backward window must itself
// advance across two observations before an unannounced reset is accepted.
type hlsCursor struct {
	hasSequence             bool
	sequence, discontinuity uint64
	explicitDiscontinuity   bool
	seen                    map[string]bool
	order                   []string
	next                    int
	pending                 map[string]resetObservation
}

func segmentIdentity(segment hlsSegment, playlist hlsPlaylist) string {
	identity := segment.url
	if u, err := url.Parse(identity); err == nil {
		// Mirrors may use different hosts for exactly the same media object.
		identity = u.EscapedPath() + "?" + u.RawQuery
	}
	if playlist.explicitDiscontinuity {
		identity = strconv.FormatUint(segment.discontinuity, 10) + ":" + identity
	}
	return identity
}

func (c *hlsCursor) fresh(p hlsPlaylist, source string) []hlsSegment {
	var fresh, backward []hlsSegment
	for _, s := range p.segments {
		if c.seen[segmentIdentity(s, p)] {
			continue
		}
		if c.hasSequence && p.explicitDiscontinuity && c.explicitDiscontinuity && s.discontinuity < c.discontinuity {
			continue
		}
		newEpoch := p.explicitDiscontinuity && s.discontinuity > c.discontinuity
		if !c.hasSequence || newEpoch || s.sequence > c.sequence {
			fresh = append(fresh, s)
		} else {
			backward = append(backward, s)
		}
	}
	if len(fresh) > 0 || len(backward) == 0 {
		return fresh
	}
	tail := backward[len(backward)-1]
	observation := resetObservation{sequence: tail.sequence, tail: segmentIdentity(tail, p)}
	previous, observed := c.pending[source]
	if c.pending == nil || len(c.pending) >= 8 {
		c.pending = make(map[string]resetObservation)
	}
	c.pending[source] = observation
	if observed && observation.sequence >= previous.sequence && observation.tail != previous.tail {
		return backward
	}
	return nil
}

func (c *hlsCursor) accept(s hlsSegment, p hlsPlaylist) {
	c.hasSequence, c.sequence = true, s.sequence
	if p.explicitDiscontinuity {
		c.discontinuity = s.discontinuity
		c.explicitDiscontinuity = true
	}
	clear(c.pending)
	if c.seen == nil {
		c.seen = make(map[string]bool)
	}
	key := segmentIdentity(s, p)
	if len(c.order) < rememberedHLSSegments {
		c.order = append(c.order, key)
	} else {
		delete(c.seen, c.order[c.next])
		c.order[c.next] = key
		c.next = (c.next + 1) % rememberedHLSSegments
	}
	c.seen[key] = true
}
