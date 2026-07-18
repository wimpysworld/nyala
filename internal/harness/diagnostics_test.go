//go:build diagnostics

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiagnostics is the explicit, artefact-producing gate run behind the
// `diagnostics` build tag. Unlike the default sweep tests it writes its sweep
// artefacts to testdata/diagnostics/ and fails listing every breach, so the tag
// keeps it out of the normal `just test` gate and reserves it for `just
// diagnostics`. It runs the shared corpus sweep once and gates on the committed
// baseline and thresholds.
//
// It also runs the golden track, so `just update-golden` (this test with
// UPDATE_GOLDEN=1) regenerates the committed testdata/golden archives. The
// UPDATE_GOLDEN flag is read from the environment and neither set nor unset here,
// so a plain run leaves the golden archives untouched.
func TestDiagnostics(t *testing.T) {
	res, _ := sharedSweep(t)

	if len(res.Breaches) > 0 {
		for _, b := range res.Breaches {
			t.Errorf("threshold breach: %s", b)
		}
		t.Fatalf("diagnostics: %d threshold breach(es)", len(res.Breaches))
	}

	// Run the golden track so UPDATE_GOLDEN=1 regenerates the committed archives
	// through this test. A plain run compares and fails on drift.
	update := updateGolden()
	for _, fx := range goldenFixtures {
		g := goldenGrid(t, fx.png)
		ok, _, err := CompareGolden(goldenPath(fx.page), fx.page, g, nil, update)
		if err != nil {
			t.Fatalf("CompareGolden %s: %v", fx.page, err)
		}
		if !ok {
			t.Fatalf("golden mismatch for %s; rerun with UPDATE_GOLDEN=1 to refresh", fx.page)
		}
	}
}

// TestSweepEmitsAllFiles checks the shared sweep emitted every report file and
// every artefact PNG at the pinned grid. It reads the shared sweep result, so it
// adds no new sweep.
func TestSweepEmitsAllFiles(t *testing.T) {
	res, dir := sharedSweep(t)

	wantRecords := len(sweepCards) * len(SweepModes)
	if len(res.Reports) != wantRecords {
		t.Fatalf("got %d reports, want %d", len(res.Reports), wantRecords)
	}

	for _, name := range []string{res.ReportJSON, res.ReportCSV, res.SummaryMD} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("report file missing: %v", err)
		}
	}

	// Every (page, mode) emits its PNGs; block also emits the quant map.
	for _, in := range loadSweepInputs(t) {
		for _, mode := range SweepModes {
			base := filepath.Join(dir, in.Page, string(mode))
			for _, png := range []string{compositeName, heatmapName, cellMapName, histogramName} {
				if mode != ModeBlock && png == cellMapName {
					// sixel and kitty own no grid, so no cell map.
					continue
				}
				if _, err := os.Stat(filepath.Join(base, png)); err != nil {
					t.Errorf("%s/%s missing %s: %v", in.Page, mode, png, err)
				}
			}
			quant := filepath.Join(base, quantMapName)
			_, qErr := os.Stat(quant)
			if mode == ModeBlock && qErr != nil {
				t.Errorf("%s/block missing quant map: %v", in.Page, qErr)
			}
			if mode != ModeBlock && qErr == nil {
				t.Errorf("%s/%s wrote a quant map, want none", in.Page, mode)
			}
		}
	}

	// Every report carries the pinned grid.
	for _, r := range res.Reports {
		if r.Viewport.Cols != CalibCols || r.Viewport.Rows != CalibRows {
			t.Errorf("%s/%s grid %dx%d, want %dx%d", r.Page, r.Mode,
				r.Viewport.Cols, r.Viewport.Rows, CalibCols, CalibRows)
		}
	}
}

// TestSweepGatesAgainstCommittedBaseline asserts a clean run of the shared sweep
// produces no breaches against the committed baseline, and refreshes the
// committed baseline and thresholds under UPDATE_BASELINE (write-if-changed).
// The thresholds file is byte-stable (generated, not measured) and checked
// exactly; the baseline holds measured metrics and is gated within the
// thresholds rather than matched byte for byte.
func TestSweepGatesAgainstCommittedBaseline(t *testing.T) {
	res, _ := sharedSweep(t)
	fresh := BaselineFromReports(Viewport{Cols: CalibCols, Rows: CalibRows}, res.Reports)

	update := updateBaseline()
	if _, bWrote, err := CompareBaseline(baselinePath(), fresh, update); err != nil {
		t.Fatalf("CompareBaseline: %v", err)
	} else if update && bWrote {
		t.Logf("baseline refreshed")
	}
	thresholds := DefaultThresholds()
	tOK, tWrote, err := CompareThresholds(thresholdsPath(), thresholds, update)
	if err != nil {
		t.Fatalf("CompareThresholds: %v", err)
	}
	if !tOK {
		t.Fatalf("thresholds mismatch; rerun with UPDATE_BASELINE=1 to refresh")
	}
	if update && tWrote {
		t.Logf("thresholds refreshed")
	}

	committed, err := LoadBaseline(baselinePath())
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if committed == nil {
		t.Skip("no committed baseline; seed it with UPDATE_BASELINE=1")
	}
	breaches := gate(res.Reports, committed, thresholds)
	for _, b := range breaches {
		t.Errorf("unexpected breach against committed baseline: %s", b)
	}
}
