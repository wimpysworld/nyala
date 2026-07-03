package app

import (
	"fmt"
	"strconv"
)

// CaptureMode is the parsed NYALA_CAPTURE result. Enabled reports whether
// capture runs; N is the number of frames to record when Enabled; ScrollPx is
// the capture scroll hook's requested vertical scroll in CSS pixels.
type CaptureMode struct {
	// Enabled reports whether bounded capture runs.
	Enabled bool
	// N is the number of frames to record. It is meaningful only when Enabled.
	N int
	// ScrollPx is the requested vertical scroll in CSS pixels from
	// NYALA_CAPTURE_SCROLL, zero when unset. It is meaningful only when Enabled;
	// off the capture path the scroll env is ignored.
	ScrollPx int
	// ScrollSettle gates the capture scroll hook's settle wait, from
	// NYALA_CAPTURE_SCROLL_SETTLE. It defaults to true (wait for the settled
	// overlay). Set to "0" the hook skips the wait and draws the first frame whose
	// scroll offset advanced, the pre-settle state, so a test can check the overlay
	// stays glued to the scrolled bitmap before the settle snapshot lands. It is
	// meaningful only when Enabled with a positive ScrollPx.
	ScrollSettle bool
}

// ParseCaptureEnv parses a NYALA_CAPTURE value plus the NYALA_CAPTURE_SCROLL and
// NYALA_CAPTURE_SCROLL_SETTLE values. Each is a value and a set flag, matching
// os.LookupEnv, so Run passes the lookup results straight through. It reads no
// environment and is pure. An unset capture value (captureSet=false) and "0"
// parse to disabled. A positive integer parses to enabled with that frame count.
// A negative number, a non-integer, or an empty set value returns an error naming
// NYALA_CAPTURE. The scroll value is parsed only when capture is enabled: unset or
// "0" means no scroll; a positive integer sets ScrollPx; a negative number or a
// non-integer returns an error naming NYALA_CAPTURE_SCROLL. The settle value
// defaults to true: unset or any value other than "0" leaves the settle wait on;
// "0" turns it off.
func ParseCaptureEnv(value string, captureSet bool, scroll string, scrollSet bool, settle string, settleSet bool) (CaptureMode, error) {
	if !captureSet {
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

	scrollPx, err := parseScrollEnv(scroll, scrollSet)
	if err != nil {
		return CaptureMode{}, err
	}
	return CaptureMode{Enabled: true, N: n, ScrollPx: scrollPx, ScrollSettle: parseSettleEnv(settle, settleSet)}, nil
}

// parseSettleEnv parses a NYALA_CAPTURE_SCROLL_SETTLE value. It defaults to true:
// unset (set=false) or any value other than "0" keeps the settle wait on; "0"
// turns it off.
func parseSettleEnv(value string, set bool) bool {
	if !set {
		return true
	}
	return value != "0"
}

// parseScrollEnv parses a NYALA_CAPTURE_SCROLL value. Unset (set=false) and "0"
// parse to zero (no scroll). A positive integer parses to that CSS-pixel scroll.
// A negative number, a non-integer, or an empty set value returns an error
// naming NYALA_CAPTURE_SCROLL.
func parseScrollEnv(value string, set bool) (int, error) {
	if !set {
		return 0, nil
	}
	px, err := strconv.Atoi(value)
	if err != nil || px < 0 {
		return 0, fmt.Errorf("NYALA_CAPTURE_SCROLL=%q: want a non-negative integer CSS-pixel scroll", value)
	}
	return px, nil
}
