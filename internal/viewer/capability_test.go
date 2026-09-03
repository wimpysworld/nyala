package viewer

import (
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// exampleAboutState builds the State that yields exampleAboutSnapshot: Kitty
// and Block in the cycle, the shm transport, sextant self-drawn, truecolour,
// and a kitty 0.4.0 identity from the handshake.
func exampleAboutState() State {
	st := NewState(160, 24)
	st.probe = probeResult{Kitty: true, KittySHM: true, TermID: "kitty 0.4.0"}
	st.SetEnabled(enabledBackends(st.probe, resolveTerminalCompatibility(st.probe.TermID, nil)))
	st.kittyTransport = resolveKittyTransport("", st.probe.TermID, st.probe.KittySHM, "linux")
	st.sextantOK = true
	return st
}

// TestCapabilitySnapshotExample pins the snapshot for the specified run:
// State.capabilitySnapshot on the example State equals exampleAboutSnapshot
// field for field, so the row test and the state test agree on one source.
func TestCapabilitySnapshotExample(t *testing.T) {
	st := exampleAboutState()
	got := st.capabilitySnapshot("dev", "Chrome 150.0.7204.49", render.WebGLState{Tier: render.WebGL2, Label: "Intel"})
	if want := exampleAboutSnapshot(); got != want {
		t.Fatalf("capabilitySnapshot =\n%+v\nwant\n%+v", got, want)
	}
}

// TestCapabilitySnapshotReportsEnabledNotProbe pins the effective-decision
// rule: Kitty and Sixel come from Enabled after every policy gate, not from
// the raw probe, so a terminal that answered the Kitty query but lost the
// backend to a multiplexer block shows ✗ Kitty, and a Sixel capability blocked
// by tmux or by the Rio 0.4 compatibility policy shows ✗ Sixel.
func TestCapabilitySnapshotReportsEnabledNotProbe(t *testing.T) {
	sixelKitty := probeResult{Kitty: true, Sixel: true}
	for _, tc := range []struct {
		name    string
		enabled []Backend
		kitty   capabilityState
		sixel   capabilityState
	}{
		{
			"probe kitty but multiplexer block",
			multiplexerBackends(muxZellij, "kitty 0.40.1", true, true, terminalCompatibility{}),
			capOff, capOff,
		},
		{
			"tmux 3.7 offers sixel and never kitty",
			multiplexerBackends(muxTmux, "tmux 3.7a", true, true, terminalCompatibility{}),
			capOff, capOn,
		},
		{
			"tmux 3.5 blocks sixel",
			multiplexerBackends(muxTmux, "tmux 3.5a", true, true, terminalCompatibility{}),
			capOff, capOff,
		},
		{
			"tmux blocks sixel without real geometry",
			multiplexerBackends(muxTmux, "tmux 3.7a", true, false, terminalCompatibility{}),
			capOff, capOff,
		},
		{
			"rio 0.4 policy blocks sixel",
			enabledBackends(sixelKitty, resolveTerminalCompatibility("Rio 0.4.7", nil)),
			capOn, capOff,
		},
		{
			"rio environment fallback blocks sixel",
			enabledBackends(sixelKitty, resolveTerminalCompatibility("", []string{"TERM_PROGRAM=rio"})),
			capOn, capOff,
		},
		{
			"direct terminal keeps both",
			enabledBackends(sixelKitty, resolveTerminalCompatibility("kitty 0.40.1", nil)),
			capOn, capOn,
		},
		{
			"block only",
			enabledBackends(probeResult{}, terminalCompatibility{}),
			capOff, capOff,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(160, 24)
			st.probe = sixelKitty
			st.SetEnabled(tc.enabled)
			snap := st.capabilitySnapshot("dev", "", render.WebGLState{})
			if snap.Kitty != tc.kitty || snap.Sixel != tc.sixel {
				t.Fatalf("snapshot kitty=%d sixel=%d, want kitty=%d sixel=%d for enabled %v",
					snap.Kitty, snap.Sixel, tc.kitty, tc.sixel, tc.enabled)
			}
			row := aboutRowText(snap)
			for _, want := range []string{
				stateGlyph(tc.kitty) + " Kitty", stateGlyph(tc.sixel) + " Sixel",
			} {
				if !strings.Contains(row, want) {
					t.Fatalf("About fields %q missing %q", row, want)
				}
			}
			// Both backends are decided, so neither ever renders as unknown.
			if strings.Contains(row, aboutGlyphUnknown+" Kitty") || strings.Contains(row, aboutGlyphUnknown+" Sixel") {
				t.Fatalf("a decided capability rendered as unknown: %q", row)
			}
		})
	}
}

