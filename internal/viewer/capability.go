package viewer

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wimpysworld/nyala/internal/render"
)

// capabilityState is the effective state of one terminal capability after
// every policy gate ran: the probe answer, the multiplexer rule, the
// compatibility deny list, and the user override. A blocked capability is a
// decision, so it reports capOff rather than capUnknown.
type capabilityState int

const (
	// capUnknown reports that no decision was reached. The terminal
	// capabilities never carry it, because configureBackends decides each one.
	capUnknown capabilityState = iota
	// capOff reports that the capability is unsupported, blocked, or failed.
	capOff
	// capOn reports that the capability is active for this run.
	capOn
)

// capabilitySnapshot is the one-source view of the runtime the About bar
// formats. Every string is display text with terminal controls already
// stripped, so the bar writer never re-checks it.
type capabilitySnapshot struct {
	Version  string // Nyala version text
	Browser  string // browser name and major version, or "" when unknown
	Terminal string // terminal identity from XTVERSION, or "" when the terminal sent none

	Kitty   capabilityState // BackendKitty is in the cycle
	Sixel   capabilityState // BackendSixel is in the cycle after multiplexer and compatibility policy
	Sextant capabilityState // the terminal self-draws the sextant Block scheme

	KittyTransport KittyTransport    // effective Kitty transmission medium
	Colour         capabilityColour  // effective Block colour depth and palette capability
	WebGL          render.WebGLState // engine WebGL probe result
}

// capabilityColour is the selected Block colour mode and whether its palette
// can adapt to each frame.
type capabilityColour struct {
	Mode     ColourMode
	Adaptive bool
}

// capabilitySnapshot builds the About bar's view of this run from the state
// configureBackends resolved. version is the Nyala version text, browser is
// the engine's product text such as "Chrome 138.0.7204.49", and webgl is the
// engine's WebGL probe result. Kitty and Sixel report the backends in Enabled,
// not the raw probe answers, so a policy block shows as off.
func (s *State) capabilitySnapshot(version, browser string, webgl render.WebGLState) capabilitySnapshot {
	webgl.Label = stripControls(webgl.Label)
	return capabilitySnapshot{
		Version:        stripControls(version),
		Browser:        browserField(browser),
		Terminal:       terminalField(s.probe.TermID),
		Kitty:          backendState(s.Enabled, BackendKitty),
		Sixel:          backendState(s.Enabled, BackendSixel),
		Sextant:        boolState(s.sextantOK),
		KittyTransport: s.kittyTransport,
		Colour:         capabilityColour{Mode: s.Colour, Adaptive: s.paletteQueryOK},
		WebGL:          webgl,
	}
}

// backendState reports capOn when b is in enabled, else capOff.
func backendState(enabled []Backend, b Backend) capabilityState {
	for _, e := range enabled {
		if e == b {
			return capOn
		}
	}
	return capOff
}

// boolState maps a decided gate onto capOn or capOff.
func boolState(on bool) capabilityState {
	if on {
		return capOn
	}
	return capOff
}

// colourLabel returns the compact name of a Block colour mode. An out-of-range
// mode reports the NewState default.
func colourLabel(colour capabilityColour) string {
	switch colour.Mode {
	case ColourXTerm256:
		if colour.Adaptive {
			return "240 Adaptive"
		}
		return "240 Static"
	case ColourXTermGrey:
		if colour.Adaptive {
			return "240 Shades"
		}
		return "24 Shades"
	default:
		return "True Colour"
	}
}

// browserField shortens the engine's product text to the browser name and
// major version: "Chrome 138.0.7204.49" becomes "Chrome 138" and
// "Chromium 150.0.1" becomes "Chromium 150". Text without a numeric version
// field passes through unchanged. The result is control-stripped.
func browserField(version string) string {
	version = strings.TrimSpace(stripControls(version))
	name, rest, ok := strings.Cut(version, " ")
	if !ok || rest == "" || !unicode.IsDigit(rune(rest[0])) {
		return version
	}
	major, _, _ := strings.Cut(rest, ".")
	return name + " " + major
}

// terminalField turns an XTVERSION identity into display text: "kitty 0.40.1"
// becomes "Kitty 0.40.1", and an empty identity stays empty so the bar omits
// the field. Only the first letter changes case, so "WezTerm" keeps its own
// capitalisation. The result is control-stripped.
func terminalField(termID string) string {
	termID = strings.TrimSpace(stripControls(termID))
	if termID == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(termID)
	return string(unicode.ToUpper(first)) + termID[size:]
}
