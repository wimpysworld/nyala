package viewer

import (
	"fmt"
	"strconv"
	"strings"
)

// terminalCompatibility holds terminal-specific rendering policy resolved at
// startup. XTVERSION is authoritative when present; environment identity is a
// conservative fallback for terminals that do not answer the query.
type terminalCompatibility struct {
	rio04                    bool
	sixelSyncUpdatesDisabled bool
	titlePopIgnored          bool
}

// resolveTerminalCompatibility derives the compatibility policy for one
// terminal. Rio 0.4.x is matched only from an XTVERSION reply in the affected
// >=0.4.0,<0.5.0 range. When no reply is available, TERM_PROGRAM=rio or TERM=rio
// applies the policy conservatively, including behind a multiplexer.
func resolveTerminalCompatibility(termID string, environ []string) terminalCompatibility {
	compat := terminalCompatibility{
		sixelSyncUpdatesDisabled: disableSixelSyncUpdates(termID),
		titlePopIgnored:          titlePopIgnoredTerminal(termID),
	}
	if strings.TrimSpace(termID) != "" {
		compat.rio04 = rio04XTVERSION(termID)
		return compat
	}
	compat.rio04 = rioEnvironmentFallback(environ)
	return compat
}

// disableSixelSyncUpdates reports whether the terminal needs live Sixel frames
// without DEC 2026 fencing. Foot identifies itself as "foot(<version>)" through
// XTVERSION. Its synchronised-update enable path can clear the grid refresh left
// by the preceding completed Sixel when frames arrive back to back. Other
// terminals, and an absent identity, retain the normal synchronised fence.
func disableSixelSyncUpdates(termID string) bool {
	name := terminalNameToken(termID)
	return name == "foot" || strings.HasPrefix(name, "foot(")
}

// titlePopIgnoredTerminal reports whether the terminal parses the XTWINOPS
// title push and pop but does not restore the saved title on pop. WezTerm and
// Konsole both accept the two sequences without restoring, so teardown must
// follow the pop with an explicit neutral title. The match reads only the
// XTVERSION identity name token; TERM is never consulted.
func titlePopIgnoredTerminal(termID string) bool {
	name := terminalNameToken(termID)
	return name == "wezterm" || name == "konsole"
}

// setTerminalCompatibility applies terminal-specific compatibility decisions
// from XTVERSION and the environment fallback. Keeping this assignment behind
// one seam lets startup detection and tests drive the same policy state.
func (s *State) setTerminalCompatibility(termID string, environ []string) {
	s.compat = resolveTerminalCompatibility(termID, environ)
}

// syncUpdatesEnabled reports whether the active backend keeps DEC 2026 around
// its live frame. The Foot and tmux compatibility policies are scoped to Sixel,
// so a backend cycle to Block or Kitty restores the normal fence without
// changing detection.
func (s *State) syncUpdatesEnabled() bool {
	return s.Backend != BackendSixel || !s.compat.sixelSyncUpdatesDisabled
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

// trueColourAllowed reports whether truecolour Block rendering is usable. Rio
// 0.4.x is excluded because its append-only style table is exhausted by
// truecolour Block frames. The auto colour resolution and the Alt+C cycle both
// read it, so the two stay on one policy.
func (c terminalCompatibility) trueColourAllowed() bool {
	return !c.rio04
}

// resolveCompatibleColour applies terminal compatibility after normal colour
// resolution. Rio 0.4.x auto mode is capped at xterm-256 because its append-only
// style table is exhausted by truecolour Block frames. Explicit 256 and none
// retain their normal result; explicit truecolor remains visible for validation.
func resolveCompatibleColour(flag string, environ []string, detected ColourMode, compat terminalCompatibility) ColourMode {
	resolved := resolveColour(flag, environ, detected)
	if flag == "auto" && !compat.trueColourAllowed() && resolved == ColourTrue {
		return ColourXTerm256
	}
	return resolved
}

// resolveColourCapability returns the highest mode that the runtime colour
// cycle can use. An explicit mode raises a lower detected cap because the user
// asserted support, while terminal compatibility remains authoritative. On a
// real TTY the cap is floored at xterm-256: an unknown or degraded profile
// still starts monochrome, but Alt+C can raise colour on user action, because
// every modern interactive terminal renders indexed colour. A non-TTY keeps
// its detected cap, so piped output never gains colour modes.
func resolveColourCapability(flag string, detected ColourMode, compat terminalCompatibility, isTTY bool) ColourMode {
	cap := detected
	if flag != "auto" {
		explicit := resolveColour(flag, nil, detected)
		if explicit < cap {
			cap = explicit
		}
	}
	if isTTY && cap > ColourXTerm256 {
		cap = ColourXTerm256
	}
	if !compat.trueColourAllowed() && cap == ColourTrue {
		return ColourXTerm256
	}
	return cap
}

func (s *State) configureColour(flag string, environ []string, detected ColourMode, isTTY bool) {
	s.Colour = resolveCompatibleColour(flag, environ, detected, s.compat)
	s.colourCap = resolveColourCapability(flag, detected, s.compat, isTTY)
}

// validateForcedBackend applies terminal-specific errors before the generic
// capability check. Rio 0.4.x advertises Sixel but draws no image, so an explicit
// request points to its working alternatives.
func (s *State) validateForcedBackend(mode string) error {
	if s.compat.rio04 && mode == ModeSixel {
		return fmt.Errorf("viewer: Rio 0.4.x does not render Sixel images; use --backend=kitty or --backend=block")
	}
	return ValidateForcedBackend(mode, s.Enabled)
}

// validateColourCompatibility rejects explicit truecolour on affected Rio
// releases regardless of the starting backend. Alt+R cycles every supported
// backend, so truecolour Block frames are always reachable at runtime.
func (s *State) validateColourCompatibility(colourFlag string) error {
	if s.compat.rio04 && colourFlag == "truecolor" {
		return fmt.Errorf("viewer: Rio 0.4.x cannot use truecolour Block rendering; use --color=256, --color=ansi-grey, --color=ansi-tonal, or --color=none")
	}
	return nil
}
