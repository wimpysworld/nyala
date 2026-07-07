package engine

import "testing"

func TestAudioAvailablePulseServer(t *testing.T) {
	// PULSE_SERVER set is the short circuit: a sink is present on any OS without
	// probing the platform check at all.
	t.Setenv("PULSE_SERVER", "unix:/run/pulse")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if !audioAvailable() {
		t.Error("audioAvailable() = false, want true when PULSE_SERVER is set")
	}
}
