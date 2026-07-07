package engine

import (
	"os"
	"path/filepath"
)

// audioAvailable reports whether a host sound sink is present, so the engine
// unmutes Chromium only when audio can reach a speaker. It checks PULSE_SERVER
// first, then probes the XDG runtime dir for a PulseAudio or PipeWire socket. It
// never probes a bogus root path: an unset XDG_RUNTIME_DIR reads as no sink.
func audioAvailable() bool {
	if os.Getenv("PULSE_SERVER") != "" {
		return true
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(xdg, "pulse", "native")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(xdg, "pipewire-0")); err == nil {
		return true
	}
	return false
}
