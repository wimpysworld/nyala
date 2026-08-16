package viewer

import (
	"bytes"
	"strings"
	"testing"
)

// titleEngine is a fakeEngine whose title and URL are settable, so a test can
// drive maybeEmitTitle through changed, repeated, and empty titles.
type titleEngine struct {
	fakeEngine
	title string
	url   string
}

func (e *titleEngine) CurrentTitle() string { return e.title }

func (e *titleEngine) CurrentURL() string { return e.url }

// newTitleSession builds a streamSession whose emitControl writes the returned
// buffer directly: the writer is nil, so the control path takes the direct
// tty branch.
func newTitleSession(eng *titleEngine) (*streamSession, *bytes.Buffer) {
	var out bytes.Buffer
	return &streamSession{eng: eng, st: &State{}, tty: &out}, &out
}

// TestMaybeEmitTitleChangeGated verifies that a changed title writes once, a
// repeated title writes nothing, and a second distinct title writes again.
func TestMaybeEmitTitleChangeGated(t *testing.T) {
	eng := &titleEngine{title: "First page", url: "https://example.org/a"}
	s, out := newTitleSession(eng)

	s.maybeEmitTitle()
	if got, want := out.String(), setTitle("First page"); got != want {
		t.Fatalf("first emit = %q, want %q", got, want)
	}

	out.Reset()
	s.maybeEmitTitle()
	if out.Len() != 0 {
		t.Fatalf("repeated title wrote %q, want nothing", out.String())
	}

	eng.title = "Second page"
	s.maybeEmitTitle()
	if got, want := out.String(), setTitle("Second page"); got != want {
		t.Fatalf("second emit = %q, want %q", got, want)
	}
}

// TestMaybeEmitTitleFrameArrival verifies the frame-arrival gating: repeated
// per-frame calls with an unchanged title write once, and a later title change
// writes again, so the frame path alone suffices without the fps beat.
func TestMaybeEmitTitleFrameArrival(t *testing.T) {
	eng := &titleEngine{title: "Loading page", url: "https://example.org/a"}
	s, out := newTitleSession(eng)

	// Simulate a burst of frame arrivals with an unchanged title.
	for range 5 {
		s.maybeEmitTitle()
	}
	if got, want := out.String(), setTitle("Loading page"); got != want {
		t.Fatalf("frame burst wrote %q, want a single %q", got, want)
	}

	// A title change after load emits on the next frame arrival.
	out.Reset()
	eng.title = "Loaded page"
	s.maybeEmitTitle()
	if got, want := out.String(), setTitle("Loaded page"); got != want {
		t.Fatalf("post-change emit = %q, want %q", got, want)
	}
}

// TestMaybeEmitTitleBytes verifies the emitted sequence is OSC 0 terminated
// by ST, with no BEL anywhere in the write.
func TestMaybeEmitTitleBytes(t *testing.T) {
	eng := &titleEngine{title: "Nyala", url: "https://example.org"}
	s, out := newTitleSession(eng)
	s.maybeEmitTitle()
	got := out.String()
	if !strings.HasPrefix(got, "\x1b]0;") {
		t.Fatalf("emit %q does not start with OSC 0", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Fatalf("emit %q does not end with ST", got)
	}
	if strings.Contains(got, "\a") {
		t.Fatalf("emit %q contains a BEL", got)
	}
}

// TestMaybeEmitTitleHostFallback verifies that an empty engine title falls
// back to the current URL's host, the startup state before any title is read.
func TestMaybeEmitTitleHostFallback(t *testing.T) {
	eng := &titleEngine{title: "", url: "https://example.org/path"}
	s, out := newTitleSession(eng)
	s.maybeEmitTitle()
	if got, want := out.String(), setTitle("example.org"); got != want {
		t.Fatalf("fallback emit = %q, want %q", got, want)
	}
}
