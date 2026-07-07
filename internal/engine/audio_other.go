//go:build !linux && !darwin

package engine

// audioSinkPresent reports no detectable sink on platforms other than Linux and
// macOS, so audio stays muted there. Those platforms are not a support target;
// Nyala aims at Linux and macOS.
func audioSinkPresent() bool {
	return false
}
