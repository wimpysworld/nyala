package app

import (
	"fmt"
	"strconv"
)

// CaptureMode is the parsed NYALA_CAPTURE result. Enabled reports whether
// capture runs; N is the number of frames to record when Enabled.
type CaptureMode struct {
	// Enabled reports whether bounded capture runs.
	Enabled bool
	// N is the number of frames to record. It is meaningful only when Enabled.
	N int
}

// ParseCaptureEnv parses a NYALA_CAPTURE value. It takes the value and a set
// flag, matching os.LookupEnv, so Run passes the lookup result straight through.
// It reads no environment and is pure. An unset value (set=false) and "0" parse
// to disabled. A positive integer parses to enabled with that frame count. A
// negative number, a non-integer, or an empty set value returns an error naming
// NYALA_CAPTURE.
func ParseCaptureEnv(value string, set bool) (CaptureMode, error) {
	if !set {
		return CaptureMode{}, nil
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return CaptureMode{}, fmt.Errorf("NYALA_CAPTURE=%q: want a non-negative integer frame count", value)
	}
	if n < 0 {
		return CaptureMode{}, fmt.Errorf("NYALA_CAPTURE=%q: want a non-negative integer frame count", value)
	}
	if n == 0 {
		return CaptureMode{}, nil
	}
	return CaptureMode{Enabled: true, N: n}, nil
}
