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

func TestParseColorDefault(t *testing.T) {
	cli := parse(t, "example.com")
	if cli.Color != "auto" {
		t.Errorf("Color = %q, want %q by default", cli.Color, "auto")
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

// TestRunCaptureUnsetReachesStream asserts that with NYALA_CAPTURE unset, Run
// reaches viewer.Stream. A pipe read end is not a terminal, so the
// not-a-terminal guard fires inside the viewer, proving selection reached it
// without launching a browser.
func TestRunCaptureUnsetReachesStream(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if _, set := os.LookupEnv("NYALA_CAPTURE"); set {
		t.Setenv("NYALA_CAPTURE", "")
		os.Unsetenv("NYALA_CAPTURE")
	}
	err = Run(CLI{URL: "example.com", Backend: "auto"}, r)
	if err == nil {
		t.Fatalf("Run capture unset: want the not-a-terminal error, got nil")
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("Run capture unset: err = %v, want the not-a-terminal guard", err)
	}
}

// TestRunCaptureEnabledReachesCapture asserts that with NYALA_CAPTURE=1, Run
// reaches viewer.Capture. The pipe read end is not a terminal, so the guard
// inside Capture fires, proving selection reached it without launching a
// browser. t.Setenv isolates NYALA_CAPTURE from other tests.
func TestRunCaptureEnabledReachesCapture(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	t.Setenv("NYALA_CAPTURE", "1")
	err = Run(CLI{URL: "example.com", Backend: "block"}, r)
	if err == nil {
		t.Fatalf("Run capture=1: want the not-a-terminal error, got nil")
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("Run capture=1: err = %v, want the not-a-terminal guard", err)
	}
}

// TestRunCaptureParseErrorReturnsBeforeViewer asserts a NYALA_CAPTURE parse
// error returns before any viewer call. The error names NYALA_CAPTURE, not the
// not-a-terminal guard, so Run failed at the parse, not in the viewer.
func TestRunCaptureParseErrorReturnsBeforeViewer(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	t.Setenv("NYALA_CAPTURE", "abc")
	err = Run(CLI{URL: "example.com", Backend: "block"}, r)
	if err == nil {
		t.Fatalf("Run capture=abc: want a parse error, got nil")
	}
	if !strings.Contains(err.Error(), "NYALA_CAPTURE") {
		t.Errorf("Run capture=abc: err = %v, want it to name NYALA_CAPTURE", err)
	}
	if strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("Run capture=abc: err = %v, want the parse error before any viewer call", err)
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
