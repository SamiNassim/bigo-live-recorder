package bigo

import (
	"fmt"
	"testing"
)

func TestCursorRecoversResetOnlyAfterNewWindowAdvances(t *testing.T) {
	var c hlsCursor
	initial := hlsPlaylist{segments: []hlsSegment{{url: "https://a.example/old.ts", sequence: 100}}}
	c.accept(initial.segments[0], initial)
	reset := hlsPlaylist{segments: []hlsSegment{{url: "https://a.example/new-a.ts", sequence: 0}}}
	for range 3 {
		if got := c.fresh(reset, "source"); len(got) != 0 {
			t.Fatal("accepted a frozen backward window", got)
		}
	}
	reset.segments = append(reset.segments, hlsSegment{url: "https://a.example/new-b.ts", sequence: 1})
	got := c.fresh(reset, "source")
	if len(got) != 2 {
		t.Fatal("missed a progressing reset", got)
	}
	for _, s := range got {
		c.accept(s, reset)
	}
	if got := c.fresh(reset, "source"); len(got) != 0 {
		t.Fatal("replayed reset segments", got)
	}
	if got := c.fresh(initial, "source"); len(got) != 0 {
		t.Fatal("replayed old cached window", got)
	}
}

func TestCursorRelativeDiscontinuityRollingOutDoesNotFreeze(t *testing.T) {
	var c hlsCursor
	first := hlsPlaylist{segments: []hlsSegment{{url: "https://a.example/a.ts", sequence: 100, discontinuity: 1}}}
	c.accept(first.segments[0], first)
	next := hlsPlaylist{segments: []hlsSegment{{url: "https://a.example/b.ts", sequence: 101}}}
	if got := c.fresh(next, "source"); len(got) != 1 {
		t.Fatal("relative discontinuity rollover froze the cursor", got)
	}
}

func TestCursorRejectsOlderExplicitEpochAndDeduplicatesMirrors(t *testing.T) {
	var c hlsCursor
	current := hlsPlaylist{explicitDiscontinuity: true, segments: []hlsSegment{{url: "https://a.example/current.ts", sequence: 3, discontinuity: 8}}}
	c.accept(current.segments[0], current)
	old := hlsPlaylist{explicitDiscontinuity: true, segments: []hlsSegment{{url: "https://b.example/old.ts", sequence: 100, discontinuity: 7}}}
	if got := c.fresh(old, "mirror"); len(got) != 0 {
		t.Fatal("accepted an older epoch", got)
	}
	current.segments[0].url = "https://b.example/current.ts"
	if got := c.fresh(current, "mirror"); len(got) != 0 {
		t.Fatal("replayed media from mirror", got)
	}
}

func TestCursorHistoryIsBounded(t *testing.T) {
	var c hlsCursor
	for i := range 3 * rememberedHLSSegments {
		c.accept(hlsSegment{url: fmt.Sprintf("https://a.example/%d.ts", i), sequence: uint64(i)}, hlsPlaylist{})
	}
	if len(c.seen) != rememberedHLSSegments || len(c.order) != rememberedHLSSegments {
		t.Fatal("unbounded media history", len(c.seen), len(c.order))
	}
}
