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

// CLI is the parsed Nyala command line. cmd/nyala binds it to kong; the fields
// carry the colour mode, the target URL, optional browser and profile paths,
// and the private-profile policy.
type CLI struct {
	Version kong.VersionFlag `help:"Show version."`

	// Color is the viewer colour mode.
	Color string `help:"Viewer colour mode: auto, truecolor, 256, or ansi-grey." default:"auto" enum:"auto,truecolor,256,ansi-grey"`

	// Backend selects the starting renderer: auto, block, kitty, or sixel. auto
	// detects the terminal's best supported backend; a named backend fails fast
	// when the terminal cannot run it. Alt+R cycles all supported backends.
	Backend string `help:"Renderer backend: auto, block, kitty, or sixel." default:"auto" enum:"auto,block,kitty,sixel"`

	// Blocks selects the block glyph mode: auto, sextant, or quadrant. On the
	// interactive path auto and sextant render sextant when the terminal
	// supports it and degrade to quadrant otherwise; quadrant forces quadrant.
	Blocks string `help:"Block glyph mode: auto, sextant, or quadrant." name:"blocks" default:"auto" enum:"auto,sextant,quadrant"`

	// Browser is the path to the Chromium executable to drive. Empty falls back
	// to NYALA_BROWSER, CHROME_PATH, then a PATH lookup at launch.
	Browser string `help:"Path to the Chromium executable to drive." name:"browser"`

	// Profile is the directory used for persistent browser profile data. Empty
	// resolves from the environment and the user's data directory.
	Profile string `help:"Browser profile directory. Defaults to $NYALA_PROFILE, $XDG_DATA_HOME/nyala/profile, or ~/.local/share/nyala/profile."`

	// Private uses a disposable browser profile and ignores persistent profile settings.
	Private bool `help:"Use a disposable browser profile and ignore --profile." name:"private"`

	// DisableFrameRateLimit removes Chromium's browser frame-rate limit. It does
	// not control terminal or display synchronisation.
	DisableFrameRateLimit bool `help:"Disable Chromium's browser frame-rate limit." name:"disable-frame-rate-limit"`

	// KittyTransport selects the Kitty graphics transmission medium: auto
	// resolves it from the startup probe and the allow and deny lists, direct
	// forces the in-band PNG path, and shm forces the shared-memory fast path.
	KittyTransport string `help:"Kitty graphics transport: auto, direct, or shm." name:"kitty-transport" default:"auto" enum:"auto,direct,shm"`

	// URL is the page to open. A bare host gains an https:// scheme; http,
	// https, and file schemes are accepted, others are rejected.
	URL string `arg:"" help:"URL to open."`
}

// Run dispatches a parsed CLI on the given tty. It prepends https:// when the
// URL has no scheme, accepts http, https, and file, and rejects any other
// scheme with a clear error. It configures the Chromium engine from the
// --browser and --profile flags, unless --private bypasses persistent profile
// setup, then runs the interactive viewer. version is the build version main
// plumbs through for the viewer's About line.
func Run(cli CLI, version string, tty *os.File) error {
	target, err := normaliseURL(cli.URL)
	if err != nil {
		return err
	}
	chromium := engine.New(cli.Browser)
	if !cli.Private {
		if err := chromium.SetProfileDir(cli.Profile); err != nil {
			return err
		}
	}
	chromium.SetDisableFrameRateLimit(cli.DisableFrameRateLimit)
	eng := engineAdapter{chromium}
	return viewer.Stream(eng, target, tty, cli.Color, cli.Backend, cli.Blocks, cli.KittyTransport, version)
}

// engineAdapter wraps a ChromiumEngine to satisfy viewer.Engine. The viewer
// declares the methods it needs with terminal-neutral parameters, so it never
// imports the engine package; the adapter maps the viewer's string key code to
// the engine's Key. The other methods pass straight through.
type engineAdapter struct{ *engine.ChromiumEngine }

