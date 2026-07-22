package viewer

import (
	"strconv"
	"strings"
)

// terminalCompatibility holds terminal-specific rendering policy resolved at
// startup. XTVERSION is authoritative when present; environment identity is a
// conservative fallback for terminals that do not answer the query.
type terminalCompatibility struct {
	rio04                    bool
	sixelSyncUpdatesDisabled bool
}

// resolveTerminalCompatibility derives the compatibility policy for one
// terminal. Rio 0.4.x is matched only from an XTVERSION reply in the affected
// >=0.4.0,<0.5.0 range. When no reply is available, TERM_PROGRAM=rio or TERM=rio
// applies the policy conservatively, including behind a multiplexer.
func resolveTerminalCompatibility(termID string, environ []string) terminalCompatibility {
	compat := terminalCompatibility{
		sixelSyncUpdatesDisabled: disableSixelSyncUpdates(termID),
	}
	if strings.TrimSpace(termID) != "" {
		compat.rio04 = rio04XTVERSION(termID)
		return compat
	}
	compat.rio04 = rioEnvironmentFallback(environ)
	return compat
}

// rio04XTVERSION reports whether identity names an affected Rio 0.4.x release.
// Patch and pre-release suffixes do not alter the compatibility range.
func rio04XTVERSION(identity string) bool {
	fields := strings.Fields(identity)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "rio") {
		return false
	}
	version := strings.TrimPrefix(strings.ToLower(fields[1]), "v")
	version, _, _ = strings.Cut(version, "-")
	version, _, _ = strings.Cut(version, "+")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return majorErr == nil && minorErr == nil && major == 0 && minor == 4
}

// rioEnvironmentFallback reports whether the environment identifies Rio. It
// reads both documented identity variables directly so a multiplexer marker or
// another terminal-specific variable cannot mask the conservative fallback.
func rioEnvironmentFallback(environ []string) bool {
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok && (name == "TERM_PROGRAM" || name == "TERM") && strings.EqualFold(value, "rio") {
			return true
		}
	}
	return false
}

// sixelAllowed reports whether a detected Sixel capability is usable.
func (c terminalCompatibility) sixelAllowed() bool {
	return !c.rio04
}

// kittyFPSBadgeAllowed reports whether Kitty can present Nyala's live FPS badge.
func (c terminalCompatibility) kittyFPSBadgeAllowed() bool {
	return !c.rio04
}
