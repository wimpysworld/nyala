package viewer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/engine"
)

// progressEngine is a fakeEngine whose load state is settable, so a test can
// drive maybeEmitProgress through repeated and changed states.
type progressEngine struct {
	fakeEngine
	load engine.LoadState
}

func (e *progressEngine) LoadState() engine.LoadState { return e.load }

// TestProgressSequenceFormat verifies the OSC 9;4 sequence always carries
// both fields and terminates with ST, never BEL.
func TestProgressSequenceFormat(t *testing.T) {
	got := progressSequence(1, 42)
	want := "\x1b]9;4;1;42\x1b\\"
	if got != want {
		t.Fatalf("progressSequence(1, 42) = %q, want %q", got, want)
	}
	if strings.Contains(got, "\x07") {
		t.Fatalf("progressSequence(1, 42) = %q, contains BEL", got)
	}
	if progressClear != progressSequence(0, 0) {
		t.Fatalf("progressClear = %q, want %q", progressClear, progressSequence(0, 0))
	}
}

// TestExitTerminalClearsProgress asserts the teardown clears the OSC 9;4
// indicator unconditionally, in the shape of the mode-pairing test: on every
// kitty-keyboard variant and before the first frame draws, so every exit path
// including a fatal signal clears the indicator.
func TestExitTerminalClearsProgress(t *testing.T) {
	for _, kitty := range []bool{false, true} {
		for _, drawn := range []bool{false, true} {
			teardown := exitTerminal(24, drawn, kitty)
			if !strings.Contains(teardown, progressClear) {
				t.Fatalf("exitTerminal(24, %t, %t) = %q, missing %q", drawn, kitty, teardown, progressClear)
			}
		}
	}
}

// TestMaybeEmitProgressChangeGated verifies that a changed load state writes
// once, a repeated state writes nothing, and a further change writes again.
func TestMaybeEmitProgressChangeGated(t *testing.T) {
	eng := &progressEngine{load: engine.LoadState{State: engine.LoadPercentage, Percent: 30}}
	var out bytes.Buffer
	s := &streamSession{eng: eng, st: &State{}, tty: &out}

	s.maybeEmitProgress()
	first := progressSequence(engine.LoadPercentage, 30)
	if got := out.String(); got != first {
		t.Fatalf("first change wrote %q, want %q", got, first)
	}

	s.maybeEmitProgress()
	if got := out.String(); got != first {
		t.Fatalf("repeated state wrote %q, want %q", got, first)
	}

	eng.load = engine.LoadState{State: engine.LoadClear}
	s.maybeEmitProgress()
	want := first + progressClear
	if got := out.String(); got != want {
		t.Fatalf("second change wrote %q, want %q", got, want)
	}
}
