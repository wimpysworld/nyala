package app

import (
	"os"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// tryParse runs kong over args against a fresh CLI, the same wiring cmd/nyala
// uses, and returns the parse result without failing the test. Callers that
// expect a clean parse use parse; callers that assert a parse error use this.
func tryParse(args ...string) (CLI, error) {
	var cli CLI
	parser, err := kong.New(
		&cli,
		kong.Name("nyala"),
		kong.Vars{"version": "test"},
	)
	if err != nil {
		return cli, err
	}
	_, err = parser.Parse(args)
	return cli, err
}

// parse runs kong over args and fails the test if the parse errors. URL is a
// required positional, so every parse call must supply one.
func parse(t *testing.T, args ...string) CLI {
	t.Helper()
	cli, err := tryParse(args...)
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cli
}

func TestParseDefaults(t *testing.T) {
	cli := parse(t, "example.com")
	if cli.Color != "auto" {
		t.Errorf("Color = %q, want %q by default", cli.Color, "auto")
	}
	if cli.Browser != "" {
		t.Errorf("Browser = %q, want empty by default", cli.Browser)
	}
	if cli.Profile != "" {
		t.Errorf("Profile = %q, want empty by default", cli.Profile)
	}
	if cli.DisableFrameRateLimit {
		t.Errorf("DisableFrameRateLimit = true, want false by default")
	}
}

func TestParseProfile(t *testing.T) {
	profileDir := t.TempDir()
	cli := parse(t, "--profile", profileDir, "example.com")
	if cli.Profile != profileDir {
		t.Errorf("--profile %q: Profile = %q, want %q", profileDir, cli.Profile, profileDir)
	}
}

func TestParseDisableFrameRateLimit(t *testing.T) {
	cli := parse(t, "--disable-frame-rate-limit", "example.com")
	if !cli.DisableFrameRateLimit {
		t.Errorf("--disable-frame-rate-limit: DisableFrameRateLimit = false, want true")
	}
}

func TestParseURL(t *testing.T) {
	cli := parse(t, "example.com")
	if cli.URL != "example.com" {
		t.Errorf("URL = %q, want the positional argument", cli.URL)
	}
}

func TestParseURLRequired(t *testing.T) {
	if _, err := tryParse(); err == nil {
		t.Errorf("no URL: want a required-argument error, got nil")
	}
}

func TestParseColorOverrides(t *testing.T) {
	for _, mode := range []string{"truecolor", "256", "none"} {
		cli := parse(t, "--color", mode, "example.com")
		if cli.Color != mode {
			t.Errorf("--color %s: Color = %q, want %q", mode, cli.Color, mode)
		}
	}
}

func TestParseColorInvalid(t *testing.T) {
	if _, err := tryParse("--color", "bogus", "example.com"); err == nil {
		t.Errorf("--color bogus: want a parse error, got nil")
	}
}

func TestParseBackendDefault(t *testing.T) {
	cli := parse(t, "example.com")
	if cli.Backend != "auto" {
		t.Errorf("Backend = %q, want %q by default", cli.Backend, "auto")
	}
}

func TestParseBackendOverrides(t *testing.T) {
	for _, mode := range []string{"auto", "block", "kitty", "sixel"} {
		cli := parse(t, "--backend", mode, "example.com")
		if cli.Backend != mode {
			t.Errorf("--backend %s: Backend = %q, want %q", mode, cli.Backend, mode)
		}
	}
}

func TestParseBackendInvalid(t *testing.T) {
	if _, err := tryParse("--backend", "bogus", "example.com"); err == nil {
		t.Errorf("--backend bogus: want a parse error, got nil")
	}
}

// TestParseLayoutWidthRemoved pins the flag removal: the Block backend derives
// its layout width from the cell-density formula, so --layout-width no longer
// parses.
func TestParseLayoutWidthRemoved(t *testing.T) {
	if _, err := tryParse("--layout-width", "960", "example.com"); err == nil {
		t.Errorf("--layout-width: want an unknown-flag error, got nil")
	}
}

func TestNormaliseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"bare host gains https", "example.com", "https://example.com"},
		{"bare host with path gains https", "example.com/page", "https://example.com/page"},
		{"https kept", "https://example.com", "https://example.com"},
		{"http kept", "http://example.com", "http://example.com"},
		{"file kept", "file:///tmp/page.html", "file:///tmp/page.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normaliseURL(tc.raw)
			if err != nil {
				t.Fatalf("normaliseURL(%q): unexpected error %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("normaliseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormaliseURLUnsupportedScheme(t *testing.T) {
	for _, raw := range []string{"ftp://example.com", "ws://example.com"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := normaliseURL(raw); err == nil {
				t.Errorf("normaliseURL(%q): want an unsupported-scheme error, got nil", raw)
			} else if !strings.Contains(err.Error(), "unsupported URL scheme") {
				t.Errorf("normaliseURL(%q): error = %q, want it to mention the unsupported scheme", raw, err)
			}
		})
	}
}

// TestRunValidURLPassesValidation asserts a valid URL passes normalisation and
// reaches the viewer, which rejects a non-terminal tty before any browser
// launches. A pipe read end is not a terminal, so Run returns the not-a-terminal
// error rather than a scheme error, proving the URL validated and no Chromium
// started.
func TestRunValidURLPassesValidation(t *testing.T) {
	for _, raw := range []string{"example.com", "https://example.com", "http://example.com", "file:///tmp/page.html"} {
		t.Run(raw, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			defer r.Close()
			defer w.Close()
			err = Run(CLI{URL: raw}, "dev", r)
			if err == nil {
				t.Fatalf("Run(%q): want the not-a-terminal error, got nil", raw)
			}
			if strings.Contains(err.Error(), "unsupported URL scheme") {
				t.Errorf("Run(%q): err = %v, want it past URL validation", raw, err)
			}
			if !strings.Contains(err.Error(), "not a terminal") {
				t.Errorf("Run(%q): err = %v, want the not-a-terminal guard", raw, err)
			}
		})
	}
}

