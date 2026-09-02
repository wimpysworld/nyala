package viewer

import (
	"io"
	"strings"

	"github.com/charmbracelet/colorprofile"
)

// detectColour resolves the terminal colour mode from env plus terminfo using
// colorprofile.Detect. The function is hermetic: environ is injected and the
// writer is always io.Discard, so it never reads os.Stdout. isTTY drives Detect's
// terminal treatment through env, not the writer: Detect derives tty-ness from
// isTTYForced(environ) || (output is a term.File and a terminal), and
// isTTYForced reads TTY_FORCE via strconv.ParseBool. So when isTTY is true,
// detectColour appends TTY_FORCE=1 to a copy of environ, forcing Detect to honour
// terminfo without a live terminal. When isTTY is false the io.Discard writer
// with no TTY_FORCE yields NoTTY, so detectColour(nil, false) returns
// ColourXTerm256 with no exec.
//
// The profile maps exhaustively: TrueColor -> ColourTrue and every other
// profile -> ColourXTerm256, the floor mode: every remaining colour mode
// needs 256-colour support, and every modern terminal renders indexed
// colour. COLORTERM overrides first, because
// colorprofile upgrades on COLORTERM only when TERM is also set and non-dumb, but
// Nyala treats a present COLORTERM as a trusted positive regardless of TERM:
// kmscon (which colorprofile caps at ANSI) and the truecolor/24bit tokens both
// force ColourTrue. This is the one place Nyala must not defer to colorprofile.
//
// Side effect: under tmux, colorprofile.Detect execs "tmux info" once to read the
// user's Tc/RGB override. Injecting environ keeps tests off that exec; the live
// colour path is multiplexer-agnostic, so the exec fires under tmux even when
// graphics are gated off.
func detectColour(environ []string, isTTY bool) ColourMode {
	switch colorterm(environ) {
	case "kmscon", "truecolor", "24bit":
		return ColourTrue
	}
	env := environ
	if isTTY {
		env = append(append([]string(nil), environ...), "TTY_FORCE=1")
	}
	return colourProfileMode(colorprofile.Detect(io.Discard, env))
}

func colourProfileMode(profile colorprofile.Profile) ColourMode {
	if profile == colorprofile.TrueColor {
		return ColourTrue
	}
	return ColourXTerm256
}

// resolveColour applies the colour precedence flag > NO_COLOR > detected. An
// explicit flag wins and selects its exact mode. Under auto, a NO_COLOR key
// present in environ (any value, including empty) yields the greyscale ramp,
// the least colourful mode Nyala still renders: greyscale output still emits
// SGR colour codes. The presence check scans environ directly so the
// no-color.org "any value" rule wins over colorprofile, which honours only a
// truthy NO_COLOR. Otherwise auto returns the detected mode.
func resolveColour(flag string, environ []string, detected ColourMode) ColourMode {
	switch flag {
	case "truecolor":
		return ColourTrue
	case "256":
		return ColourXTerm256
	case "ansi-grey":
		return ColourXTermGrey
	}
	if noColorPresent(environ) {
		return ColourXTermGrey
	}
	return detected
}

// colorterm returns the COLORTERM value from environ, lowercased, or "" when
// absent. It scans environ directly so the lookup matches the injected env.
func colorterm(environ []string) string {
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok && name == "COLORTERM" {
			return strings.ToLower(value)
		}
	}
	return ""
}

// noColorPresent reports whether environ carries a NO_COLOR key, regardless of
// its value. The no-color.org spec treats any value, including empty, as set.
func noColorPresent(environ []string) bool {
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); name == "NO_COLOR" {
			return true
		}
	}
	return false
}
