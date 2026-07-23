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

// tmuxSixelSupported reports whether the tmux version named by the XTVERSION
// identity may carry Sixel at all: 3.6 and later. tmux 3.4 through 3.5a
// compile in a 1 MiB DCS input cap with no input-buffer-size option to raise
// it, so any busy frame over 1 MiB is silently discarded (the original
// invisible black-screen failure). The 900 KB fallback budget cannot
// guarantee that cap on large grids: the ladder floor at factor 4 encodes
// ~1.45 MB and the smallest-anyway rule would feed tmux frames it throws
// away with no signal. Those versions are also untested, and the ecosystem
// answer for them is shrink or refuse, so Nyala refuses. An unknown or
// unparsed identity fails the gate, matching the conservative treatment of
// unknown versions elsewhere.
func tmuxSixelSupported(termID string) bool {
	major, minor, ok := parseTmuxVersion(termID)
	return ok && (major > 3 || (major == 3 && minor >= 6))
}

// multiplexerBackends returns the backend cycle offered under a focus
// multiplexer. Kitty is never offered: tmux and zellij consume the Kitty APC
// protocol without passthrough, and screen and herdr pass no graphics at all.
// Sixel is offered under tmux only, and only when the DA1 reply advertised
// attribute 4, the resolved cell pixel geometry is real, terminal
// compatibility allows Sixel, and the XTVERSION identity is 3.6 or later
// (tmuxSixelSupported): older tmux caps DCS input at an unraisable 1 MiB and
// discards busy frames silently. tmux advertises DA1 Sixel only when both its
// build and its outer terminal support it, so its DA1 is honest, and without
// real geometry the Sixel surface would be sized from the 8x16 fallback and
// render at the wrong physical scale. zellij never enters the Sixel branch,
// regardless of DA1 and geometry: zellij 0.31+ re-encodes Sixel but allocates
// image ids as the current store size (sixel.rs next_image_id) instead of
// monotonically, so a full-frame-replacement stream collides ids and drops
// every third frame, and its bare re-emission of visible image chunks to the
// host breaks scrolling (the page appears to scroll, then snaps back). Both
// are zellij-side defects with no Nyala-side workaround; blocking Sixel keeps
// them from presenting as Nyala bugs, so zellij stays block only. Lift the
// block, gated on the XTVERSION identity Zellij(<version>), once a zellij
// release fixes both. screen and herdr stay block only.
func multiplexerBackends(mux multiplexerKind, termID string, sixelDA1, realGeometry bool, compat terminalCompatibility) []Backend {
	if mux == muxTmux && tmuxSixelSupported(termID) && sixelDA1 && realGeometry && compat.sixelAllowed() {
		return []Backend{BackendSixel, BackendBlock}
	}
	return []Backend{BackendBlock}
}

// engineRows is the pure engine-grid row decision: the row count the engine
// lays out, renders, and captures at for one backend under one multiplexer.
// Sixel under tmux reserves the bottom terminal row, so the frame is (rows-1)
// cells of pixel height and the last row stays blank: tmux rescales any Sixel
// image taller than pane rows minus one, keeping the bottom rows and cropping
// the top, so a full-height frame always loses its top row. Every other
// backend, and every backend on a native terminal, keeps the full grid (Sixel
// never runs under zellij, screen, or herdr, so no other multiplexer needs
// the reservation). rows below two pass through unchanged, so a one-row
// terminal never shrinks to an empty grid.
func engineRows(backend Backend, mux multiplexerKind, rows int) int {
	if backend == BackendSixel && mux == muxTmux && rows >= 2 {
		return rows - 1
	}
	return rows
}
