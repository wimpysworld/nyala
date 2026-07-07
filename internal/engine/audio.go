package engine

import "os"

// audioAvailable reports whether a host sound sink is present, so the engine
// unmutes Chromium only when audio can reach a speaker. PULSE_SERVER set marks a
// sink on any OS; otherwise the check is platform-specific (audioSinkPresent):
// Linux probes the PulseAudio and PipeWire sockets, macOS assumes CoreAudio, and
// other platforms report no sink.
func audioAvailable() bool {
	if os.Getenv("PULSE_SERVER") != "" {
		return true
	}
	return audioSinkPresent()
}
