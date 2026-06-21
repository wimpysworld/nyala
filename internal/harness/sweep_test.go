package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sweepCards lists the corpus the sweep test runs over: the three test cards and
// the Linux Matters page, matching the four committed fixtures.
var sweepCards = []string{
	"linuxmatters.png",
	"testcard-01-greyscale.png",
	"testcard-02-colour.png",
	"testcard-03-images.png",
}

// baselinePath and thresholdsPath return the committed gate files.
func baselinePath() string   { return filepath.Join("..", "..", "testdata", "baseline.json") }
func thresholdsPath() string { return filepath.Join("..", "..", "testdata", "thresholds.json") }

// updateBaseline reports whether UPDATE_BASELINE is set, mirroring UPDATE_GOLDEN.
func updateBaseline() bool {
	v := os.Getenv("UPDATE_BASELINE")
	return v != "" && v != "0"
}

// pageName turns a fixture filename into its page name, stripping the extension.
func pageName(card string) string {
	return card[:len(card)-len(filepath.Ext(card))]
}

// loadSweepInputs decodes the corpus PNGs into sweep inputs.
func loadSweepInputs(t *testing.T) []SweepInput {
	t.Helper()
	inputs := make([]SweepInput, 0, len(sweepCards))
	for _, card := range sweepCards {
		img := loadRGBA(t, filepath.Join("..", "..", "testdata", card))
		inputs = append(inputs, SweepInput{Page: pageName(card), Src: img})
	}
	return inputs
}

// sweepConfig builds the pinned 120x40 sweep config writing into dir.
func sweepConfig(dir string) SweepConfig {
	return SweepConfig{Cols: CalibCols, Rows: CalibRows, OutDir: dir, NyalaCommit: "test"}
}

// TestSweepEmitsAllFiles runs the full sweep into a temp dir and checks every
// output file and every artefact PNG lands at the pinned grid.
func TestSweepEmitsAllFiles(t *testing.T) {
	rast := newRasteriser(t)
	dir := t.TempDir()
	thresholds, err := LoadThresholds(thresholdsPath())
	if err != nil {
		t.Fatalf("LoadThresholds: %v", err)
	}
	baseline, err := LoadBaseline(baselinePath())
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}

	res, err := RunSweep(rast, sweepConfig(dir), loadSweepInputs(t), baseline, thresholds)
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}

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
			base := filepath.Join(dir, in.Page, mode)
			for _, png := range []string{compositeName, heatmapName, cellMapName, histogramName} {
				if mode != "block" && png == cellMapName {
					// sixel and kitty own no grid, so no cell map.
					continue
				}
				if _, err := os.Stat(filepath.Join(base, png)); err != nil {
					t.Errorf("%s/%s missing %s: %v", in.Page, mode, png, err)
				}
			}
			quant := filepath.Join(base, quantMapName)
			_, qErr := os.Stat(quant)
			if mode == "block" && qErr != nil {
				t.Errorf("%s/block missing quant map: %v", in.Page, qErr)
			}
			if mode != "block" && qErr == nil {
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

// TestSweepGatesAgainstCommittedBaseline runs the sweep against the committed
// baseline and thresholds and asserts a clean run produces no breaches. The
// thresholds file is byte-stable (it is generated, not measured) and is checked
// exactly; the baseline holds measured metrics, and the lossy sixel encoder
// jitters run to run, so the baseline is gated within the thresholds, not
// matched byte for byte. Under UPDATE_BASELINE it rewrites both committed files
// (write-if-changed) from the fresh run.
func TestSweepGatesAgainstCommittedBaseline(t *testing.T) {
	rast := newRasteriser(t)
	dir := t.TempDir()

	thresholds := DefaultThresholds()
	res, err := RunSweep(rast, sweepConfig(dir), loadSweepInputs(t), nil, thresholds)
	if err != nil {
		t.Fatalf("RunSweep (seed): %v", err)
	}
	fresh := BaselineFromReports(Viewport{Cols: CalibCols, Rows: CalibRows}, res.Reports)

	update := updateBaseline()
	if _, bWrote, err := CompareBaseline(baselinePath(), fresh, update); err != nil {
		t.Fatalf("CompareBaseline: %v", err)
	} else if update && bWrote {
		t.Logf("baseline refreshed")
	}
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

	// A clean run against the committed baseline must not breach. The committed
	// baseline is the gate reference; the fresh run's sixel jitter must stay
	// inside the data-driven bands.
	committed, err := LoadBaseline(baselinePath())
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if committed == nil {
		t.Skip("no committed baseline; seed it with UPDATE_BASELINE=1")
	}
	breaches := gate(res.Reports, committed, thresholds)
	if len(breaches) != 0 {
		for _, b := range breaches {
			t.Errorf("unexpected breach against committed baseline: %s", b)
		}
	}
}

// TestSweepBreachExitsNonZero checks a simulated quality drop and a simulated
// cost rise past threshold each surface as a breach, so the sweep would exit
// non-zero.
func TestSweepBreachExitsNonZero(t *testing.T) {
	rast := newRasteriser(t)
	dir := t.TempDir()
	res, err := RunSweep(rast, sweepConfig(dir), loadSweepInputs(t), nil, DefaultThresholds())
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	base := BaselineFromReports(Viewport{Cols: CalibCols, Rows: CalibRows}, res.Reports)

	// Simulate a quality drop: lift the baseline SSIM well above the band on the
	// first entry, so the measured value reads as a regression.
	drop := *base.Entries[0].Metrics.SSIM + 0.1
	base.Entries[0].Metrics.SSIM = &drop

	// Simulate a cost rise: cut the baseline byte count so the measured value
	// reads as a large rise on the second entry.
	half := *base.Entries[1].Metrics.BytesPerFrame / 2
	base.Entries[1].Metrics.BytesPerFrame = &half

	breaches := gate(res.Reports, &base, DefaultThresholds())
	if len(breaches) < 2 {
		t.Fatalf("got %d breaches, want at least 2 (a quality drop and a cost rise)", len(breaches))
	}
	var sawQuality, sawCost bool
	for _, b := range breaches {
		if b.Metric == "ssim" {
			sawQuality = true
		}
		if b.Metric == bytesPerFrameMetric {
			sawCost = true
		}
	}
	if !sawQuality {
		t.Error("no SSIM quality-drop breach")
	}
	if !sawCost {
		t.Error("no bytesPerFrame cost-rise breach")
	}
}

// TestSweepWithinThresholdPasses checks a baseline nudged within the band passes
// the gate, so ordinary jitter does not fail the run.
func TestSweepWithinThresholdPasses(t *testing.T) {
	rast := newRasteriser(t)
	dir := t.TempDir()
	res, err := RunSweep(rast, sweepConfig(dir), loadSweepInputs(t), nil, DefaultThresholds())
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	base := BaselineFromReports(Viewport{Cols: CalibCols, Rows: CalibRows}, res.Reports)

	// Nudge the first entry's SSIM baseline up by half the band: still inside it.
	nudged := *base.Entries[0].Metrics.SSIM + 0.01
	base.Entries[0].Metrics.SSIM = &nudged

	breaches := gate(res.Reports, &base, DefaultThresholds())
	for _, b := range breaches {
		if b.Page == base.Entries[0].Page && b.Mode == base.Entries[0].Mode && b.Metric == "ssim" {
			t.Errorf("within-band nudge breached: %s", b)
		}
	}
}

// TestBaselineRoundTrip checks the committed baseline decodes and that
// BaselineFromReports is stable: marshalling it twice is byte-identical.
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
