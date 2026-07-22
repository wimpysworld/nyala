package viewer

import "strings"

// KittyTransport is the Kitty graphics transmission medium the viewer uses for
// live frames. The flag surface carries three values; the resolver only ever
// returns direct or shm, so the rest of the viewer never sees auto.
type KittyTransport string

const (
	// KittyTransportAuto asks the resolver to pick the transport from the deny
	// list, the startup probe, and the allow list. It is a flag value only;
	// resolveKittyTransport never returns it.
	KittyTransportAuto KittyTransport = "auto"
	// KittyTransportDirect is the in-band t=d PNG path, the universal fallback.
	KittyTransportDirect KittyTransport = "direct"
	// KittyTransportSHM is the POSIX shared-memory t=s raw f=32 fast path.
	KittyTransportSHM KittyTransport = "shm"
)

// transportListEntry keys one allow or deny entry on a normalised terminal
// identity: the lower-case leading name token of the XTVERSION reply, with an
// optional GOOS qualifier. An empty goos matches every OS.
type transportListEntry struct {
	name string // lower-case terminal name token, matched as a prefix
	goos string // runtime.GOOS qualifier; empty matches every OS
}

// kittySHMAllow lists terminals that support the shm transport but stay silent
// on the a=q probe, so probe silence must not demote them to direct. The list
// is seeded from the research matrix and grown only by live testing.
var kittySHMAllow = []transportListEntry{
	// WezTerm on Linux supports t=s (wezterm PR #1810) but never acks the
	// probe, so detection relies on this entry rather than the DA1 timeout.
	{name: "wezterm", goos: "linux"},
}

// kittySHMDeny lists terminals whose shm transport is broken even when the
// probe appears to succeed. A deny hit forces the direct fallback before the
// probe result is consulted.
var kittySHMDeny = []transportListEntry{
	// WezTerm on macOS accepts t=s but renders it wrongly (wezterm #7631).
	{name: "wezterm", goos: "darwin"},
}

// resolveKittyTransport returns the effective Kitty transport for one run. The
// decision order is fixed: a direct or shm override wins outright (an shm
// override without probe support still returns shm, the user asked), then a
// deny-list hit forces direct, then a positive probe grants shm, then an
// allow-list hit grants shm despite probe silence, and everything else falls
// back to direct. override is the --kitty-transport flag value, termID the
// XTVERSION identity (may be empty), probeSHM the per-id a=q probe result, and
// goos the runtime.GOOS the lists qualify on (a parameter for testability).
func resolveKittyTransport(override, termID string, probeSHM bool, goos string) KittyTransport {
	switch KittyTransport(override) {
	case KittyTransportDirect:
		return KittyTransportDirect
	case KittyTransportSHM:
		return KittyTransportSHM
	}
	if matchTransportList(kittySHMDeny, termID, goos) {
		return KittyTransportDirect
	}
	if probeSHM {
		return KittyTransportSHM
	}
	if matchTransportList(kittySHMAllow, termID, goos) {
		return KittyTransportSHM
	}
	return KittyTransportDirect
}

// matchTransportList reports whether the terminal identified by termID on goos
// hits an entry in list. The match is a case-insensitive prefix match on the
// leading name token of termID; an empty termID never matches.
func matchTransportList(list []transportListEntry, termID, goos string) bool {
	name := terminalNameToken(termID)
	if name == "" {
		return false
	}
	for _, e := range list {
		if e.goos != "" && e.goos != goos {
			continue
		}
		if strings.HasPrefix(name, e.name) {
			return true
		}
	}
	return false
}

// terminalNameToken extracts the lower-case leading whitespace-separated token
// from an XTVERSION identity, e.g. "wezterm" from
// "WezTerm 20240203-110809-5046fc22". It returns "" for an empty identity.
func terminalNameToken(termID string) string {
	fields := strings.Fields(termID)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}
