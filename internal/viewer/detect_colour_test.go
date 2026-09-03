package viewer

import (
	"testing"

	"github.com/charmbracelet/colorprofile"
)

// TestDetectColour asserts the COLORTERM matrix and the no-env path. environ is
// injected so the function stays pure. No case sets a tmux env var, so
// colorprofile.Detect execs no subprocess.
//
// detectColour is hermetic: it always passes io.Discard to Detect and, when
// isTTY is true, appends TTY_FORCE=1 to a copy of environ so Detect honours
// COLORTERM without a live terminal. So COLORTERM=truecolor and 24bit resolve to
// ColourTrue under isTTY=true. The kmscon row shortcuts before Detect, so it is
// TTY-free. With isTTY=false there is no TTY_FORCE, so Detect derives NoTTY and a
// lower profile maps to ColourXTerm256, the floor mode.
func TestDetectColour(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		isTTY   bool
		want    ColourMode
	}{
		{"colorterm kmscon override", []string{"TERM=xterm", "COLORTERM=kmscon"}, false, ColourTrue},
		{"colorterm truecolor tty", []string{"TERM=xterm-256color", "COLORTERM=truecolor"}, true, ColourTrue},
		{"colorterm 24bit tty", []string{"TERM=xterm-256color", "COLORTERM=24bit"}, true, ColourTrue},
		{"colorterm absent without tty", []string{"TERM=xterm-256color"}, false, ColourXTerm256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectColour(c.environ, c.isTTY); got != c.want {
				t.Fatalf("detectColour(%v, %v) = %v, want %v", c.environ, c.isTTY, got, c.want)
			}
		})
	}
}

// TestDetectColourNilNoTTY asserts detectColour(nil, false) returns
// ColourXTerm256 without panicking. isTTY=false routes Detect to io.Discard,
// so there is no live terminal and no exec.
func TestDetectColourNilNoTTY(t *testing.T) {
	if got := detectColour(nil, false); got != ColourXTerm256 {
		t.Fatalf("detectColour(nil, false) = %v, want %v", got, ColourXTerm256)
	}
}

// TestAutoNeverResolvesAdaptiveColour pins the explicit-only rule: the
// adaptive xterm-256 mode writes the terminal palette, so nothing but the
// --color=256-adaptive flag may select it. The sweep covers every detected
// mode, the NO_COLOR branch, and the Rio compatibility cap, and it checks
// detection itself, which reports only ColourTrue or ColourXTerm256.
func TestAutoNeverResolvesAdaptiveColour(t *testing.T) {
	environs := [][]string{nil, {"TERM=xterm-256color"}, {"NO_COLOR=1"}, {"TERM=xterm", "COLORTERM=truecolor"}}
	detecteds := []ColourMode{ColourTrue, ColourXTerm256, ColourXTermGrey}
	compats := []terminalCompatibility{{}, {rio04: true}}
	for _, environ := range environs {
		for _, detected := range detecteds {
			if got := resolveColour("auto", environ, detected); got == ColourXTerm256Adaptive {
				t.Fatalf("resolveColour(auto, %v, %v) selected the adaptive mode", environ, detected)
			}
			for _, compat := range compats {
				if got := resolveCompatibleColour("auto", environ, detected, compat); got == ColourXTerm256Adaptive {
					t.Fatalf("resolveCompatibleColour(auto, %v, %v, %+v) selected the adaptive mode", environ, detected, compat)
				}
			}
		}
		for _, isTTY := range []bool{false, true} {
			if got := detectColour(environ, isTTY); got == ColourXTerm256Adaptive {
				t.Fatalf("detectColour(%v, %v) reported the adaptive mode", environ, isTTY)
			}
		}
	}
}

func TestColourProfileMode(t *testing.T) {
	cases := []struct {
		profile colorprofile.Profile
		want    ColourMode
	}{
		{colorprofile.TrueColor, ColourTrue},
		{colorprofile.ANSI256, ColourXTerm256},
		{colorprofile.ANSI, ColourXTerm256},
		{colorprofile.ASCII, ColourXTerm256},
		{colorprofile.NoTTY, ColourXTerm256},
		{colorprofile.Unknown, ColourXTerm256},
	}
	for _, c := range cases {
		if got := colourProfileMode(c.profile); got != c.want {
			t.Errorf("colourProfileMode(%v) = %v, want %v", c.profile, got, c.want)
		}
	}
}

// TestResolveColour asserts the precedence flag > NO_COLOR > detected. The
// explicit flags override both NO_COLOR and detection. Under auto, a present
// NO_COLOR (any value, including empty and "0") yields the greyscale ramp,
// proving the viewer owns the no-color.org "any value" rule. Under auto with
// no NO_COLOR, the detected mode wins.
func TestResolveColour(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		environ  []string
		detected ColourMode
		want     ColourMode
	}{
		{"flag truecolor overrides no_color", "truecolor", []string{"NO_COLOR=1"}, ColourXTerm256, ColourTrue},
		{"flag truecolor overrides detection", "truecolor", nil, ColourXTerm256, ColourTrue},
		{"flag 256 overrides no_color", "256", []string{"NO_COLOR=1"}, ColourTrue, ColourXTerm256},
		{"flag 256 overrides detection", "256", nil, ColourTrue, ColourXTerm256},
		{"flag grey selects exactly", "grey", nil, ColourTrue, ColourXTermGrey},
		{"flag 256-adaptive selects exactly", "256-adaptive", nil, ColourTrue, ColourXTerm256Adaptive},
		{"flag 256-adaptive overrides no_color", "256-adaptive", []string{"NO_COLOR=1"}, ColourTrue, ColourXTerm256Adaptive},
		{"auto no_color key only", "auto", []string{"NO_COLOR"}, ColourTrue, ColourXTermGrey},
		{"auto no_color present", "auto", []string{"NO_COLOR=1"}, ColourTrue, ColourXTermGrey},
		{"auto no_color empty", "auto", []string{"NO_COLOR="}, ColourTrue, ColourXTermGrey},
		{"auto no_color zero", "auto", []string{"NO_COLOR=0"}, ColourTrue, ColourXTermGrey},
		{"auto no_color on a 256 profile", "auto", []string{"NO_COLOR=1"}, ColourXTerm256, ColourXTermGrey},
		{"auto no no_color returns detected true", "auto", nil, ColourTrue, ColourTrue},
		{"auto no no_color returns detected 256", "auto", []string{"TERM=xterm"}, ColourXTerm256, ColourXTerm256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveColour(c.flag, c.environ, c.detected); got != c.want {
				t.Fatalf("resolveColour(%q, %v, %v) = %v, want %v", c.flag, c.environ, c.detected, got, c.want)
			}
		})
	}
}
