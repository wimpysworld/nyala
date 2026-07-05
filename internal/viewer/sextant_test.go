package viewer

import "testing"

// TestSextantSupported asserts the allow-list verdict for each supported
// terminal's representative identity var, for iTerm2 (matched but
// unsupported), for an unmatched terminal, and for a multiplexer-plus-kitty
// case: TMUX rewrites TERM to screen-256color, but KITTY_WINDOW_ID still wins
// because specific vars are checked ahead of TERM.
func TestSextantSupported(t *testing.T) {
	cases := []struct {
		name          string
		environ       []string
		wantName      string
		wantSupported bool
	}{
		{"kitty", []string{"KITTY_WINDOW_ID=1"}, "kitty", true},
		{"wezterm", []string{"WEZTERM_EXECUTABLE=/usr/bin/wezterm"}, "wezterm", true},
		{"ghostty specific var", []string{"GHOSTTY_RESOURCES_DIR=/usr/share/ghostty"}, "ghostty", true},
		{"alacritty", []string{"ALACRITTY_SOCKET=/tmp/sock"}, "alacritty", true},
		{"konsole", []string{"KONSOLE_VERSION=230400"}, "konsole", true},
		{"vte", []string{"VTE_VERSION=6800"}, "vte", true},
		{"wezterm term program", []string{"TERM_PROGRAM=WezTerm"}, "wezterm", true},
		{"ghostty term program", []string{"TERM_PROGRAM=ghostty"}, "ghostty", true},
		{"contour term program", []string{"TERM_PROGRAM=contour"}, "contour", true},
		{"rio term program", []string{"TERM_PROGRAM=rio"}, "rio", true},
		{"foot via TERM", []string{"TERM=foot"}, "foot", true},
		{"foot-extra via TERM", []string{"TERM=foot-extra"}, "foot", true},
		{"kitty via TERM", []string{"TERM=xterm-kitty"}, "kitty", true},
		{"iterm2 unsupported", []string{"TERM_PROGRAM=iTerm.app"}, "iterm2", false},
		{"unmatched terminal", []string{"TERM=xterm-256color"}, "unknown", false},
		{"empty env", nil, "unknown", false},
		{
			"multiplexer plus kitty",
			[]string{"TMUX=/tmp/tmux-1000/default,1234,0", "TERM=screen-256color", "KITTY_WINDOW_ID=1"},
			"kitty",
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotSupported := SextantSupported(c.environ)
			if gotName != c.wantName || gotSupported != c.wantSupported {
				t.Fatalf("SextantSupported(%v) = (%q, %v), want (%q, %v)",
					c.environ, gotName, gotSupported, c.wantName, c.wantSupported)
			}
		})
	}
}

// TestSextantDetectTerminalUnmatched asserts detectTerminal returns "" (not
// "unknown") for env with no identifying var, since SextantSupported, not
// detectTerminal, owns the "unknown" label.
func TestSextantDetectTerminalUnmatched(t *testing.T) {
	if got := detectTerminal([]string{"TERM=xterm-256color"}); got != "" {
		t.Fatalf("detectTerminal(...) = %q, want \"\"", got)
	}
}
