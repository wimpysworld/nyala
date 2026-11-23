package viewer

import (
	"os"
	"strconv"
	"strings"
)

// configureTmuxKitty accepts a user's assertion that the outer terminal can
// display Kitty placeholders. tmux does not relay replies to wrapped Kitty
// probes, so a normal terminal handshake cannot establish this capability.
func configureTmuxKitty(st *State, session tmuxSession, verified bool, termID string, realGeometry bool, transportFlag string) bool {
	if os.Getenv("NYALA_TMUX_KITTY") != "1" || !verified || !realGeometry ||
		transportFlag == string(KittyTransportSHM) || !tmuxSixelSupported(termID) ||
		!tmuxKittyGridFits(st.Cols, engineRows(st.Rows)) {
		return false
	}
	if !tmuxKittyClientOK(session, os.Environ()) {
		return false
	}
	passthrough, err := session.query("show-options", "-Apqv", "-t", session.paneID, "allow-passthrough")
	if err != nil || strings.TrimSpace(passthrough) != "on" {
		return false
	}
	size, err := session.query("show-options", "-sv", "input-buffer-size")
	bytes, parseErr := strconv.ParseUint(strings.TrimSpace(size), 10, 64)
	// One wrapped 4096-byte Kitty continuation must fit, including framing
	// and tmux's power-of-two allocation.
	if err != nil || parseErr != nil || bytes < 8192 {
		return false
	}
	st.kittyTmux = true
	st.kittyTransport = KittyTransportDirect
	return true
}

func tmuxKittyClientOK(session tmuxSession, environ []string) bool {
	if len(session.clients) != 1 {
		return false
	}
	client := session.clients[0]
	return client.utf8 && client.hasFeature("RGB") && client.termName != "" &&
		!tmuxClientIsNested(client.termName) && !hasOuterMultiplexer(environ)
}