// aboutRowText joins the About fields for one snapshot at a width that keeps
// them all, so a test can search the rendered text for a field.
func aboutRowText(snap capabilitySnapshot) string {
	content, _, _ := renderBarAbout(400, aboutFields(snap))
	return content
}

// TestCapabilitySnapshotKittyTransport pins the transport text: the resolved
// shm transport shows as (shm), the direct fallback as (direct), and an off
// Kitty shows no transport at all whatever the resolver chose.
func TestCapabilitySnapshotKittyTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport KittyTransport
		enabled   []Backend
		want      string
	}{
		{"shm from the probe", resolveKittyTransport("", "kitty 0.40.1", true, "linux"), []Backend{BackendKitty, BackendBlock}, "✓ Kitty (shm)"},
		{"direct fallback", resolveKittyTransport("", "", false, "linux"), []Backend{BackendKitty, BackendBlock}, "✓ Kitty (direct)"},
		{"direct override beats the probe", resolveKittyTransport("direct", "kitty 0.40.1", true, "linux"), []Backend{BackendKitty, BackendBlock}, "✓ Kitty (direct)"},
		{"off hides the transport", KittyTransportSHM, []Backend{BackendBlock}, "✗ Kitty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(160, 24)
			st.SetEnabled(tc.enabled)
			st.kittyTransport = tc.transport
			snap := st.capabilitySnapshot("dev", "", render.WebGLState{})
			if snap.KittyTransport != tc.transport {
				t.Fatalf("snapshot transport = %q, want %q", snap.KittyTransport, tc.transport)
			}
			row := aboutRowText(snap)
			if !strings.Contains(row, tc.want+aboutFieldSep) {
				t.Fatalf("About fields %q missing %q as a whole field", row, tc.want)
			}
		})
	}
}

// TestCapabilitySnapshotSextant pins the sextant gate: the resolved
// self-draw answer maps onto ✓ Sextant or ✗ Sextant.
func TestCapabilitySnapshotSextant(t *testing.T) {
	for _, ok := range []bool{true, false} {
		st := NewState(160, 24)
		st.sextantOK = ok
		snap := st.capabilitySnapshot("dev", "", render.WebGLState{})
		if want := boolState(ok); snap.Sextant != want {
			t.Fatalf("sextantOK=%t snapshot = %d, want %d", ok, snap.Sextant, want)
		}
		if row, want := aboutRowText(snap), stateGlyph(boolState(ok))+" Sextant"; !strings.Contains(row, want) {
			t.Fatalf("sextantOK=%t About fields %q missing %q", ok, row, want)
		}
	}
}

// TestCapabilitySnapshotColourLabel pins the colour depth text for each mode
// as the snapshot carries it and as the row shows it, and the label for an
// out-of-range mode, which reads as the NewState default.
func TestCapabilitySnapshotColourLabel(t *testing.T) {
	for _, tc := range []struct {
		mode ColourMode
		want string
	}{
		{ColourTrue, "True Colour"},
		{ColourXTerm256, "256 Colour"},
		{ColourXTerm256Adaptive, "256 Adaptive"},
		{ColourXTermGrey, "Greyscale"},
		{ColourMode(99), "True Colour"},
	} {
		if got := colourLabel(tc.mode); got != tc.want {
			t.Fatalf("colourLabel(%d) = %q, want %q", tc.mode, got, tc.want)
		}
		st := NewState(160, 24)
		st.Colour = tc.mode
		snap := st.capabilitySnapshot("dev", "", render.WebGLState{})
		if snap.Colour != tc.mode {
			t.Fatalf("snapshot colour = %d, want %d", snap.Colour, tc.mode)
		}
		if row := aboutRowText(snap); !strings.Contains(row, "Colour: "+tc.want) {
			t.Fatalf("About fields %q missing %q", row, "Colour: "+tc.want)
		}
	}
}

