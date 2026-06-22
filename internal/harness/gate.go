// The regression gate: it compares a fresh sweep against the committed baseline
// under the thresholds and returns a breach for every metric that moved the
// wrong way past its band. The threshold and baseline model and their
// persistence live in threshold.go; this file holds only the gate algorithm.
//
// Channel order is RGBA throughout (the gate reads only scalar metrics).
package harness

// gatedMetric is one entry of the gate's quality-metric table: the metric name,
// the report accessor, and the baseline accessor. Driving the gate from one
// table keeps the six gated-metric tuple in step across the report, the
// baseline, and the check loop, so adding a quality metric is one entry rather
// than scattered edits.
type gatedMetric struct {
	// name is the gated metric's name, the threshold map key and breach label.
	name metricName
	// quality reads the metric's measured value from a report, or nil when the
	// metric does not apply to that mode.
	quality func(Report) *float64
	// baseline reads the metric's committed value from a baseline snapshot, or
	// nil when the snapshot did not record it.
	baseline func(BaselineMetrics) *float64
}

// gatedMetrics is the quality-metric gate table, in the order the gate checks
// them. The bytesPerFrame cost metric is not here: its band is relative to the
// baseline, so gateOne handles it as a special case after this loop.
var gatedMetrics = []gatedMetric{
	{ssimMetric, func(r Report) *float64 { return r.Quality.SSIM }, func(b BaselineMetrics) *float64 { return b.SSIM }},
	{msssimMetric, func(r Report) *float64 { return r.Quality.MSSSIM }, func(b BaselineMetrics) *float64 { return b.MSSSIM }},
	{psnrMetric, func(r Report) *float64 { return r.Quality.PSNR }, func(b BaselineMetrics) *float64 { return b.PSNR }},
	{deltaE2000MeanMetric, func(r Report) *float64 { return r.Quality.DeltaE2000Mean }, func(b BaselineMetrics) *float64 { return b.DeltaE2000Mean }},
	{histDivergenceMetric, func(r Report) *float64 { return r.Quality.HistDivergence }, func(b BaselineMetrics) *float64 { return b.HistDivergence }},
}

// gate compares each report against the baseline under the thresholds and
// returns every metric that regressed past its band. A nil baseline skips the
// gate (a first run seeds the baseline rather than failing). A report with no
// matching baseline entry is skipped, so adding a page never fails the gate.
func gate(reports []Report, baseline *Baseline, thresholds Thresholds) []Breach {
	if baseline == nil {
		return nil
	}
	var breaches []Breach
	for _, r := range reports {
		base, ok := baseline.lookup(r.Page, r.Mode)
		if !ok {
			continue
		}
		breaches = append(breaches, gateOne(r, base, thresholds)...)
	}
	return breaches
}

// gateOne checks one report's gated metrics against its baseline entry. It
// drives the quality metrics from the gatedMetrics table, then handles the
// relative-band bytesPerFrame cost metric as a special case.
func gateOne(r Report, base BaselineMetrics, t Thresholds) []Breach {
	var out []Breach
	check := func(metric metricName, measured, baseline *float64) {
		if measured == nil || baseline == nil {
			return
		}
		th, ok := t.thresholdFor(r.Mode, metric)
		if !ok {
			return
		}
		if br, breached := evalBand(r.Page, r.Mode, metric, *baseline, *measured, th); breached {
			out = append(out, br)
		}
	}
	for _, m := range gatedMetrics {
		check(m.name, m.quality(r), m.baseline(base))
	}

	if th, ok := t.thresholdFor(r.Mode, bytesPerFrameMetric); ok && r.Perf.BytesPerFrame != nil && base.BytesPerFrame != nil {
		measured := float64(*r.Perf.BytesPerFrame)
		baseline := float64(*base.BytesPerFrame)
		// The byte band is relative: a fraction of the baseline frame size,
		// since payload sizes span orders of magnitude across the backends.
		effBand := th.Band * baseline
		if br, breached := evalBand(r.Page, r.Mode, bytesPerFrameMetric, baseline, measured, MetricThreshold{Band: effBand, HigherIsBetter: th.HigherIsBetter}); breached {
			out = append(out, br)
		}
	}
	return out
}

// evalBand decides whether a single metric breached its band. For a quality
// metric (HigherIsBetter) the regression is a drop, so it breaches when the
// measured value falls more than Band below the baseline. For a cost metric it
// breaches when the measured value rises more than Band above the baseline. A
// move the good way, or within the band, passes. The recorded band is th.Band,
// the same band the gate compares against, so the breach's delta and band carry
// the same units.
func evalBand(page, mode string, metric metricName, baseline, measured float64, th MetricThreshold) (Breach, bool) {
	delta := measured - baseline
	var breached bool
	if th.HigherIsBetter {
		// Regression is a drop: measured below baseline by more than the band.
		breached = baseline-measured > th.Band+gateEps
	} else {
		// Regression is a rise: measured above baseline by more than the band.
		breached = measured-baseline > th.Band+gateEps
	}
	if !breached {
		return Breach{}, false
	}
	return Breach{
		Page:     page,
		Mode:     mode,
		Metric:   string(metric),
		Baseline: baseline,
		Measured: measured,
		Delta:    delta,
		Band:     th.Band,
	}, true
}

// gateEps absorbs floating-point noise at the band edge so a value exactly on
// the band does not flap between pass and fail across runs.
const gateEps = 1e-9
