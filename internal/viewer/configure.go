package viewer

import (
	"os"
	"runtime"

	cterm "github.com/charmbracelet/x/term"
)

// configureBackends decides which backends the cycle offers and resolves the
// real cell pixel size, then writes both into st. Under GNU screen or herdr it
// runs no handshake and offers block only: screen blocks graphics passthrough
// and herdr consumes graphics sequences. Under tmux or zellij it runs the
// reduced handshake (no Kitty APC probe, which both consume silently) and
// offers Sixel per multiplexerBackends, which admits tmux only and keeps
// zellij block only; Kitty is never offered under a multiplexer, and the auto
// start is the cycle head SetEnabled nominates. Otherwise
// it runs the full terminal handshake and offers a
// graphics backend only when the terminal confirms it. The colour mode resolves
// on every path after terminal compatibility, so no multiplexer early return
// can strand it at the NewState default. The bar's SGR opener resolves on
// every path beside it, from the OSC 11 reply the full handshake collects;
// every other path resolves to reverse video, because it sent no query. A
// NYALA_CELL_PX override (a diagnostic and harness knob) takes precedence over
// the ioctl and handshake cell; a malformed override returns an error so the
// run fails loudly. transportFlag is
// the --kitty-transport intent; the effective transport resolves here, once,
// from the flag and the probe result, and lands on st for the kitty draw path.
func configureBackends(st *State, tty *os.File, cols, rows int, colourFlag, transportFlag string) error {
	environ := os.Environ()
	detected := detectColour(environ, cterm.IsTerminal(tty.Fd()))
	// Record sextant support once, on every path, so the Alt+B scheme cycle
	// skips the sextant scheme on a terminal that cannot self-draw it,
	// matching the resolveScheme startup degrade.
	_, st.sextantOK = SextantSupported(environ)

	overrideValue, overrideSet := os.LookupEnv(cellPxEnv)
	overridePx, overrideOK, err := parseCellPxOverride(overrideValue, overrideSet)
	if err != nil {
		return err
	}

	winPx, winOK := winsizeCellPixels(tty.Fd(), cols, rows)
	switch {
	case overrideOK:
		st.cellPx = overridePx
		st.cellPxForced = true
	case winOK:
		st.cellPx = winPx
	}

	mux := detectMultiplexer()
	st.mux = mux
	switch mux {
	case muxScreen, muxHerdr:
		st.setTerminalCompatibility("", environ)
		st.Colour = resolveCompatibleColour(colourFlag, environ, detected, st.compat)
		st.barSGR = resolveBarStyle(false, false, st.Colour)
		// No handshake runs here, so the transport resolves with no identity
		// and no probe: an explicit override still wins and auto falls back
		// to direct.
		st.kittyTransport = resolveKittyTransport(transportFlag, "", false, runtime.GOOS)
		st.SetEnabled([]Backend{BackendBlock})
		return nil
	case muxTmux, muxZellij:
		res := probeTerminalReduced(tty, cols, rows)
		if !overrideOK && !winOK {
			st.cellPx = res.cellPx
		}
		st.setTerminalCompatibility(res.TermID, environ)
		if mux == muxTmux {
			// tmux Sixel drops DEC 2026 on every version. tmux does not defer
			// image operations under an application sync, so the bracket buys
			// nothing for Sixel frames (live-verified on 3.7b). The flag rides
			// the same compatibility seam as the Foot policy and scopes to
			// Sixel via syncUpdatesEnabled, so Block under tmux keeps its
			// fence.
			st.compat.sixelSyncUpdatesDisabled = true
			// Sixel is version-gated to tmux 3.6 and later
			// (tmuxSixelSupported). When the gate fails no Sixel flows, so
			// the tmux server mutation inside resolveTmuxSixelBudget is
			// skipped and the budget and floor stay unarmed.
			if tmuxSixelSupported(res.TermID) {
				// A successful input-buffer-size raise carries a
				// version-tuned budget: the raised full-quality budget on
				// 3.7+, where rate pacing alone suffices, and the 1.5 MB
				// output-stage rate cap on 3.6.x, whose re-encoded output
				// toward the host terminal saturates invisibly at higher
				// byte rates. A failed set keeps the 900 KB fallback under
				// the default 1 MiB input buffer.
				st.sixelBudget.limit = resolveTmuxSixelBudget(res.TermID, runTmux)
				// The pacing floor is version-tuned: tmux 3.7 and later
				// drain their output scheduler at 250 ms, while 3.6.x needs
				// 300 ms to avoid stall-and-burst release toward the host
				// terminal.
				st.sixelBudget.minInterval = resolveTmuxSixelMinInterval(res.TermID)
				st.sixelBudget.debug = os.Getenv(debugTimingEnv) != ""
			}
		}
		st.Colour = resolveCompatibleColour(colourFlag, environ, detected, st.compat)
		// The reduced handshake sent no OSC 11 query, so the bar keeps reverse
		// video: tmux would answer from a cache read once at attach, which
		// describes the terminal as it was rather than as it is.
		st.barSGR = resolveBarStyle(false, false, st.Colour)
		// The reduced handshake sent no Kitty probe, so the transport resolves
		// with no identity and no probe result: an explicit override still
		// wins and auto falls back to direct.
		st.kittyTransport = resolveKittyTransport(transportFlag, "", false, runtime.GOOS)
		realGeometry := overrideOK || winOK || res.cellPxOK
		st.SetEnabled(multiplexerBackends(mux, res.TermID, res.Sixel, realGeometry, st.compat))
		return nil
	}

	res := probeTerminal(tty, cols, rows)
	if !overrideOK && !winOK {
		st.cellPx = res.cellPx
	}
	// The keyboard protocol rides the full handshake only. A multiplexer would
	// have to translate the reports back for its own key handling, and the
	// reduced handshake deliberately asks nothing it cannot trust, so tmux,
	// zellij, screen, and herdr sessions keep the legacy parser.
	st.kittyKeyboard = res.KittyKbd
	st.kittyTransport = resolveKittyTransport(transportFlag, res.TermID, res.KittySHM, runtime.GOOS)
	st.setTerminalCompatibility(res.TermID, environ)
	st.Colour = resolveCompatibleColour(colourFlag, environ, detected, st.compat)
	st.barSGR = resolveBarStyle(res.BackgroundDark, res.BackgroundOK, st.Colour)

	st.SetEnabled(enabledBackends(res, st.compat))
	return nil
}

// enabledBackends returns the detected backend cycle after terminal
// compatibility gates. Block remains the floor and Rio 0.4.x silently omits
// its advertised but non-rendering Sixel capability.
func enabledBackends(res probeResult, compat terminalCompatibility) []Backend {
	enabled := []Backend{}
	if res.Kitty {
		enabled = append(enabled, BackendKitty)
	}
	if res.Sixel && compat.sixelAllowed() {
		enabled = append(enabled, BackendSixel)
	}
	return append(enabled, BackendBlock)
}