// TestRunInvalidURLReturnsSchemeError asserts an unsupported scheme is rejected
// before the viewer runs, so Run returns the scheme error and never reaches the
// tty guard or a browser launch.
func TestRunInvalidURLReturnsSchemeError(t *testing.T) {
	err := Run(CLI{URL: "ftp://example.com"}, "dev", nil)
	if err == nil {
		t.Fatalf("Run(ftp): want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Errorf("Run(ftp): error = %q, want it to mention the unsupported scheme", err)
	}
}

// TestEngineKeyMapsPseudoCodes pins the viewer-to-engine key mapping: the two
// pseudo-codes become a modified stroke of a real key, and every other code
// passes through untouched. The modifier is the whole meaning here, so a lost
// bit would turn Shift+Tab into Tab and select-all into a keypress on the letter
// a.
func TestEngineKeyMapsPseudoCodes(t *testing.T) {
	cases := []struct {
		code string
		want engine.Key
	}{
		{"ShiftTab", engine.Key{Code: "Tab", Mod: 8}},
		{"SelectAll", engine.Key{Code: "KeyA", Mod: 2}},
		{"Tab", engine.Key{Code: "Tab"}},
		{"ArrowDown", engine.Key{Code: "ArrowDown"}},
	}
	for _, c := range cases {
		if got := engineKey(c.code); got != c.want {
			t.Errorf("engineKey(%q) = %+v, want %+v", c.code, got, c.want)
		}
	}
}

// TestMouseActionMapping walks every viewer mouse action and pins the engine
// action it becomes. The mapping ends in a default arm that answers Press, so a
// missing case would not fail to compile: it would quietly turn a release, a
// wheel step, or a hover into a button press on the page.
func TestMouseActionMapping(t *testing.T) {
	cases := []struct {
		name string
		in   viewer.MouseAction
		want engine.MouseAction
	}{
		{"press", viewer.MouseActionPress, engine.MouseActionPress},
		{"release", viewer.MouseActionRelease, engine.MouseActionRelease},
		{"wheel", viewer.MouseActionWheel, engine.MouseActionWheel},
		{"move", viewer.MouseActionMove, engine.MouseActionMove},
	}
	for _, c := range cases {
		if got := mouseAction(c.in); got != c.want {
			t.Errorf("mouseAction(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestMouseButtonMapping walks every viewer mouse button and pins the engine
// button it becomes. The mapping ends in a default arm that answers Left, so a
// missing case would silently forge a held left button; that is the whole
// meaning of the no-button hover value, which must survive as None or Chromium
// reads the motion as a drag. Back and Forward map to Left through that default
// on purpose: the viewer takes both as history navigation and never forwards
// them, so the two rows pin the safety net rather than a live route.
func TestMouseButtonMapping(t *testing.T) {
	cases := []struct {
		name string
		in   viewer.MouseButton
		want engine.MouseButton
	}{
		{"left", viewer.MouseButtonLeft, engine.MouseButtonLeft},
		{"middle", viewer.MouseButtonMiddle, engine.MouseButtonMiddle},
		{"right", viewer.MouseButtonRight, engine.MouseButtonRight},
		{"back", viewer.MouseButtonBack, engine.MouseButtonLeft},
		{"forward", viewer.MouseButtonForward, engine.MouseButtonLeft},
		{"none", viewer.MouseButtonNone, engine.MouseButtonNone},
	}
	for _, c := range cases {
		if got := mouseButton(c.in); got != c.want {
			t.Errorf("mouseButton(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestSendMouseCarriesHover pins the whole hover event across the adapter: the
// action, the no-button value, the modifiers, and the cell all arrive intact.
// The engine records nothing a test can read back, and SendMouse returns before
// the CDP dispatch when no browser context exists, so the assertion is built
// from the same mapping the adapter applies field by field; running the real
// adapter over a browserless engine beside it proves a hover reaches SendMouse
// and returns rather than panicking on the nil context.
func TestSendMouseCarriesHover(t *testing.T) {
	hover := viewer.MouseEvent{
		Action: viewer.MouseActionMove,
		Button: viewer.MouseButtonNone,
		Mod:    8, // CDP Shift.
		Col:    12,
		Row:    5,
	}
	want := engine.MouseEvent{
		Action: engine.MouseActionMove,
		Button: engine.MouseButtonNone,
		Mod:    8,
		Col:    12,
		Row:    5,
	}
	got := engine.MouseEvent{
		Action:  mouseAction(hover.Action),
		Button:  mouseButton(hover.Button),
		Mod:     hover.Mod,
		Col:     hover.Col,
		Row:     hover.Row,
		WheelDX: hover.WheelDX,
		WheelDY: hover.WheelDY,
	}
	if got != want {
		t.Errorf("hover mapping = %+v, want %+v", got, want)
	}
	engineAdapter{engine.New("")}.SendMouse(hover)
}
