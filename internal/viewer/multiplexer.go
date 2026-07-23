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
// Sixel is offered under tmux and zellij only, and only when the DA1 reply
// advertised attribute 4, the resolved cell pixel geometry is real, and
// terminal compatibility allows Sixel. tmux additionally requires an
// XTVERSION identity of 3.6 or later (tmuxSixelSupported): older tmux caps
// DCS input at an unraisable 1 MiB and discards busy frames silently. tmux
// advertises DA1 Sixel only when both its build and its outer terminal
// support it, so its DA1 is honest; zellij answers DA1 with Sixel
// unconditionally, so the geometry gate carries the host-chain-live signal
// there (zellij synthesises pane pixel geometry only after its own startup
// query to the host succeeded). Without real geometry the Sixel surface would
// be sized from the 8x16 fallback and render at the wrong physical scale, so
// both multiplexers require it. screen and herdr stay block only.
func multiplexerBackends(mux multiplexerKind, termID string, sixelDA1, realGeometry bool, compat terminalCompatibility) []Backend {
	muxOK := (mux == muxTmux && tmuxSixelSupported(termID)) || mux == muxZellij
	if muxOK && sixelDA1 && realGeometry && compat.sixelAllowed() {
		return []Backend{BackendSixel, BackendBlock}
	}
	return []Backend{BackendBlock}
}

// multiplexerStartBackend nominates the backend auto-detection starts on under
// a focus multiplexer, from the enabled cycle multiplexerBackends resolved.
// tmux, screen, and herdr start on the head of the cycle, the highest-fidelity
// backend. zellij starts on Block even when Sixel is enabled: zellij allocates
// Sixel image ids as the current store size (sixel.rs next_image_id) instead
// of monotonically, so a full-frame-replacement stream collides ids every
// third frame and zellij's end-of-frame reap (grid.rs read_changes) deletes
// the just-inserted image before serialising it to the host, dropping the
// picture. Sixel stays in the cycle, so Tab and --backend=sixel still reach
// it. Lift this demotion in the compatibility policy, gated on the XTVERSION
// identity Zellij(<version>), once a zellij release ships monotonic image ids.
func multiplexerStartBackend(mux multiplexerKind, enabled []Backend) Backend {
	if mux == muxZellij || len(enabled) == 0 {
		return BackendBlock
	}
	return enabled[0]
}

// engineRows is the pure engine-grid row decision: the row count the engine
// lays out, renders, and captures at for one backend under one multiplexer.
// Sixel under tmux or zellij reserves the bottom terminal row, so the frame is
// (rows-1) cells of pixel height and the last row stays blank. tmux rescales
// any Sixel image taller than pane rows minus one, keeping the bottom rows and
// cropping the top, so a full-height frame always loses its top row; zellij
// re-serialises visible image chunks to the host terminal bare (no DECSDM), so
// a frame whose bottom pixel row reaches the host's bottom cell row scrolls
// the host one line per frame. Every other backend, and every backend on a
// native terminal, keeps the full grid. rows below two pass through unchanged,
// so a one-row terminal never shrinks to an empty grid.
func engineRows(backend Backend, mux multiplexerKind, rows int) int {
	if backend == BackendSixel && (mux == muxTmux || mux == muxZellij) && rows >= 2 {
		return rows - 1
	}
	return rows
}