// TestBrowserField pins the product shortening: the name keeps its major
// version only, free text without a numeric version passes through, the
// result is trimmed, and control bytes never survive.
func TestBrowserField(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"Chrome 138.0.7204.49", "Chrome 138"},
		{"Chromium 150.0.1", "Chromium 150"},
		{"Chrome 150", "Chrome 150"},
		{"HeadlessChrome", "HeadlessChrome"},
		{"Chrome beta", "Chrome beta"},
		{"Chrome ", "Chrome"},
		{"  Chromium 150.0.1  ", "Chromium 150"},
		{"", ""},
		{"Chrome\x1b\u009b 138.0.1\x07", "Chrome 138"},
		{"Chrome\x1b[31m 138.0.1", "Chrome[31m 138"},
	} {
		if got := browserField(tc.in); got != tc.want {
			t.Fatalf("browserField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTerminalField pins the identity display text: the first letter is
// upper-cased and nothing else changes, an empty or blank identity stays
// empty so the row omits the field, and control bytes never survive.
func TestTerminalField(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"kitty 0.40.1", "Kitty 0.40.1"},
		{"WezTerm 20240203", "WezTerm 20240203"},
		{"foot(1.16.2)", "Foot(1.16.2)"},
		{"", ""},
		{"   ", ""},
		{"\x1b\u009bkitty\x07 0.4.0\x7f", "Kitty 0.4.0"},
		{"\u009b", ""},
	} {
		if got := terminalField(tc.in); got != tc.want {
			t.Fatalf("terminalField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCapabilitySnapshotStripsControls pins the snapshot as the one stripping
// point: the version, the browser product, the terminal identity, and the
// WebGL label all lose ESC, CSI, C0, DEL, and C1 bytes before the bar sees
// them, so every string in the snapshot is safe display text.
func TestCapabilitySnapshotStripsControls(t *testing.T) {
	const smuggled = "\x1b[31m\u009b\x07\x01\x7f"
	st := NewState(160, 24)
	st.probe.TermID = "kitty" + smuggled + " 0.4.0"
	snap := st.capabilitySnapshot("dev"+smuggled, "Chrome"+smuggled+" 150.0.1", render.WebGLState{Tier: render.WebGL2, Label: "Intel" + smuggled})
	for name, got := range map[string]string{
		"version":  snap.Version,
		"browser":  snap.Browser,
		"terminal": snap.Terminal,
		"label":    snap.WebGL.Label,
	} {
		if strings.ContainsFunc(got, isControlRune) {
			t.Fatalf("snapshot %s kept a control rune: %q", name, got)
		}
	}
	if snap.Version != "dev[31m" || snap.Browser != "Chrome[31m 150" || snap.Terminal != "Kitty[31m 0.4.0" || snap.WebGL.Label != "Intel[31m" {
		t.Fatalf("stripped snapshot = %+v", snap)
	}
}

// TestBackendStateAndBoolState pins the two small mappers the snapshot uses:
// a backend is on only when it is in the enabled list, an empty list reports
// off, and a bool gate maps onto the two decided states.
func TestBackendStateAndBoolState(t *testing.T) {
	enabled := []Backend{BackendSixel, BackendBlock}
	if got := backendState(enabled, BackendSixel); got != capOn {
		t.Fatalf("backendState(sixel in list) = %d, want on", got)
	}
	if got := backendState(enabled, BackendKitty); got != capOff {
		t.Fatalf("backendState(kitty not in list) = %d, want off", got)
	}
	if got := backendState(nil, BackendBlock); got != capOff {
		t.Fatalf("backendState(empty list) = %d, want off", got)
	}
	if boolState(true) != capOn || boolState(false) != capOff {
		t.Fatalf("boolState = %d/%d, want on/off", boolState(true), boolState(false))
	}
}
