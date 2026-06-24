// Package app holds the Nyala command-line surface and its dispatch. It owns one
// CLI struct, parsed by kong in cmd/nyala, and routes a parsed run to the
// engine and viewer. The dispatch is thin: it validates the URL and will drive
// the live screencast loop. It holds no rendering logic of its own.
package app

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// CLI is the parsed Nyala command line. cmd/nyala binds it to kong; the fields
// carry the colour mode, the target URL, and an optional browser path.
type CLI struct {
	Version kong.VersionFlag `help:"Show version."`

	// Color is the viewer colour mode: auto, truecolor, 256, or none.
	Color string `help:"Viewer colour mode: auto, truecolor, 256, or none." default:"auto" enum:"auto,truecolor,256,none"`

	// Backend forces one renderer: auto, block, kitty, or sixel. auto detects the
	// terminal's best supported backend.
	Backend string `help:"Renderer backend: auto, block, kitty, or sixel." default:"auto" enum:"auto,block,kitty,sixel"`

	// Browser is the path to the Chromium executable to drive. Empty falls back
	// to NYALA_BROWSER, CHROME_PATH, then a PATH lookup at launch.
	Browser string `help:"Path to the Chromium executable to drive." name:"browser"`

	// URL is the page to open. A bare host gains an https:// scheme; http,
	// https, and file schemes are accepted, others are rejected.
	URL string `arg:"" help:"URL to open."`
}

// Run dispatches a parsed CLI on the given tty. It prepends https:// when the
// URL has no scheme, accepts http, https, and file, and rejects any other
// scheme with a clear error. It reads NYALA_CAPTURE: a parse error returns
// before any viewer call. With capture disabled it runs the interactive
// viewer.Stream; with capture enabled it runs the bounded viewer.Capture. Both
// pass the --backend mode through. It constructs the Chromium engine from the
// validated URL and the --browser flag.
func Run(cli CLI, tty *os.File) error {
	target, err := normaliseURL(cli.URL)
	if err != nil {
		return err
	}
	capture, err := ParseCaptureEnv(os.LookupEnv("NYALA_CAPTURE"))
	if err != nil {
		return err
	}
	eng := engineAdapter{engine.New(cli.Browser)}
	if capture.Enabled {
		return viewer.Capture(eng, target, tty, cli.Color, cli.Backend, capture.N)
	}
	return viewer.Stream(eng, target, tty, cli.Color, cli.Backend)
}

// engineAdapter wraps a ChromiumEngine to satisfy viewer.Engine. The viewer
// declares the methods it needs with terminal-neutral parameters, so it never
// imports the engine package; the adapter maps the viewer's string key code to
// the engine's Key and its capture recorder to the viewer's CaptureSession. The
// other methods pass straight through.
type engineAdapter struct{ *engine.ChromiumEngine }

// SendKey maps a viewer key code to an engine Key. The engine routes scroll-key
// codes (ArrowUp, PageDown, and so on) to its wheel transport.
func (a engineAdapter) SendKey(code string) {
	a.ChromiumEngine.SendKey(engine.Key{Code: code})
}

// EnableCapture constructs the engine capture recorder over the bundle's
// cdp-events.ndjson sink, enables engine capture before Navigate, and returns the
// recorder as a viewer.CaptureSession. The recorder is the single seam for the
// engine-to-capture coupling; constructing it here keeps the viewer free of the
// engine import.
func (a engineAdapter) EnableCapture(ndjson io.Writer) viewer.CaptureSession {
	rec := engine.NewCaptureRecorder(ndjson)
	a.ChromiumEngine.EnableCapture(rec)
	return rec
}

// normaliseURL prepends https:// when raw carries no scheme, then accepts only
// http, https, and file. It returns a clear error for any other scheme.
func normaliseURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}

	switch parsed.Scheme {
	case "http", "https", "file":
		return raw, nil
	default:
		return "", fmt.Errorf("unsupported URL scheme %q (want http, https, or file)", parsed.Scheme)
	}
}
