package timeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// TrackInfo describes actual demuxed packet timestamps, not container duration
// tags. First/End are seconds on the original file's presentation clock.
type TrackInfo struct {
	Video          bool
	First, End     float64
	Packets        int64
	PacketDuration float64
	Digest         string
}

type Clip struct {
	File      string  `json:"file"`
	UID       uint32  `json:"uid"`
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Video     bool    `json:"video"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	FileStart float64 `json:"file_start"`
	FileEnd   float64 `json:"file_end"`
	Slot      int     `json:"slot"`
}

type Plan struct {
	Version  int         `json:"version"`
	Timing   string      `json:"timing"`
	Origin   time.Time   `json:"origin"`
	Duration float64     `json:"duration"`
	Slots    int         `json:"slots"`
	Clips    []Clip      `json:"clips"`
	Warnings []string    `json:"warnings,omitempty"`
	Files    []MediaFile `json:"files"`
}

type MediaFile struct {
	File   string      `json:"file"`
	Tracks []TrackInfo `json:"tracks"`
}

type Probe func(context.Context, string) ([]TrackInfo, error)

type clockAnchor struct{ x, y float64 }
type mediaClock struct {
	uid           uint32
	base          uint32
	start         float64
	buckets       map[int64]clockAnchor
	slope, offset float64
}

func (c *mediaClock) source(stamp uint32) float64 { return float64(int32(stamp-c.base)) / 1000 }
func (c *mediaClock) add(stamp uint32, received float64) {
	x := c.source(stamp)
	bucket := int64(math.Floor(x / 30))
	anchor, exists := c.buckets[bucket]
	if !exists || received-x < anchor.y-anchor.x {
		c.buckets[bucket] = clockAnchor{x, received}
	}
}

// Fit the lower arrival envelope. Buffered/replayed packets and jitter increase
// delay; they must not move a source's clock forward. This estimates receiver
// playback timing, not an unobserved common clock on the broadcasters' devices.
func (c *mediaClock) fit() {
	var x, y, xx, xy float64
	keys := make([]int64, 0, len(c.buckets))
	for key := range c.buckets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		a := c.buckets[key]
		x += a.x
		y += a.y
		xx += a.x * a.x
		xy += a.x * a.y
	}
	n := float64(len(c.buckets))
	c.slope = 1
	if den := n*xx - x*x; n >= 3 && den > 0 {
		candidate := (n*xy - x*y) / den
		if candidate >= .999 && candidate <= 1.001 {
			c.slope = candidate
		}
	}
	c.offset = math.Inf(1)
	for _, key := range keys {
		a := c.buckets[key]
		c.offset = min(c.offset, a.y-c.slope*a.x)
	}
}

type mediaSpan struct {
	clip             Clip
	clock            *mediaClock
	first, end       float64
	muxFirst, muxEnd float64
	last             Frame
	frames           int64
}

type fileTrack struct {
	first   float64
	set     bool
	spans   []*mediaSpan
	current *mediaSpan
}

type frameIdentity struct {
	pts, dts, checksum uint32
	video              bool
	codec              byte
}
type clockHistory struct {
	keys   [512]frameIdentity
	clocks map[frameIdentity]*mediaClock
	next   int
}

func (h *clockHistory) remember(f Frame, c *mediaClock) *mediaClock {
	if f.Checksum == 0 {
		return nil
	}
	key := frameIdentity{f.SourcePTS, f.SourceDTS, f.Checksum, f.Video, f.Codec}
	if old := h.clocks[key]; old != nil {
		return old
	}
	if c == nil {
		return nil
	}
	if h.clocks == nil {
		h.clocks = make(map[frameIdentity]*mediaClock)
	}
	delete(h.clocks, h.keys[h.next])
	h.keys[h.next] = key
	h.clocks[key] = c
	h.next = (h.next + 1) % len(h.keys)
	return nil
}

// Reconcile reads every recorded frame using bounded memory, inspects actual
// media packets, estimates source clocks, and removes overlapping source ranges.
// It refuses missing evidence instead of quietly assembling a misleading replay.
func Reconcile(ctx context.Context, dir string, probe Probe) (Plan, error) {
	entries, err := Read(dir)
	if err != nil {
		return Plan{}, err
	}
	if len(entries) == 0 || entries[0].Version != 1 || entries[0].Kind != "broadcast" {
		return Plan{}, errors.New("unsupported broadcast timeline")
	}
	plan := Plan{Version: 1, Timing: "receiver-aligned"}
	files := make(map[string]bool)
	for _, e := range entries {
		if e.Kind == "file" && !e.Epoch.IsZero() && (plan.Origin.IsZero() || e.Epoch.Before(plan.Origin)) {
			plan.Origin = e.Epoch
		}
		if e.Kind == "file_end" && e.Error != "" {
			return Plan{}, errors.New("a guest timing sidecar could not be saved completely; originals are preserved")
		}
	}
	if plan.Origin.IsZero() {
		return Plan{}, errors.New("this broadcast has no received-media timeline")
	}
	var clocks []*mediaClock
	var spans []*mediaSpan
	history := make(map[uint32]*clockHistory)
	for _, entry := range entries {
		if entry.Kind != "file" || files[entry.File] {
			continue
		}
		files[entry.File] = true
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		path, err := LocalPath(dir, entry.File)
		if err != nil {
			return Plan{}, fmt.Errorf("read original guest media: %w", err)
		}
		timing, err := LocalPath(dir, entry.Timing)
		if err != nil {
			return Plan{}, fmt.Errorf("read guest timing evidence: %w", err)
		}
		tracks := map[bool]*fileTrack{false: {}, true: {}}
		epoch := entry.Epoch.Sub(plan.Origin).Seconds()
		seen := history[entry.UID]
		if seen == nil {
			seen = &clockHistory{}
			history[entry.UID] = seen
		}
		err = Walk(timing, func(f Frame) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			t := tracks[f.Video]
			mux := float64(f.PTS) / 90000
			if !t.set {
				t.first, t.set = mux, true
			}
			t.first = min(t.first, mux)
			received := epoch + f.Received.Seconds()
			current := t.current
			var clock *mediaClock
			if current != nil {
				clock = current.clock
			}
			duplicateClock := seen.remember(f, nil)
			// A reset source clock gets its own mapping. Cached media still
			// belongs to its old clock and will be trimmed after reconciliation.
			if duplicateClock != nil {
				clock = duplicateClock
			} else if clock == nil || math.Abs(received-(clock.start+clock.source(f.SourceDTS))) > 10 {
				clock = nil
				for _, c := range clocks {
					if c.uid == entry.UID && math.Abs(received-(c.start+c.source(f.SourceDTS))) <= 10 {
						clock = c
						break
					}
				}
				if clock == nil {
					clock = &mediaClock{uid: entry.UID, base: f.SourceDTS, start: received, buckets: make(map[int64]clockAnchor)}
					clocks = append(clocks, clock)
				}
			}
			if duplicateClock == nil {
				clock.add(f.SourceDTS, received)
				seen.remember(f, clock)
			}
			duration := f.Duration.Seconds()
			if f.Video {
				duration = .04
			}
			x := clock.source(f.SourcePTS)
			gap := false
			if current != nil {
				delta := float64(int32(f.SourceDTS-current.last.SourceDTS)) / 1000
				limit := .25
				if f.Video {
					limit = 1
				}
				gap = delta > limit || delta < -0.25 || math.Abs((float64(f.DTS-current.last.DTS)/90000)-delta) > .05
			}
			if current == nil || current.clock != clock || gap {
				current = &mediaSpan{clip: Clip{File: entry.File, UID: entry.UID, ID: entry.ID, Name: entry.Name, Video: f.Video}, clock: clock, first: x, end: x + duration, muxFirst: mux, muxEnd: mux + duration}
				t.spans = append(t.spans, current)
				spans = append(spans, current)
				t.current = current
			}
			current.first = min(current.first, x)
			current.end = max(current.end, x+duration)
			current.muxFirst = min(current.muxFirst, mux)
			current.muxEnd = max(current.muxEnd, mux+duration)
			current.last = f
			current.frames++
			return nil
		})
		if err != nil {
			return Plan{}, err
		}
		if !tracks[false].set && !tracks[true].set {
			continue
		}
		actual, err := probe(ctx, path)
		if err != nil {
			return Plan{}, fmt.Errorf("inspect original guest media: %w", err)
		}
		plan.Files = append(plan.Files, MediaFile{File: entry.File, Tracks: actual})
		for video, t := range tracks {
			if !t.set {
				continue
			}
			index := slices.IndexFunc(actual, func(a TrackInfo) bool { return a.Video == video && a.Packets > 0 })
			if index < 0 {
				return Plan{}, errors.New("a recorded guest track is missing from its media file")
			}
			a := actual[index]
			shift := a.First - t.first
			for _, s := range t.spans {
				if video && a.PacketDuration > 0 && a.PacketDuration < 1 {
					correction := a.PacketDuration - .04
					s.end += correction
					s.muxEnd += correction
				}
				s.clip.FileStart = s.muxFirst + shift
				s.clip.FileEnd = min(s.muxEnd+shift, a.End)
				if s.clip.FileEnd < s.clip.FileStart {
					return Plan{}, errors.New("a guest file ends before its recorded timing evidence")
				}
				// The final packet duration comes from the saved file, rather
				// than a nominal frame rate or the time FFmpeg finished writing.
				if s == t.spans[len(t.spans)-1] {
					if math.Abs(a.End-(s.muxEnd+shift)) > 1 {
						return Plan{}, errors.New("the saved media extends beyond its timing evidence; originals are preserved")
					}
					s.end += a.End - (s.muxEnd + shift)
					s.clip.FileEnd = a.End
				}
			}
		}
	}
	for _, c := range clocks {
		c.fit()
	}
	for _, s := range spans {
		clip := s.clip
		clip.Start = s.clock.offset + s.clock.slope*s.first
		clip.End = s.clock.offset + s.clock.slope*s.end
		if clip.End > clip.Start && clip.FileEnd > clip.FileStart {
			plan.Clips = append(plan.Clips, clip)
		}
	}
	slices.SortStableFunc(plan.Clips, func(a, b Clip) int {
		if a.Start < b.Start {
			return -1
		}
		if a.Start > b.Start {
			return 1
		}
		return 0
	})
	// Each guest and media type has one active source. Retained video pre-roll
	// overlaps the previous file deliberately; play those frames exactly once.
	type key struct {
		uid   uint32
		video bool
	}
	ends := make(map[key]float64)
	kept := plan.Clips[:0]
	for _, c := range plan.Clips {
		k := key{c.UID, c.Video}
		start := max(0, ends[k], c.Start)
		if start >= c.End {
			continue
		}
		ratio := (c.FileEnd - c.FileStart) / (c.End - c.Start)
		c.FileStart += (start - c.Start) * ratio
		c.Start = start
		ends[k] = c.End
		plan.Duration = max(plan.Duration, c.End)
		kept = append(kept, c)
	}
	plan.Clips = kept
	if len(plan.Clips) == 0 {
		return Plan{}, errors.New("no timed guest media is available for a replay")
	}
	// Do not present an incomplete reconstruction as the full broadcast when
	// recording began before timing support or an evidence file was removed.
	listing, err := os.ReadDir(dir)
	if err != nil {
		return Plan{}, err
	}
	for _, file := range listing {
		ext := strings.ToLower(filepath.Ext(file.Name()))
		if !file.IsDir() && (ext == ".mkv" || ext == ".mka") && file.Name() != "replay.mkv" && !strings.HasSuffix(file.Name(), "_replay.mkv") && !strings.Contains(file.Name(), ".processed.") && !files[file.Name()] {
			return Plan{}, errors.New("some original files have no timing evidence; a complete replay cannot be assembled automatically")
		}
	}
	first := plan.Clips[0].Start
	for i := range plan.Clips {
		plan.Clips[i].Start -= first
		plan.Clips[i].End -= first
	}
	plan.Origin = plan.Origin.Add(time.Duration(first * float64(time.Second)))
	plan.Duration -= first
	assignSlots(&plan)
	return plan, nil
}

func assignSlots(p *Plan) {
	type seat struct {
		uid uint32
		end float64
	}
	var seats []seat
	for i, c := range p.Clips {
		slot := slices.IndexFunc(seats, func(s seat) bool { return s.uid == c.UID && s.end >= c.Start-1 })
		if slot < 0 {
			slot = slices.IndexFunc(seats, func(s seat) bool { return s.end <= c.Start })
		}
		if slot < 0 {
			slot = len(seats)
			seats = append(seats, seat{})
		}
		seats[slot] = seat{c.UID, max(seats[slot].end, c.End)}
		p.Clips[i].Slot = slot
	}
	p.Slots = len(seats)
}
