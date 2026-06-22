package harness

import "testing"

// The gate logic (gate, gateOne, evalBand, the relative bytesPerFrame band) is
// the heart of the regression check, so it keeps fast coverage here with
// hand-built reports rather than a real corpus sweep. These tests run no
// rasteriser, so they stay in the default `just test` gate.

// fp returns a pointer to f, for the nullable metric fields.
func fp(f float64) *float64 { return &f }

// ip returns a pointer to n, for the nullable byte-count fields.
func ip(n int) *int { return &n }

// syntheticFrameBytes is the fixed frame byte cost every synthetic report
// carries. The cost-rise test breaches by mutating the baseline below this, not
// by changing the report, so one constant suffices.
const syntheticFrameBytes = 1000

// syntheticReport builds one block report with a quality SSIM and the fixed
// frame byte cost, the two metrics the gate tests exercise.
func syntheticReport(page string, ssim float64) Report {
	return Report{
		Page:     page,
		Mode:     "block",
		Viewport: Viewport{Cols: CalibCols, Rows: CalibRows},
		Quality:  Quality{SSIM: fp(ssim)},
		Perf:     Perf{BytesPerFrame: ip(syntheticFrameBytes)},
	}
}

// syntheticBaseline mirrors a report set as a baseline snapshot.
func syntheticBaseline(reports []Report) Baseline {
	return BaselineFromReports(Viewport{Cols: CalibCols, Rows: CalibRows}, reports)
}

// TestGateBreachesOnQualityDropAndCostRise checks a simulated quality drop and a
// simulated cost rise each surface as a breach, so the sweep would exit
// non-zero. It uses hand-built reports, no rasteriser.
func TestGateBreachesOnQualityDropAndCostRise(t *testing.T) {
	reports := []Report{
		syntheticReport("p1", 0.80),
		syntheticReport("p2", 0.80),
	}
	base := syntheticBaseline(reports)

	// Quality drop: lift the baseline SSIM on p1 well above the 0.02 band, so the
	// measured 0.80 reads as a regression.
	drop := *base.Entries[0].Metrics.SSIM + 0.1
	base.Entries[0].Metrics.SSIM = &drop

	// Cost rise: halve the baseline bytes on p2, so the measured 1000 reads as a
	// large rise past the relative 5% band.
	half := *base.Entries[1].Metrics.BytesPerFrame / 2
	base.Entries[1].Metrics.BytesPerFrame = &half

	breaches := gate(reports, &base, DefaultThresholds())
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

// TestGateWithinBandPasses checks a baseline nudged within the band does not
// breach, so ordinary jitter never fails the run.
func TestGateWithinBandPasses(t *testing.T) {
	reports := []Report{syntheticReport("p1", 0.75)}
	base := syntheticBaseline(reports)

	// Nudge the baseline SSIM up by half the 0.02 band: still inside it.
	nudged := *base.Entries[0].Metrics.SSIM + 0.01
	base.Entries[0].Metrics.SSIM = &nudged

	breaches := gate(reports, &base, DefaultThresholds())
	for _, b := range breaches {
		if b.Page == "p1" && b.Metric == "ssim" {
			t.Errorf("within-band nudge breached: %s", b)
		}
	}
}

// TestGateNilBaselineSeeds checks a nil baseline reports no breaches, the path
// that seeds a first baseline rather than failing.
func TestGateNilBaselineSeeds(t *testing.T) {
	reports := []Report{syntheticReport("p1", 0.80)}
	if breaches := gate(reports, nil, DefaultThresholds()); len(breaches) != 0 {
		t.Fatalf("nil baseline returned %d breaches, want 0", len(breaches))
	}
}
