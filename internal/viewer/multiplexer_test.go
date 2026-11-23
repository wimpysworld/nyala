package viewer

import (
	"slices"
	"testing"
)

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
// Sixel is offered under tmux only, when DA1 advertised it, the cell geometry
// is real, and the XTVERSION identity is 3.6 or later (older tmux caps DCS
// input at an unraisable 1 MiB and discards busy frames silently), Kitty is
// never offered, zellij refuses Sixel regardless of capability (its image id
// collision drops every third full-replacement frame and its bare host
// re-emission breaks scrolling), screen and herdr do not use this native Sixel policy, and host
// support and the Rio compatibility deny can both gate Sixel off.
func TestMultiplexerBackends(t *testing.T) {
	cases := []struct {
		name      string
		mux       multiplexerKind
		termID    string
		sixelDA1  bool
		geometry  bool
		hostSixel bool
		compat    terminalCompatibility
		want      []Backend
	}{
		{"tmux sixel and geometry", muxTmux, "tmux 3.6", true, true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"tmux 3.6a passes the version gate", muxTmux, "tmux 3.6a", true, true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"tmux 3.7b passes the version gate", muxTmux, "tmux 3.7b", true, true, true, terminalCompatibility{}, []Backend{BackendSixel, BackendBlock}},
		{"tmux 3.5a fails the version gate", muxTmux, "tmux 3.5a", true, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux 3.4 fails the version gate", muxTmux, "tmux 3.4", true, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux unknown identity fails the version gate", muxTmux, "", true, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux sixel no geometry", muxTmux, "tmux 3.7b", true, false, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux DA1 but host lacks Sixel", muxTmux, "tmux 3.7b", true, true, false, terminalCompatibility{}, []Backend{BackendBlock}},
		{"tmux no sixel", muxTmux, "tmux 3.7b", false, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"zellij refuses sixel despite da1 and geometry", muxZellij, "Zellij(0.43.1)", true, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"zellij without capability stays block", muxZellij, "Zellij(0.43.1)", false, false, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"screen ignores capability", muxScreen, "", true, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"herdr ignores capability", muxHerdr, "", true, true, true, terminalCompatibility{}, []Backend{BackendBlock}},
		{"rio deny gates tmux sixel", muxTmux, "tmux 3.7b", true, true, true, terminalCompatibility{rio04: true}, []Backend{BackendBlock}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := multiplexerBackends(c.mux, c.termID, c.sixelDA1, c.geometry, c.hostSixel, c.compat)
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
	enabled := multiplexerBackends(muxTmux, "tmux 3.7b", true, true, true, terminalCompatibility{})
	if err := ValidateForcedBackend(ModeSixel, enabled); err != nil {
		t.Fatalf("ValidateForcedBackend(sixel, %v) = %v, want nil", enabled, err)
	}
	if err := ValidateForcedBackend(ModeKitty, enabled); err == nil {
		t.Fatalf("ValidateForcedBackend(kitty, %v) = nil, want error", enabled)
	}
	withoutHost := multiplexerBackends(muxTmux, "tmux 3.7b", true, true, false, terminalCompatibility{})
	if err := ValidateForcedBackend(ModeSixel, withoutHost); err == nil {
		t.Fatalf("ValidateForcedBackend(sixel, %v) = nil without Sixel host", withoutHost)
	}
}

// TestZellijSixelBlocked pins the zellij Sixel block end to end: with DA1
// advertised and real geometry the enabled cycle stays Block only, so
// auto-detection starts on Block, the backend cycle never reaches Sixel, and
// --backend=sixel fails the generic validator with the standard
// unsupported-backend error, no zellij-specific error path.
func TestZellijSixelBlocked(t *testing.T) {
	st := NewState(80, 24)
	st.SetEnabled(multiplexerBackends(muxZellij, "Zellij(0.43.1)", true, true, true, terminalCompatibility{}))
	if st.Backend != BackendBlock {
		t.Fatalf("zellij auto start = %v, want %v", st.Backend, BackendBlock)
	}
	if got := st.CycleBackend(); got != BackendBlock {
		t.Fatalf("backend cycle = %v, want %v", got, BackendBlock)
	}
	err := ValidateForcedBackend(ModeSixel, st.Enabled)
	if err == nil {
		t.Fatalf("ValidateForcedBackend(sixel, %v) = nil, want error", st.Enabled)
	}
	if want := `viewer: backend "sixel" is not supported by this terminal`; err.Error() != want {
		t.Fatalf("forced-backend error = %q, want %q", err.Error(), want)
	}
}
