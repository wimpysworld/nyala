package app

import (
	"os"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
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
	if cli.DisableFrameRateLimit {
		t.Errorf("DisableFrameRateLimit = true, want false by default")
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

func TestParseLayoutWidthDefault(t *testing.T) {
	cli := parse(t, "example.com")
	if cli.LayoutWidth != defaultLayoutWidth {
		t.Errorf("LayoutWidth = %d, want %d by default", cli.LayoutWidth, defaultLayoutWidth)
	}
}

func TestParseLayoutWidthOverride(t *testing.T) {
	cli := parse(t, "--layout-width", "960", "example.com")
	if cli.LayoutWidth != 960 {
		t.Errorf("--layout-width 960: LayoutWidth = %d, want 960", cli.LayoutWidth)
	}
}

func TestParseLayoutWidthRejectsNonPositive(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		t.Run(v, func(t *testing.T) {
			if _, err := tryParse("--layout-width", v, "example.com"); err == nil {
				t.Errorf("--layout-width %s: want a validation error, got nil", v)
			} else if !strings.Contains(err.Error(), "layout-width") {
				t.Errorf("--layout-width %s: err = %v, want it to name layout-width", v, err)
			}
		})
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
			err = Run(CLI{URL: raw}, r)
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
	err := Run(CLI{URL: "ftp://example.com"}, nil)
	if err == nil {
		t.Fatalf("Run(ftp): want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Errorf("Run(ftp): error = %q, want it to mention the unsupported scheme", err)
	}
}
