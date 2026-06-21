//go:build linux

package metrics

import "syscall"

// PeakRSS returns the process peak resident set size in bytes. On Linux it reads
// ru_maxrss from getrusage, which the kernel reports in kilobytes, and converts
// to bytes. It returns zero if getrusage fails.
func PeakRSS() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Maxrss < 0 {
		return 0
	}
	return uint64(ru.Maxrss) * 1024
}
