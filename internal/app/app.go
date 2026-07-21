// Package app holds the Nyala command-line surface and its dispatch. It owns one
// CLI struct, parsed by kong in cmd/nyala, and routes a parsed run to the
// engine and viewer. The dispatch is thin: it validates the URL and drives
// the live screencast loop. It holds no rendering logic of its own.
package app

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// defaultLayoutWidth is the target CSS-pixel layout width for the block backend.
// A terminal wider than this lays out at this width via the block device scale, so
// the default is recorded here in one place and referenced by the LayoutWidth flag.
const defaultLayoutWidth = 1280

// CLI is the parsed Nyala command line. cmd/nyala binds it to kong; the fields
// carry the colour mode, the target URL, and an optional browser path.
type CLI struct {
	Version kong.VersionFlag `help:"Show version."`

	// Color is the viewer colour mode: auto, truecolor, 256, or none.
	Color string `help:"Viewer colour mode: auto, truecolor, 256, or none." default:"auto" enum:"auto,truecolor,256,none"`

	// Backend forces one renderer: auto, block, kitty, or sixel. auto detects the
	// terminal's best supported backend.
	Backend string `help:"Renderer backend: auto, block, kitty, or sixel." default:"auto" enum:"auto,block,kitty,sixel"`

	// Blocks selects the block glyph mode: auto, sextant, quadrant, or half. On the
	// interactive path auto and sextant render sextant when the terminal supports it
	// and degrade to quadrant otherwise; quadrant forces quadrant; half renders
	// half-block.
	Blocks string `help:"Block glyph mode: auto, sextant, quadrant, or half." name:"blocks" default:"auto" enum:"auto,sextant,quadrant,half"`

	// Browser is the path to the Chromium executable to drive. Empty falls back
	// to NYALA_BROWSER, CHROME_PATH, then a PATH lookup at launch.
	Browser string `help:"Path to the Chromium executable to drive." name:"browser"`

	// DisableFrameRateLimit removes Chromium's browser frame-rate limit. It does
	// not control terminal or display synchronisation.
	DisableFrameRateLimit bool `help:"Disable Chromium's browser frame-rate limit." name:"disable-frame-rate-limit"`

	// KittyTransport selects the Kitty graphics transmission medium: auto
	// resolves it from the startup probe and the allow and deny lists, direct
	// forces the in-band PNG path, and shm forces the shared-memory fast path.
	KittyTransport string `help:"Kitty graphics transport: auto, direct, or shm." name:"kitty-transport" default:"auto" enum:"auto,direct,shm"`

	// LayoutWidth is the target CSS-pixel width the block backend lays out at. A
	// terminal wider than this shrinks its device scale so the page fits the target.
	// It must be positive; Validate rejects a non-positive value. It defaults to
	// defaultLayoutWidth.
	LayoutWidth int `help:"Block backend target layout width in CSS pixels." name:"layout-width" default:"1280"`

	// URL is the page to open. A bare host gains an https:// scheme; http,
	// https, and file schemes are accepted, others are rejected.
	URL string `arg:"" help:"URL to open."`
}

// Validate rejects a non-positive LayoutWidth. kong calls it after parsing; a
// clear error stops the run before engine construction.
func (c CLI) Validate() error {
	if c.LayoutWidth <= 0 {
		return fmt.Errorf("--layout-width must be positive, got %d", c.LayoutWidth)
	}
	return nil
}

// Run dispatches a parsed CLI on the given tty. It prepends https:// when the
// URL has no scheme, accepts http, https, and file, and rejects any other
// scheme with a clear error. It runs the interactive viewer and constructs the
// Chromium engine from the validated URL and the --browser flag.
func Run(cli CLI, tty *os.File) error {
	target, err := normaliseURL(cli.URL)
	if err != nil {
		return err
	}
	chromium := engine.New(cli.Browser)
	chromium.SetDisableFrameRateLimit(cli.DisableFrameRateLimit)
	eng := engineAdapter{chromium}
	return viewer.Stream(eng, target, tty, cli.Color, cli.Backend, cli.Blocks, cli.KittyTransport, cli.LayoutWidth)
}

// engineAdapter wraps a ChromiumEngine to satisfy viewer.Engine. The viewer
// declares the methods it needs with terminal-neutral parameters, so it never
// imports the engine package; the adapter maps the viewer's string key code to
// the engine's Key. The other methods pass straight through.
type engineAdapter struct{ *engine.ChromiumEngine }

// SendKey maps a viewer key code to an engine Key. The engine routes scroll-key
// codes (ArrowUp, PageDown, and so on) to its wheel transport.
func (a engineAdapter) SendKey(code string) {
	a.ChromiumEngine.SendKey(engine.Key{Code: code})
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
