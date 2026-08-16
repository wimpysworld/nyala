package viewer

import (
	"bytes"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestFootSixelSyncUpdateCompatibility pins the XTVERSION compatibility
// decision: Foot alone disables the fence, including its parenthesised version
// reply, while an absent identity and other terminal names retain normal
// synchronised updates.
func TestFootSixelSyncUpdateCompatibility(t *testing.T) {
	cases := []struct {
		termID string
		want   bool
	}{
		{"foot(1.27.0)", true},
		{"FOOT(1.27.0-extra)", true},
		{"foot", true},
		{"foot-extra 1.27.0", false},
		{"football 1.0", false},
		{"WezTerm 20240203", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := disableSixelSyncUpdates(tc.termID); got != tc.want {
			t.Fatalf("disableSixelSyncUpdates(%q) = %v, want %v", tc.termID, got, tc.want)
		}
	}
}

func TestRio04XTVERSIONRange(t *testing.T) {
	cases := []struct {
		identity string
		want     bool
	}{
		{"Rio 0.3.9", false},
		{"Rio 0.4.0", true},
		{"Rio 0.4.7", true},
		{"rio v0.4.9-beta.1", true},
		{"Rio 0.5.0", false},
		{"Rio", false},
		{"WezTerm 0.4.7", false},
	}
	for _, tc := range cases {
		t.Run(tc.identity, func(t *testing.T) {
			if got := rio04XTVERSION(tc.identity); got != tc.want {
				t.Fatalf("rio04XTVERSION(%q) = %v, want %v", tc.identity, got, tc.want)
			}
		})
	}
}

func TestRioCompatibilityIdentityPriorityAndFallback(t *testing.T) {
	cases := []struct {
		name     string
		termID   string
		environ  []string
		wantRio4 bool
	}{
		{"XTVERSION affected", "Rio 0.4.7", nil, true},
		{"XTVERSION fixed overrides Rio env", "Rio 0.5.0", []string{"TERM_PROGRAM=rio"}, false},
		{"other XTVERSION overrides Rio env", "WezTerm 20240203", []string{"TERM=rio"}, false},
		{"TERM_PROGRAM fallback", "", []string{"TERM_PROGRAM=rio"}, true},
		{"TERM fallback", "", []string{"TERM=rio"}, true},
		{"multiplexer keeps fallback", "", []string{"TMUX=/tmp/tmux", "TERM=screen-256color", "TERM_PROGRAM=rio"}, true},
		{"unrelated terminal", "", []string{"TERM=xterm-256color"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTerminalCompatibility(tc.termID, tc.environ)
			if got.rio04 != tc.wantRio4 {
				t.Fatalf("Rio policy = %v, want %v", got.rio04, tc.wantRio4)
			}
		})
	}
}

func TestRioBackendCompatibility(t *testing.T) {
	detected := probeResult{Kitty: true, Sixel: true}
	normal := enabledBackends(detected, terminalCompatibility{})
	if len(normal) != 3 || normal[0] != BackendKitty || normal[1] != BackendSixel || normal[2] != BackendBlock {
		t.Fatalf("normal backends = %v, want Kitty, Sixel, Block", normal)
	}

	rio := enabledBackends(detected, terminalCompatibility{rio04: true})
	if len(rio) != 2 || rio[0] != BackendKitty || rio[1] != BackendBlock {
		t.Fatalf("Rio backends = %v, want Kitty, Block", rio)
	}

	st := NewState(80, 24)
	st.compat.rio04 = true
	st.SetEnabled(rio)
	err := st.validateForcedBackend(ModeSixel)
	if err == nil || !strings.Contains(err.Error(), "Rio 0.4.x") || !strings.Contains(err.Error(), "--backend=kitty") || !strings.Contains(err.Error(), "--backend=block") {
		t.Fatalf("forced Rio Sixel error = %v", err)
	}
}

func TestRioColourCompatibility(t *testing.T) {
	compat := terminalCompatibility{rio04: true}
	if got := resolveCompatibleColour("auto", nil, ColourTrue, compat); got != ColourXTerm256 {
		t.Fatalf("Rio auto colour = %v, want xterm-256", got)
	}
	if got := resolveCompatibleColour("auto", []string{"NO_COLOR="}, ColourTrue, compat); got != ColourMono {
		t.Fatalf("Rio auto with NO_COLOR = %v, want mono", got)
	}
	if got := resolveCompatibleColour("256", nil, ColourTrue, compat); got != ColourXTerm256 {
		t.Fatalf("Rio explicit 256 = %v, want xterm-256", got)
	}
	if got := resolveCompatibleColour("none", nil, ColourTrue, compat); got != ColourMono {
		t.Fatalf("Rio explicit none = %v, want mono", got)
	}
	if got := resolveCompatibleColour("truecolor", nil, ColourXTerm256, compat); got != ColourTrue {
		t.Fatalf("Rio explicit truecolor = %v, want visible for validation", got)
	}

	// Alt+R cycles every supported backend from any --backend start, so explicit
	// truecolour is rejected regardless of the starting backend, Kitty included.
	st := NewState(80, 24)
	st.compat = compat
	if err := st.validateColourCompatibility("truecolor"); err == nil || !strings.Contains(err.Error(), "truecolour Block") {
		t.Fatalf("Rio truecolor error = %v", err)
	}
	for _, colour := range []string{"auto", "256", "none"} {
		if err := st.validateColourCompatibility(colour); err != nil {
			t.Fatalf("colour %s rejected: %v", colour, err)
		}
	}
	if err := (&State{}).validateColourCompatibility("truecolor"); err != nil {
		t.Fatalf("non-Rio truecolor rejected: %v", err)
	}
}

// TestRioKittyTimingUsesBarLikeAnyTerminal pins the deletion of the Rio FPS
// suppression: the fps segment lives on the bar row, outside the page image
// on every backend, so a timed Rio Kitty frame carries the below-text
// z-index and the bar fps text like any other terminal, and the meter keeps
// sampling.
func TestRioKittyTimingUsesBarLikeAnyTerminal(t *testing.T) {
	// 60 columns give the bar room for its full chrome, fps segment included.
	st := NewState(60, 4)
	st.setTerminalCompatibility("Rio 0.4.7", nil)
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	meter := &fpsMeter{window: fpsWindow, start: time.Now()}
	s := &streamSession{st: &st, diff: diff, fps: meter}
	kittyFrame := render.Frame{
		EncodedPNG: encodedPNG(t, image.NewRGBA(image.Rect(0, 0, 8, 8))),
		AfterFCP:   true,
	}

	var kitty bytes.Buffer
	if _, err := s.assemble(&kitty, kittyFrame); err != nil {
		t.Fatalf("assemble Rio Kitty frame: %v", err)
	}
	if !strings.Contains(kitty.String(), "z=") {
		t.Fatalf("timed Rio Kitty frame lost the below-text z-index: %q", kitty.String())
	}
	if !strings.Contains(kitty.String(), "↑ ") {
		t.Fatalf("Rio Kitty frame lost the bar fps segment: %q", kitty.String())
	}
	if meter.frames != 1 {
		t.Fatalf("Rio Kitty sampled frames = %d, want 1", meter.frames)
	}

	st.CycleBackend()
	var blockOut bytes.Buffer
	blockFrame := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16)), AfterFCP: true}
	if _, err := s.assemble(&blockOut, blockFrame); err != nil {
		t.Fatalf("assemble Rio Block frame: %v", err)
	}
	if !strings.Contains(blockOut.String(), "↑ ") {
		t.Fatal("Rio Block frame lost the bar fps segment")
	}
	if meter.frames != 2 {
		t.Fatalf("sampled frames after cycling = %d, want 2", meter.frames)
	}

	var summary strings.Builder
	meter.printSummary(&summary)
	if !strings.Contains(summary.String(), "out_frames=2") {
		t.Fatalf("Rio timing summary = %q, want retained final summary", summary.String())
	}
}

// TestTitlePopIgnoredCompatibility pins the title-pop compatibility policy:
// WezTerm and Konsole parse the XTWINOPS title push and pop but do not
// restore the saved title, so teardown must follow the pop with an explicit
// neutral title. The match reads only the XTVERSION identity; TERM never
// gates the policy.
func TestTitlePopIgnoredCompatibility(t *testing.T) {
	cases := []struct {
		name    string
		termID  string
		environ []string
		want    bool
	}{
		{"wezterm", "WezTerm 20240203-110809-5046fc22", nil, true},
		{"konsole", "Konsole 23.08.5", nil, true},
		{"kitty", "kitty(0.32.2)", nil, false},
		{"foot", "foot(1.16.2)", nil, false},
		{"no identity", "", nil, false},
		{"TERM never gates", "", []string{"TERM=wezterm", "TERM_PROGRAM=WezTerm"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTerminalCompatibility(tc.termID, tc.environ)
			if got.titlePopIgnored != tc.want {
				t.Fatalf("resolveTerminalCompatibility(%q).titlePopIgnored = %v, want %v", tc.termID, got.titlePopIgnored, tc.want)
			}
		})
	}
}
