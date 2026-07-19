// Package ocr provides pure-Go glyph matching and recognition scoring.
//
// Nyala uses a pure-Go nearest-glyph matcher for the overlay quality gate (see
// matcher.go). This file holds the Character Error Rate scoring shared by the
// matcher tests and diagnostics.
package ocr

import "github.com/agnivade/levenshtein"

// CER returns the Character Error Rate of got against truth: the Levenshtein
// edit distance between the two strings divided by the truth length. An empty
// truth returns 0 when got is also empty, else 1.
func CER(truth, got string) float64 {
	if truth == "" {
		if got == "" {
			return 0
		}
		return 1
	}
	d := levenshtein.ComputeDistance(truth, got)
	return float64(d) / float64(len([]rune(truth)))
}
