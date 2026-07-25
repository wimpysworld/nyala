//go:build live

package engine

import (
	"image"
	"math"
	"testing"
	"time"
)

// liveFindPage is a self-contained data: URL with four occurrences of one word,
// a text field, and a tall spacer, so a test knows the match total, can prove
// focus and the caret survive a search, and can step to a match far below the
// fold. It carries no "#" colour literal, because a data: URL would read that
// as a fragment.
const liveFindPage = "data:text/html," +
	"<html><body style='margin:0;font:16px monospace;background:white;color:black'>" +
	"<input id='q' style='width:200px'>" +
	"<p style='margin:0'>needle alpha</p>" +
	"<p style='margin:0'>bravo needle</p>" +
	"<p style='margin:0'>charlie needle</p>" +
	"<div style='height:200vh'></div>" +
	"<p style='margin:0'>needle omega</p>" +
	"</body></html>"

// findColourTolerance is the per-channel slack a painted highlight is matched
// with. The block backend captures JPEG q90, so a solid fill arrives with
// compression noise around its edges.
const findColourTolerance = 32

// countColour returns how many of the frame's pixels sit within
// findColourTolerance of want on every channel.
func countColour(img image.Image, want [3]int) int {
	if img == nil {
		return 0
	}
	near := func(got, target int) bool {
		d := got - target
		if d < 0 {
			d = -d
		}
		return d <= findColourTolerance
	}
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if near(int(r>>8), want[0]) && near(int(g>>8), want[1]) && near(int(bl>>8), want[2]) {
				n++
			}
		}
	}
	return n
}

// waitForFrameColour drains delivered frames until one satisfies want for the
// pixel count of colour, so a test asserts against what Chromium actually
// painted rather than against the search state alone. It returns the last count
// seen, so a failure reports the pixels the frame really carried.
func waitForFrameColour(t *testing.T, e *ChromiumEngine, colour [3]int, want func(int) bool) (got int, ok bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case frame, open := <-e.Frames():
			if !open {
				return got, false
			}
			if frame.Img == nil {
				continue
			}
			got = countColour(frame.Img, colour)
			if want(got) {
				return got, true
			}
		case <-deadline:
			return got, false
		}
	}
}

// heldScrollY reads the engine's held vertical scroll offset, the value the
// wheel transport, the Home key, and crash recovery all work from.
func heldScrollY(e *ChromiumEngine) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scrollY
}

// TestLiveFindCountsMatches proves a query returns the total the page really
// holds and selects the first match, and that the count arrives on the state
// snapshot the viewer reads.
func TestLiveFindCountsMatches(t *testing.T) {
	e, _ := liveEngineOn(t, liveFindPage)

	e.Find("needle")

	got := e.FindState()
	if got.Total != 4 || got.Current != 1 {
		t.Fatalf("find state = %+v, want 4 matches with the first selected", got)
	}
	if got.Pending || got.Failed {
		t.Fatalf("find state = %+v, want a settled search", got)
	}
	if got.Query != "needle" {
		t.Fatalf("find state query = %q, want %q", got.Query, "needle")
	}

	// A query the page does not hold reports no matches and leaves no current
	// match, so the previous search's counts cannot linger.
	e.Find("zebra")

	if got := e.FindState(); got.Total != 0 || got.Current != 0 || got.Failed {
		t.Fatalf("find state = %+v for a query the page does not hold, want no matches", got)
	}
}

// TestLiveFindStepsThroughMatches proves next and previous move the current
// match in the expected direction and wrap at both ends, and that a step to a
// match below the fold scrolls the page and leaves the engine's held offset
// aligned with where the page settled.
func TestLiveFindStepsThroughMatches(t *testing.T) {
	e, ctx := liveEngineOn(t, liveFindPage)

	e.Find("needle")
	for _, want := range []int{2, 3, 4, 1} {
		e.FindNext()
		if got := e.FindState().Current; got != want {
			t.Fatalf("current match = %d after next, want %d", got, want)
		}
	}
	for _, want := range []int{4, 3} {
		e.FindPrev()
		if got := e.FindState().Current; got != want {
			t.Fatalf("current match = %d after previous, want %d", got, want)
		}
	}

	// The fourth match sits under a 200vh spacer, so stepping to it must scroll
	// the page. Land on it again and check the engine adopted the page offset.
	e.FindNext()
	if got := e.FindState().Current; got != 4 {
		t.Fatalf("current match = %d, want the fourth", got)
	}
	page := evalFloat(t, ctx, "window.scrollY")
	if page <= 0 {
		t.Fatalf("window.scrollY = %g after stepping to a match below the fold, want a scroll", page)
	}
	if held := heldScrollY(e); held != int(math.Round(page)) {
		t.Fatalf("engine held scroll offset = %d, want the page offset %g", held, page)
	}
}

