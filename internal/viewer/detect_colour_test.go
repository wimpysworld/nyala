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
// lower profile maps to ColourMono.
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
		{"colorterm absent without tty", []string{"TERM=xterm-256color"}, false, ColourMono},
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
// ColourMono without panicking. isTTY=false routes Detect to io.Discard,
// so there is no live terminal and no exec.
func TestDetectColourNilNoTTY(t *testing.T) {
	if got := detectColour(nil, false); got != ColourMono {
		t.Fatalf("detectColour(nil, false) = %v, want %v", got, ColourMono)
	}
}

func TestColourProfileMode(t *testing.T) {
	cases := []struct {
		profile colorprofile.Profile
		want    ColourMode
	}{
		{colorprofile.TrueColor, ColourTrue},
		{colorprofile.ANSI256, ColourXTerm256},
		{colorprofile.ANSI, ColourANSITonal},
		{colorprofile.ASCII, ColourMono},
		{colorprofile.NoTTY, ColourMono},
		{colorprofile.Unknown, ColourMono},
	}
	for _, c := range cases {
		if got := colourProfileMode(c.profile); got != c.want {
			t.Errorf("colourProfileMode(%v) = %v, want %v", c.profile, got, c.want)
		}
	}
}

// TestResolveColour asserts the precedence flag > NO_COLOR > detected. The
// explicit flags override both NO_COLOR and detection. Under auto, a present
// NO_COLOR (any value, including empty and "0") yields ColourMono, proving the
// viewer owns the no-color.org "any value" rule. Under auto with no NO_COLOR,
// the detected mode wins.
func TestResolveColour(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		environ  []string
		detected ColourMode
		want     ColourMode
	}{
		{"flag truecolor overrides no_color", "truecolor", []string{"NO_COLOR=1"}, ColourMono, ColourTrue},
		{"flag truecolor overrides detection", "truecolor", nil, ColourMono, ColourTrue},
		{"flag 256 overrides no_color", "256", []string{"NO_COLOR=1"}, ColourTrue, ColourXTerm256},
		{"flag 256 overrides detection", "256", nil, ColourTrue, ColourXTerm256},
		{"flag ansi grey selects exactly", "ansi-grey", nil, ColourTrue, ColourXTermGrey},
		{"flag ansi tonal selects exactly", "ansi-tonal", nil, ColourTrue, ColourANSITonal},
		{"flag none overrides detection", "none", nil, ColourTrue, ColourMono},
		{"auto no_color key only", "auto", []string{"NO_COLOR"}, ColourTrue, ColourMono},
		{"auto no_color present", "auto", []string{"NO_COLOR=1"}, ColourTrue, ColourMono},
		{"auto no_color empty", "auto", []string{"NO_COLOR="}, ColourTrue, ColourMono},
		{"auto no_color zero", "auto", []string{"NO_COLOR=0"}, ColourTrue, ColourMono},
		{"auto no no_color returns detected true", "auto", nil, ColourTrue, ColourTrue},
		{"auto no no_color returns detected 256", "auto", []string{"TERM=xterm"}, ColourXTerm256, ColourXTerm256},
		{"auto does not select detected grey", "auto", nil, ColourXTermGrey, ColourMono},
		{"auto does not select detected tonal", "auto", nil, ColourANSITonal, ColourMono},
		{"auto no no_color returns detected mono", "auto", nil, ColourMono, ColourMono},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveColour(c.flag, c.environ, c.detected); got != c.want {
				t.Fatalf("resolveColour(%q, %v, %v) = %v, want %v", c.flag, c.environ, c.detected, got, c.want)
			}
		})
	}
}
