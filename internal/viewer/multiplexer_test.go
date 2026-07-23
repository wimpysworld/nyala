package viewer

import (
	"slices"
	"testing"
)

// TestEngineRows pins the pure engine-grid row decision: Sixel under tmux or
// zellij reserves the bottom terminal row, every other backend-multiplexer
// pair keeps the full grid, and a grid below two rows never shrinks.
func TestEngineRows(t *testing.T) {
	cases := []struct {
		name    string
		backend Backend
		mux     multiplexerKind
		rows    int
		want    int
	}{
		{"sixel under tmux reserves bottom row", BackendSixel, muxTmux, 24, 23},
		{"sixel under zellij reserves bottom row", BackendSixel, muxZellij, 24, 23},
		{"block under tmux keeps full grid", BackendBlock, muxTmux, 24, 24},
		{"block under zellij keeps full grid", BackendBlock, muxZellij, 24, 24},
		{"kitty under tmux keeps full grid", BackendKitty, muxTmux, 24, 24},
		{"sixel on native terminal keeps full grid", BackendSixel, muxNone, 24, 24},
		{"block on native terminal keeps full grid", BackendBlock, muxNone, 24, 24},
		{"sixel under screen keeps full grid", BackendSixel, muxScreen, 24, 24},
		{"sixel under herdr keeps full grid", BackendSixel, muxHerdr, 24, 24},
		{"two rows reserve to one", BackendSixel, muxTmux, 2, 1},
		{"one row never shrinks", BackendSixel, muxTmux, 1, 1},
		{"zero rows pass through", BackendSixel, muxZellij, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineRows(tc.backend, tc.mux, tc.rows); got != tc.want {
				t.Fatalf("engineRows(%v, %v, %d) = %d, want %d",
					tc.backend, tc.mux, tc.rows, got, tc.want)
			}
		})
	}
}

// muxKeys is every focus-multiplexer env var detectMultiplexer reads.
// clearMuxEnv unsets all of them for the current test, registering cleanup, so
// a host-set TMUX or ZELLIJ does not leak into a false-branch case.
var muxKeys = []string{"TMUX", "ZELLIJ", "HERDR_ENV", "HERDR_PANE_ID", "STY"}