// TestLiveFindPaintsHighlights proves Chromium paints the search highlights into
// ordinary screencast frames, in both registrations (every match in the match
// colour, the selected one in the current colour), and that a clear removes them
// from the frames again. The pixel counts come from the frames the viewer draws,
// so the highlight is observed rather than assumed.
func TestLiveFindPaintsHighlights(t *testing.T) {
	e, _ := liveEngineOn(t, liveFindPage)

	e.Find("needle")

	// One match is about 48x16 device pixels, so a painted registration covers
	// several hundred pixels; the floor stays well under one match.
	const painted = 200
	if got, ok := waitForFrameColour(t, e, findCurrentRGB, func(n int) bool { return n >= painted }); !ok {
		t.Fatalf("current-match pixels = %d, want at least %d painted in a frame", got, painted)
	}
	if got, ok := waitForFrameColour(t, e, findMatchRGB, func(n int) bool { return n >= painted }); !ok {
		t.Fatalf("match pixels = %d, want at least %d painted in a frame", got, painted)
	}

	e.FindClear()

	const gone = 20
	if got, ok := waitForFrameColour(t, e, findCurrentRGB, func(n int) bool { return n <= gone }); !ok {
		t.Fatalf("current-match pixels = %d after a clear, want at most %d", got, gone)
	}
	if got, ok := waitForFrameColour(t, e, findMatchRGB, func(n int) bool { return n <= gone }); !ok {
		t.Fatalf("match pixels = %d after a clear, want at most %d", got, gone)
	}
}

// TestLiveFindKeepsPageFocus proves the page's DOM focus and caret survive a
// search, which is what lets the bottom bar own the text field while the page
// stays the search target, and that page input still reaches the focused field
// after a clear: the clear must not trap it.
func TestLiveFindKeepsPageFocus(t *testing.T) {
	e, ctx := liveEngineOn(t, liveFindPage)

	evalVoid(t, ctx, "document.getElementById('q').focus()")
	e.SendText("abc")
	if got, ok := waitForString(t, ctx, "document.getElementById('q').value", "abc"); !ok {
		t.Fatalf("field value = %q before the search, want %q", got, "abc")
	}
	evalVoid(t, ctx, "document.getElementById('q').setSelectionRange(1,1)")

	e.Find("needle")

	if got := evalString(t, ctx, "document.activeElement.id"); got != "q" {
		t.Fatalf("focused element = %q after a search, want the page field", got)
	}
	if got := evalString(t, ctx, "String(document.getElementById('q').selectionStart)"); got != "1" {
		t.Fatalf("caret offset = %q after a search, want 1", got)
	}

	e.FindClear()

	e.SendText("Z")
	if got, ok := waitForString(t, ctx, "document.getElementById('q').value", "aZbc"); !ok {
		t.Fatalf("field value = %q after typing past a cleared search, want %q", got, "aZbc")
	}
}

// TestLiveFindClearRemovesSearchState proves a clear takes the page-side state
// with it: the highlight registrations, the injected stylesheet, and the held
// ranges all go, and the engine reports no active search.
func TestLiveFindClearRemovesSearchState(t *testing.T) {
	e, ctx := liveEngineOn(t, liveFindPage)

	e.Find("needle")
	if got := evalString(t, ctx, "String(CSS.highlights.size)"); got != "2" {
		t.Fatalf("registered highlights = %q after a search, want both registrations", got)
	}

	e.FindClear()

	if got := evalString(t, ctx, "String(CSS.highlights.size)"); got != "0" {
		t.Fatalf("registered highlights = %q after a clear, want none", got)
	}
	if got := evalString(t, ctx, "String(document.getElementById('nyala-find-style')===null)"); got != "true" {
		t.Fatalf("highlight stylesheet present after a clear (%q)", got)
	}
	if got := evalString(t, ctx, "String(window.__nyalaFind.ranges.length)"); got != "0" {
		t.Fatalf("held match ranges = %q after a clear, want none", got)
	}
	if state := e.FindState(); state != (FindState{}) {
		t.Fatalf("find state = %+v after a clear, want no active search", state)
	}
}

// TestLiveFindStepWithoutQueryIsNoOp proves a step with no active search
// dispatches nothing and leaves the state empty, so a stray next or previous
// cannot invent a search.
func TestLiveFindStepWithoutQueryIsNoOp(t *testing.T) {
	e, ctx := liveEngineOn(t, liveFindPage)

	e.FindNext()
	e.FindPrev()

	if state := e.FindState(); state != (FindState{}) {
		t.Fatalf("find state = %+v after stepping with no search, want no search", state)
	}
	if got := evalString(t, ctx, "String(window.__nyalaFind===undefined)"); got != "true" {
		t.Fatalf("page search state exists after stepping with no search (%q)", got)
	}
}
