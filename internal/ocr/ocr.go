// Package ocr provides pure-Go glyph matching and recognition scoring.
//
// Nyala uses a pure-Go nearest-glyph matcher for the overlay quality gate (see
// matcher.go). This file holds the Character Error Rate and Word Error Rate
// scoring shared by the matcher tests and diagnostics.
package ocr

import (
	"strings"

	"github.com/agnivade/levenshtein"
)

// Result is one OCR round-trip outcome: the recognised text scored against the
// known truth with Character Error Rate and Word Error Rate.
type Result struct {
	Truth string  // the known ground-truth string baked into the fixture
	Got   string  // the text the engine recovered
	CER   float64 // character error rate, edit distance over truth length
	WER   float64 // word error rate, token edit distance over truth word count
}

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

// WER returns the Word Error Rate of got against truth: the token-level
// Levenshtein edit distance over whitespace-split words, divided by the truth
// word count. An empty truth returns 0 when got is also empty, else 1.
func WER(truth, got string) float64 {
	tw := strings.Fields(truth)
	gw := strings.Fields(got)
	if len(tw) == 0 {
		if len(gw) == 0 {
			return 0
		}
		return 1
	}
	d := wordDistance(tw, gw)
	return float64(d) / float64(len(tw))
}

// Score builds a Result for got against truth, computing CER and WER.
func Score(truth, got string) Result {
	return Result{
		Truth: truth,
		Got:   got,
		CER:   CER(truth, got),
		WER:   WER(truth, got),
	}
}

// wordDistance is the Levenshtein edit distance over two token slices, the
// word-level analogue of the rune-level distance levenshtein computes.
func wordDistance(a, b []string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