// TestDetectMultiplexer asserts detectMultiplexer reports the right kind for
// each focus multiplexer and muxNone when none is set. ZELLIJ is a presence
// signal: a bare ZELLIJ=0 still gates. herdr needs both HERDR_ENV=1 and
// HERDR_PANE_ID; one key alone does not gate.
func TestDetectMultiplexer(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want multiplexerKind
	}{
		{"none set", nil, muxNone},
		{"tmux", map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}, muxTmux},
		{"zellij present", map[string]string{"ZELLIJ": "1"}, muxZellij},
		{"zellij zero is presence", map[string]string{"ZELLIJ": "0"}, muxZellij},
		{"screen", map[string]string{"STY": "1234.pts-0.host"}, muxScreen},
		{"herdr both keys", map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p1"}, muxHerdr},
		{"herdr env only", map[string]string{"HERDR_ENV": "1"}, muxNone},
		{"herdr pane only", map[string]string{"HERDR_PANE_ID": "p1"}, muxNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMuxEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := detectMultiplexer(); got != c.want {
				t.Fatalf("detectMultiplexer() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestMultiplexerBackends drives the pure per-multiplexer backend policy:
// Sixel is offered under tmux and zellij only when DA1 advertised it and the
// cell geometry is real, tmux additionally needs an XTVERSION identity of 3.6
// or later (older tmux caps DCS input at an unraisable 1 MiB and discards
// busy frames silently), Kitty is never offered, screen and herdr stay block
// only regardless of capability, and the Rio compatibility deny still gates
// Sixel off.
func TestMultiplexerBackends(t *testing.T) {
	cases := []struct {
		name     string
		mux      multiplexerKind
		termID   string
		sixelDA1 bool
		geometry bool
		compat   terminalCompatibility
		want     []Backend
	}{
		{"tmux sixel and geometry", muxTmux, "tmux 3.6", true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"tmux 3.6a passes the version gate", muxTmux, "tmux 3.6a", true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"tmux 3.7b passes the version gate", muxTmux, "tmux 3.7b", true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"tmux 3.5a fails the version gate", muxTmux, "tmux 3.5a", true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux 3.4 fails the version gate", muxTmux, "tmux 3.4", true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux unknown identity fails the version gate", muxTmux, "", true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux sixel no geometry", muxTmux, "tmux 3.7b", true, false, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux no sixel", muxTmux, "tmux 3.7b", false, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"zellij sixel and geometry", muxZellij, "Zellij(0.43.1)", true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"zellij da1 without geometry is not trusted", muxZellij, "Zellij(0.43.1)", true, false, terminalCompatibility{}, []Backend{BackendBlock}},
		{"zellij no sixel", muxZellij, "Zellij(0.43.1)", false, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"zellij ignores the tmux version gate", muxZellij, "", true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"screen ignores capability", muxScreen, "", true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"herdr ignores capability", muxHerdr, "", true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"rio deny gates tmux sixel", muxTmux, "tmux 3.7b", true, true, terminalCompatibility{rio04: true}, []Backend{BackendBlock}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := multiplexerBackends(c.mux, c.termID, c.sixelDA1, c.geometry, c.compat)
			if !slices.Equal(got, c.want) {
				t.Fatalf("multiplexerBackends = %v, want %v", got, c.want)
			}
		})
	}
}

// TestMultiplexerSixelPassesForcedBackend asserts that when the multiplexer
// policy offers Sixel, an explicit --backend=sixel passes the generic
// validator, so live testing inside tmux and zellij can force the backend.
func TestMultiplexerSixelPassesForcedBackend(t *testing.T) {
	enabled := multiplexerBackends(muxTmux, "tmux 3.7b", true, true, terminalCompatibility{})
	if err := ValidateForcedBackend(ModeSixel, enabled); err != nil {
		t.Fatalf("ValidateForcedBackend(sixel, %v) = %v, want nil", enabled, err)
	}
	if err := ValidateForcedBackend(ModeKitty, enabled); err == nil {
		t.Fatalf("ValidateForcedBackend(kitty, %v) = nil, want error", enabled)
	}
}

// TestMultiplexerStartBackend drives the pure auto-start nomination under a
// focus multiplexer: tmux starts on the cycle head (Sixel when gated in),
// zellij demotes the start to Block while Sixel stays in the cycle (its image
// id collision drops every third full-replacement frame), a failed zellij gate
// stays Block, and screen and herdr keep their Block-only start.
func TestMultiplexerStartBackend(t *testing.T) {
	sixelCycle := []Backend{BackendSixel, BackendBlock}
	blockOnly := []Backend{BackendBlock}
	cases := []struct {
		name    string
		mux     multiplexerKind
		enabled []Backend
		want    Backend
	}{
		{"tmux gated in starts sixel", muxTmux, sixelCycle, BackendSixel},
		{"zellij gated in demotes to block", muxZellij, sixelCycle, BackendBlock},
		{"zellij failed gate stays block", muxZellij, blockOnly, BackendBlock},
		{"tmux failed gate stays block", muxTmux, blockOnly, BackendBlock},
		{"screen stays block", muxScreen, blockOnly, BackendBlock},
		{"herdr stays block", muxHerdr, blockOnly, BackendBlock},
		{"empty cycle falls back to block", muxTmux, nil, BackendBlock},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := multiplexerStartBackend(c.mux, c.enabled); got != c.want {
				t.Fatalf("multiplexerStartBackend(%v, %v) = %v, want %v", c.mux, c.enabled, got, c.want)
			}
		})
	}
}

// TestZellijForcedSixelStillStarts guards the manual escape hatch: with the
// zellij demotion applied, --backend=sixel still validates against the enabled
// cycle and applyForcedBackend starts the session on Sixel.
func TestZellijForcedSixelStillStarts(t *testing.T) {
	st := NewState(80, 24)
	st.SetEnabled(multiplexerBackends(muxZellij, "Zellij(0.43.1)", true, true, terminalCompatibility{}))
	st.Backend = multiplexerStartBackend(muxZellij, st.Enabled)
	if st.Backend != BackendBlock {
		t.Fatalf("zellij auto start = %v, want %v", st.Backend, BackendBlock)
	}
	if err := ValidateForcedBackend(ModeSixel, st.Enabled); err != nil {
		t.Fatalf("ValidateForcedBackend(sixel, %v) = %v, want nil", st.Enabled, err)
	}
	applyForcedBackend(&st, ModeSixel)
	if st.Backend != BackendSixel {
		t.Fatalf("forced backend = %v, want %v", st.Backend, BackendSixel)
	}
}
