package harness

import (
	"encoding/json"
	"testing"
)

// TestBaselineRoundTrip checks the committed baseline decodes and that
// BaselineFromReports is stable: marshalling it twice is byte-identical. It
// needs no rasteriser, so it stays in the fast default gate.
func TestBaselineRoundTrip(t *testing.T) {
	b, err := LoadBaseline(baselinePath())
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if b == nil {
		t.Skip("no committed baseline; seed it with UPDATE_BASELINE=1")
	}
	a1, err := MarshalBaseline(*b)
	if err != nil {
		t.Fatalf("MarshalBaseline: %v", err)
	}
	var rt Baseline
	if err := json.Unmarshal(a1, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a2, err := MarshalBaseline(rt)
	if err != nil {
		t.Fatalf("MarshalBaseline: %v", err)
	}
	if string(a1) != string(a2) {
		t.Fatal("baseline marshal not stable across a round trip")
	}
}
