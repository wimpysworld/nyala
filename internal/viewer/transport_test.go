package viewer

import "testing"

// TestResolveKittyTransport pins every branch of the transport decision order:
// user override > deny list > probe result > allow list > direct fallback. The
// resolver takes goos as a parameter, so the WezTerm OS-qualified entries are
// tested on both linux and darwin regardless of the host.
func TestResolveKittyTransport(t *testing.T) {
	cases := []struct {
		name     string
		override string
		termID   string
		probeSHM bool
		goos     string
		want     KittyTransport
	}{
		// Override branch: direct and shm win outright, bypassing the probe
		// and both lists. An shm override without probe support still returns
		// shm; the user asked.
		{"override direct bypasses positive probe", "direct", "kitty 0.40.1", true, "linux", KittyTransportDirect},
		{"override shm without probe support", "shm", "Konsole 24.12.3", false, "linux", KittyTransportSHM},
		{"override shm beats deny list", "shm", "WezTerm 20240203-110809-5046fc22", true, "darwin", KittyTransportSHM},
		{"override direct on allow-listed terminal", "direct", "WezTerm 20240203-110809-5046fc22", false, "linux", KittyTransportDirect},

		// Deny branch: a hit forces direct before the probe is consulted.
		{"deny hit beats positive probe", "auto", "WezTerm 20240203-110809-5046fc22", true, "darwin", KittyTransportDirect},

		// Probe branch: a positive probe grants shm for any terminal not
		// denied, including unknown and unidentified ones.
		{"known terminal with positive probe", "auto", "kitty 0.40.1", true, "linux", KittyTransportSHM},
		{"unknown terminal with positive probe", "auto", "FutureTerm 1.0", true, "linux", KittyTransportSHM},
		{"empty identity with positive probe", "auto", "", true, "linux", KittyTransportSHM},

		// Allow branch: probe silence still grants shm on a listed terminal
		// with a matching OS qualifier.
		{"allow hit on probe silence (WezTerm linux)", "auto", "WezTerm 20240203-110809-5046fc22", false, "linux", KittyTransportSHM},
		{"allow entry is OS-qualified (WezTerm freebsd)", "auto", "WezTerm 20240203-110809-5046fc22", false, "freebsd", KittyTransportDirect},

		// Fallback branch: no override, no list hit, no probe support.
		{"unknown terminal without probe support", "auto", "FutureTerm 1.0", false, "linux", KittyTransportDirect},
		{"known direct-only terminal", "auto", "Konsole 24.12.3", false, "linux", KittyTransportDirect},
		{"empty identity without probe support", "auto", "", false, "linux", KittyTransportDirect},
		{"empty override treated as auto", "", "", false, "linux", KittyTransportDirect},

		// Identity matching: the name token match is case-insensitive and
		// keyed on the leading token only.
		{"deny match is case-insensitive", "auto", "WEZTERM 20240203", true, "darwin", KittyTransportDirect},
		{"allow match is case-insensitive", "auto", "wezterm 20240203", false, "linux", KittyTransportSHM},
		{"version text never matches a list name", "auto", "kitty wezterm-compat", true, "darwin", KittyTransportSHM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKittyTransport(tc.override, tc.termID, tc.probeSHM, tc.goos)
			if got != tc.want {
				t.Fatalf("resolveKittyTransport(%q, %q, %v, %q) = %q, want %q",
					tc.override, tc.termID, tc.probeSHM, tc.goos, got, tc.want)
			}
		})
	}
}

// TestTerminalNameToken pins the identity normalisation the list match keys on:
// the lower-case leading whitespace-separated token, empty for an empty reply.
func TestTerminalNameToken(t *testing.T) {
	cases := []struct {
		termID string
		want   string
	}{
		{"kitty 0.40.1", "kitty"},
		{"WezTerm 20240203-110809-5046fc22", "wezterm"},
		{"Konsole 24.12.3", "konsole"},
		{"ghostty", "ghostty"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := terminalNameToken(tc.termID); got != tc.want {
			t.Fatalf("terminalNameToken(%q) = %q, want %q", tc.termID, got, tc.want)
		}
	}
}
