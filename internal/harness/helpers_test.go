package harness

import "path/filepath"

// baselinePath returns the committed baseline gate file. It is read by the fast
// TestBaselineRoundTrip in the default gate and by the heavy diagnostics gate,
// so it stays untagged.
func baselinePath() string { return filepath.Join("..", "..", "testdata", "baseline.json") }
