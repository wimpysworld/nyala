package viewer

import "strings"

// sextantSupportedNames is the static allow-list of terminals known to render
// sextant blocks correctly. iTerm2 is deliberately absent: its 3.5.x line carries
// a vertical-gap seam between sextant glyphs, so it is unsupported pending a
// version gate.
var sextantSupportedNames = map[string]bool{
	"kitty":     true,
	"wezterm":   true,
	"ghostty":   true,
	"foot":      true,
	"alacritty": true,
	"konsole":   true,
	"vte":       true,
	"contour":   true,
	"rio":       true,
}

// sextantSpecificPrefixes maps an env var name prefix to the terminal it
// identifies. Presence of any key with the prefix is a hit, regardless of
// value. Order matters: this is the highest-priority match tier, checked ahead
// of TERM_PROGRAM and TERM.
var sextantSpecificPrefixes = []struct {
	prefix string
	name   string
}{
	{"KITTY_", "kitty"},
	{"WEZTERM_", "wezterm"},
	{"GHOSTTY_", "ghostty"},
	{"ALACRITTY_", "alacritty"},
	{"KONSOLE_", "konsole"},
}

// sextantTermProgram maps a TERM_PROGRAM value to the terminal it identifies.
var sextantTermProgram = map[string]string{
	"WezTerm":   "wezterm",
	"ghostty":   "ghostty",
	"contour":   "contour",
	"rio":       "rio",
	"iTerm.app": "iterm2",
}

// sextantTerm maps a TERM value to the terminal it identifies.
var sextantTerm = map[string]string{
	"xterm-kitty": "kitty",
	"foot":        "foot",
	"foot-extra":  "foot",
}

// detectTerminal identifies the running terminal emulator from env, an
// os.Environ()-shaped []string of "KEY=VALUE" entries. It never reads os.Environ
// itself, so it stays hermetic and testable with canned env. Match order,
// first hit wins: terminal-specific vars (KITTY_*, WEZTERM_*, GHOSTTY_*,
// ALACRITTY_*, KONSOLE_*, VTE_VERSION), then TERM_PROGRAM, then TERM. Specific
// vars are checked first because a multiplexer (tmux, screen) commonly
// rewrites TERM and TERM_PROGRAM but leaves the outer terminal's identity vars
// untouched. Returns "" when nothing matches.
func detectTerminal(env []string) string {
	names := make(map[string]string, len(env))
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok {
			names[name] = value
		}
	}

	for _, p := range sextantSpecificPrefixes {
		for name := range names {
			if strings.HasPrefix(name, p.prefix) {
				return p.name
			}
		}
	}
	if _, ok := names["VTE_VERSION"]; ok {
		return "vte"
	}

	if term, ok := sextantTermProgram[names["TERM_PROGRAM"]]; ok {
		return term
	}

	if term, ok := sextantTerm[names["TERM"]]; ok {
		return term
	}

	return ""
}

// SextantSupported reports whether the terminal identified in env is known to
// render sextant blocks correctly. It composes detectTerminal with the static
// allow-list: an unmatched terminal reports name "unknown" and supported
// false; a matched but disallowed terminal (such as iTerm2) reports its name
// and supported false; a matched, allowed terminal reports its name and
// supported true.
func SextantSupported(env []string) (name string, supported bool) {
	name = detectTerminal(env)
	if name == "" {
		return "unknown", false
	}
	return name, sextantSupportedNames[name]
}
