package viewer

import "os"

// multiplexerKind identifies the focus multiplexer hosting the session, or
// muxNone when the viewer talks to the terminal directly.
type multiplexerKind int

const (
	// muxNone means no focus multiplexer is detected.
	muxNone multiplexerKind = iota
	// muxTmux is tmux, detected through TMUX.
	muxTmux
	// muxZellij is zellij, detected through ZELLIJ presence, any value.
	muxZellij
	// muxScreen is GNU screen, detected through STY.
	muxScreen
	// muxHerdr is herdr, detected through HERDR_ENV=1 with HERDR_PANE_ID present.
	muxHerdr
)

// detectMultiplexer reports which focus multiplexer hosts the viewer: tmux
// (TMUX), GNU screen (STY), zellij (ZELLIJ present, any value), or herdr
// (HERDR_ENV=1 with HERDR_PANE_ID present). A bare ZELLIJ=0 is a presence
// signal, not a truth value; herdr needs both keys so a stray var does not
// gate graphics off.
func detectMultiplexer() multiplexerKind {
	if os.Getenv("TMUX") != "" {
		return muxTmux
	}
	if os.Getenv("STY") != "" {
		return muxScreen
	}
	if _, ok := os.LookupEnv("ZELLIJ"); ok {
		return muxZellij
	}
	if os.Getenv("HERDR_ENV") == "1" {
		if _, ok := os.LookupEnv("HERDR_PANE_ID"); ok {
			return muxHerdr
		}
	}
	return muxNone
}

// tmuxSixelSupported reports whether the XTVERSION identity names a
// supported tmux release (3.6 or later). Earlier releases have a fixed DCS
// input cap and do not expose the server option that Nyala checks.
func tmuxSixelSupported(termID string) bool {
	major, minor, ok := parseTmuxVersion(termID)
	return ok && (major > 3 || (major == 3 && minor >= 6))
}

// multiplexerBackends is the native graphics policy under a multiplexer.
// Native Sixel requires tmux 3.6+, DA1 attribute 4, real cell geometry,
// terminal compatibility and a verified Sixel-capable host. tmux's DA1
// describes its own decoder, not the capability of its attached clients.
// Zellij stays on Block because of image id reuse and scroll redraw defects.
func multiplexerBackends(mux multiplexerKind, termID string, sixelDA1, realGeometry, hostSixel bool, compat terminalCompatibility) []Backend {
	if mux == muxTmux && tmuxSixelSupported(termID) && sixelDA1 && realGeometry && hostSixel && compat.sixelAllowed() {
		return []Backend{BackendSixel, BackendBlock}
	}
	return []Backend{BackendBlock}
}
