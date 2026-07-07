//go:build darwin

package engine

// audioSinkPresent reports a sink on macOS. CoreAudio is the always-present system
// output, so there is no socket to probe: assume a sink. If the Mac has no output
// device the unmute plays to nothing, degrading to silent, not to an error.
func audioSinkPresent() bool {
	return true
}
