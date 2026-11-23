package viewer

import (
	"os"
	"runtime"

	cterm "github.com/charmbracelet/x/term"
)

// configureBackends resolves cell geometry, the backend cycle and its starting
// backend. Screen uses Block without a handshake. Herdr uses a reduced
// handshake with a native Kitty query. Tmux and zellij use a reduced
// handshake without Kitty. A bare terminal gets the full handshake.
// The colour mode resolves
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
	isTTY := cterm.IsTerminal(tty.Fd())
	detected := detectColour(environ, isTTY)
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
	case muxScreen:
		st.setTerminalCompatibility("", environ)
		st.configureColour(colourFlag, environ, detected, isTTY)
		st.barSGR = resolveBarStyle(false, false)
		// No handshake runs here, so the transport resolves with no identity
		// and no probe: an explicit override still wins and auto falls back
		// to direct.
		st.kittyTransport = resolveKittyTransport(transportFlag, "", false, runtime.GOOS)
		st.SetEnabled([]Backend{BackendBlock})
		return nil
	case muxHerdr:
		res := probeTerminalReduced(tty, cols, rows, true)
		st.probe = res
		if !overrideOK && !winOK {
			st.cellPx = res.cellPx
		}
		st.setTerminalCompatibility(res.TermID, environ)
		st.configureColour(colourFlag, environ, detected, isTTY)
		st.barSGR = resolveBarStyle(false, false)
		// Herdr owns image placement and may not share Nyala's memory namespace.
		st.kittyTransport = KittyTransportDirect
		realGeometry := overrideOK || winOK || res.cellPxOK
		if res.Kitty && realGeometry && KittyTransport(transportFlag) != KittyTransportSHM {
			st.SetEnabled([]Backend{BackendKitty, BackendBlock})
		} else {
			st.SetEnabled([]Backend{BackendBlock})
		}
		return nil
	case muxTmux, muxZellij:
		res := probeTerminalReduced(tty, cols, rows, false)
		st.probe = res
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
		}
		st.configureColour(colourFlag, environ, detected, isTTY)
		// The reduced handshake sent no OSC 11 query, so the bar keeps reverse
		// video: tmux would answer from a cache read once at attach, which
		// describes the terminal as it was rather than as it is.
		st.barSGR = resolveBarStyle(false, false)
		// The reduced handshake sent no Kitty probe, so the transport resolves
		// with no identity and no probe result: an explicit override still
		// wins and auto falls back to direct.
		st.kittyTransport = resolveKittyTransport(transportFlag, "", false, runtime.GOOS)
		realGeometry := overrideOK || winOK || res.cellPxOK
		var session tmuxSession
		var verified bool
		if mux == muxTmux && realGeometry {
			session, verified = inspectTmux(tty)
		}
		hostSixel := verified && !hasOuterMultiplexer(environ) && session.supportsSixel()
		enabled := multiplexerBackends(mux, res.TermID, res.Sixel, realGeometry, hostSixel, st.compat)
		if enabled[0] == BackendSixel {
			st.sixelBudget.limit, st.sixelBudget.hardLimit = resolveTmuxSixelBudget(res.TermID, session.query)
			if st.sixelBudget.limit == 0 {
				enabled = []Backend{BackendBlock}
			} else {
				st.sixelBudget.minInterval = resolveTmuxSixelMinInterval(res.TermID)
				st.sixelBudget.debug = os.Getenv(debugTimingEnv) != ""
			}
		}
		if configureTmuxKitty(st, session, verified, res.TermID, realGeometry, transportFlag) {
			enabled = append([]Backend{BackendKitty}, enabled...)
		}
		st.SetEnabled(enabled)
		return nil
	}

	res := probeTerminal(tty, cols, rows)
	st.probe = res
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
	st.configureColour(colourFlag, environ, detected, isTTY)
	st.barSGR = resolveBarStyle(res.BackgroundDark, res.BackgroundOK)
	// The pointer-shape capability rides the full handshake only, like the
	// keyboard protocol above: the reduced handshake sends no OSC 22 query,
	// so multiplexer sessions keep the terminal's own pointer.
	st.pointerShapeOK = res.PointerShapeOK
	st.pointerShapes = res.PointerShapes
	// The palette-query capability rides the full handshake only, for the same
	// reason as the pointer shapes above: the reduced handshake sends no OSC 4
	// query, so multiplexer sessions record no palette support.
	st.paletteQueryOK = res.PaletteQueryOK

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
