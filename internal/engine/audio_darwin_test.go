//go:build darwin

package engine

import "testing"

func TestAudioAvailableDarwinCoreAudio(t *testing.T) {
	// On macOS CoreAudio is the always-present system output, so a sink reads as
	// present with no PULSE_SERVER and no socket to probe.
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if !audioAvailable() {
		t.Error("audioAvailable() = false, want true on darwin (CoreAudio present)")
	}
}
