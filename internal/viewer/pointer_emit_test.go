package viewer

import (
	"bytes"
	"strings"
	"testing"
)

// pointerEngine is a fakeEngine whose computed cursor is settable, so a test
// can drive maybeEmitPointerShape through changed, repeated, and unsupported
// cursor values.
type pointerEngine struct {
	fakeEngine
	cursor string
}

func (e *pointerEngine) PointerCursor() string { return e.cursor }

// newPointerSession builds a streamSession whose emitControl writes the
// returned buffer directly (the writer is nil), on a State with OSC 22
// support confirmed for the given shapes.
func newPointerSession(eng *pointerEngine, shapes ...string) (*streamSession, *bytes.Buffer) {
	supported := make(map[string]bool, len(shapes))
	for _, name := range shapes {
		supported[name] = true
	}
	var out bytes.Buffer
	st := &State{pointerShapeOK: len(shapes) > 0, pointerShapes: supported}
	return &streamSession{eng: eng, st: st, tty: &out}, &out
}

// TestMaybeEmitPointerShapeCapabilityGate verifies that a session whose
// handshake found no OSC 22 support writes zero pointer-shape bytes across
// page hovers and the bar row.
func TestMaybeEmitPointerShapeCapabilityGate(t *testing.T) {
	eng := &pointerEngine{cursor: "pointer"}
	var out bytes.Buffer
	s := &streamSession{eng: eng, st: &State{}, tty: &out}

	s.maybeEmitPointerShape()
	s.pointerOnBar = true
	s.maybeEmitPointerShape()
	if out.Len() != 0 {
		t.Fatalf("unsupporting terminal wrote %q, want nothing", out.String())
	}
}

// TestMaybeEmitPointerShapeChangeGated verifies that a changed cursor writes
// once, a repeated cursor writes nothing, and a second distinct cursor writes
// again.
func TestMaybeEmitPointerShapeChangeGated(t *testing.T) {
	eng := &pointerEngine{cursor: "pointer"}
	s, out := newPointerSession(eng, "pointer", "text")

	s.maybeEmitPointerShape()
	if got, want := out.String(), pointerShapeSequence("pointer"); got != want {
		t.Fatalf("first emit = %q, want %q", got, want)
	}

	out.Reset()
	s.maybeEmitPointerShape()
	if out.Len() != 0 {
		t.Fatalf("repeated cursor wrote %q, want nothing", out.String())
	}

	eng.cursor = "text"
	s.maybeEmitPointerShape()
	if got, want := out.String(), pointerShapeSequence("text"); got != want {
		t.Fatalf("second emit = %q, want %q", got, want)
	}
}

// TestMaybeEmitPointerShapeUnsupportedDropped verifies that a cursor mapping
// to a shape outside the terminal's supported set emits nothing and leaves
// the last written shape standing.
func TestMaybeEmitPointerShapeUnsupportedDropped(t *testing.T) {
	eng := &pointerEngine{cursor: "pointer"}
	s, out := newPointerSession(eng, "pointer")

	s.maybeEmitPointerShape()
	out.Reset()

	// "wait" maps by identity but is absent from the supported set, so it is
	// dropped rather than substituted.
	eng.cursor = "wait"
	s.maybeEmitPointerShape()
	if out.Len() != 0 {
		t.Fatalf("unsupported shape wrote %q, want nothing", out.String())
	}
	if s.lastShape != "pointer" {
		t.Fatalf("lastShape = %q, want %q", s.lastShape, "pointer")
	}
}

// TestMaybeEmitPointerShapeBarReset verifies that motion onto the bar row
// resets the pointer to the terminal default exactly once, and that leaving
// the bar restores the page's shape.
func TestMaybeEmitPointerShapeBarReset(t *testing.T) {
	eng := &pointerEngine{cursor: "pointer"}
	s, out := newPointerSession(eng, "pointer")

	s.maybeEmitPointerShape()
	out.Reset()

	s.pointerOnBar = true
	s.maybeEmitPointerShape()
	if got := out.String(); got != pointerShapeReset {
		t.Fatalf("bar emit = %q, want %q", got, pointerShapeReset)
	}

	out.Reset()
	s.maybeEmitPointerShape()
	if out.Len() != 0 {
		t.Fatalf("repeated bar hover wrote %q, want nothing", out.String())
	}

	s.pointerOnBar = false
	s.maybeEmitPointerShape()
	if got, want := out.String(), pointerShapeSequence("pointer"); got != want {
		t.Fatalf("leave-bar emit = %q, want %q", got, want)
	}
}

// TestMaybeEmitPointerShapeBytes verifies the emitted sequence is OSC 22
// terminated by ST, with no BEL anywhere in the write.
func TestMaybeEmitPointerShapeBytes(t *testing.T) {
	eng := &pointerEngine{cursor: "text"}
	s, out := newPointerSession(eng, "text")
	s.maybeEmitPointerShape()
	got := out.String()
	if !strings.HasPrefix(got, "\x1b]22;") {
		t.Fatalf("emit %q does not start with OSC 22", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Fatalf("emit %q does not end with ST", got)
	}
	if strings.Contains(got, "\a") {
		t.Fatalf("emit %q contains a BEL", got)
	}
}
