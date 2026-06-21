//go:build !linux

package metrics

// PeakRSS returns the process peak resident set size in bytes. On platforms
// without a getrusage binding wired in it returns zero, so callers stay portable
// and the default build stays green.
func PeakRSS() uint64 {
	return 0
}