// SendKey maps a viewer key code to an engine Key. The engine dispatches every
// stroke code (Tab, Enter, and the caret keys such as ArrowUp and PageDown) as
// a full key stroke, so Chromium runs the key's own default action. The viewer's
// two pseudo-codes map to a modified stroke here, because the viewer seam
// carries only a code string: ShiftTab (the CSI Z backtab) is a Tab stroke
// carrying the CDP Shift bit, and SelectAll (Ctrl+A) is an A stroke carrying the
// Ctrl bit, which Chromium reads as its own select-all.
func (a engineAdapter) SendKey(code string) {
	a.ChromiumEngine.SendKey(engineKey(code))
}

// engineKey maps one viewer key code to the engine Key it dispatches. Every
// code but the two pseudo-codes passes through unmodified.
func engineKey(code string) engine.Key {
	// CDP modifier bits: Ctrl=2, Shift=8.
	switch code {
	case "ShiftTab":
		return engine.Key{Code: "Tab", Mod: 8}
	case "SelectAll":
		return engine.Key{Code: "KeyA", Mod: 2}
	default:
		return engine.Key{Code: code}
	}
}

// FindState maps the engine's page-search snapshot to the viewer's. The counts
// come from the engine's own state, so the read never waits on Chromium.
func (a engineAdapter) FindState() viewer.FindState {
	s := a.ChromiumEngine.FindState()
	return viewer.FindState{
		Query:   s.Query,
		Total:   s.Total,
		Current: s.Current,
		Pending: s.Pending,
		Failed:  s.Failed,
	}
}

// SendNav maps a viewer navigation request to the engine's NavAction. The
// engine owns the launch URL and the navigation path.
func (a engineAdapter) SendNav(action viewer.NavAction) {
	a.ChromiumEngine.SendNav(navAction(action))
}

// navAction maps the viewer navigation enum to the engine's.
func navAction(a viewer.NavAction) engine.NavAction {
	switch a {
	case viewer.NavBack:
		return engine.NavBack
	case viewer.NavForward:
		return engine.NavForward
	case viewer.NavHome:
		return engine.NavHome
	default:
		return engine.NavReload
	}
}

// SendMouse maps a viewer mouse event to the engine's terminal-neutral
// MouseEvent. The fields mirror each other; the engine converts the cell to
// CSS pixels and dispatches over CDP.
func (a engineAdapter) SendMouse(ev viewer.MouseEvent) {
	a.ChromiumEngine.SendMouse(engine.MouseEvent{
		Action:  mouseAction(ev.Action),
		Button:  mouseButton(ev.Button),
		Mod:     ev.Mod,
		Col:     ev.Col,
		Row:     ev.Row,
		WheelDX: ev.WheelDX,
		WheelDY: ev.WheelDY,
	})
}

// mouseAction maps the viewer mouse action enum to the engine's.
func mouseAction(a viewer.MouseAction) engine.MouseAction {
	switch a {
	case viewer.MouseActionRelease:
		return engine.MouseActionRelease
	case viewer.MouseActionWheel:
		return engine.MouseActionWheel
	case viewer.MouseActionMove:
		return engine.MouseActionMove
	default:
		return engine.MouseActionPress
	}
}

// mouseButton maps the viewer mouse button enum to the engine's. The no-button
// value needs its own arm rather than the default one: the default answers Left,
// so every hover would forge a held left button and Chromium would read the
// motion as a drag. The default stays for the viewer's Back and Forward buttons,
// which the viewer takes as history navigation and never forwards here, so it is
// only ever a safety net.
func mouseButton(b viewer.MouseButton) engine.MouseButton {
	switch b {
	case viewer.MouseButtonMiddle:
		return engine.MouseButtonMiddle
	case viewer.MouseButtonRight:
		return engine.MouseButtonRight
	case viewer.MouseButtonNone:
		return engine.MouseButtonNone
	default:
		return engine.MouseButtonLeft
	}
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
