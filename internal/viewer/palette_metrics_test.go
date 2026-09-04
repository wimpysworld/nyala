package viewer

import (
	"reflect"
	"strings"
	"testing"
)

func TestPaletteSessionMetricsDropsPreGateObservation(t *testing.T) {
	var metrics paletteSessionMetrics
	metrics.record(false, completePaletteObservation(), 4096)

	if !reflect.DeepEqual(metrics, paletteSessionMetrics{}) {
		t.Fatalf("pre-gate observation changed metrics: %+v", metrics)
	}
}

func TestPaletteSessionMetricsRecordsReuse(t *testing.T) {
	var metrics paletteSessionMetrics
	metrics.record(true, paletteFrameObservation{
		paletteChecks:  1,
		paletteCheckMS: 0.125,
	}, 512)

	if metrics.paletteChecks != 1 || metrics.paletteReuseFailures != 0 {
		t.Fatalf("reuse totals = checks %d, failures %d", metrics.paletteChecks, metrics.paletteReuseFailures)
	}
	if metrics.outBytes != 512 {
		t.Fatalf("out bytes = %d, want 512", metrics.outBytes)
	}
	if !reflect.DeepEqual(metrics.paletteCheckMS, []float64{0.125}) {
		t.Fatalf("check samples = %v, want [0.125]", metrics.paletteCheckMS)
	}
	if len(metrics.paletteFitMS) != 0 || len(metrics.paletteOSCMS) != 0 {
		t.Fatalf("reuse recorded fit or OSC samples: fit=%v OSC=%v", metrics.paletteFitMS, metrics.paletteOSCMS)
	}
}

func TestPaletteSessionMetricsRecordsChangedAndNoOpRefits(t *testing.T) {
	var metrics paletteSessionMetrics
	metrics.record(true, paletteFrameObservation{
		paletteChecks:         1,
		paletteReuseFailures:  1,
		paletteChangedEntries: 3,
		paletteUpdates:        1,
		paletteOSCEntries:     16,
		paletteOSCSequences:   3,
		paletteOSCBytes:       384,
		paletteFullRepaints:   1,
		paletteCheckMS:        0.2,
		paletteFitMS:          4,
		paletteOSCMS:          0.3,
	}, 1000)
	metrics.record(true, paletteFrameObservation{
		paletteChecks:            1,
		paletteReuseFailures:     1,
		paletteLengthEqualRefits: 1,
		paletteEqualRefits:       1,
		paletteLUTEqualRefits:    1,
		paletteNoOpRefits:        1,
		paletteCheckMS:           0.4,
		paletteFitMS:             6,
	}, 1200)

	if metrics.paletteChecks != 2 || metrics.paletteReuseFailures != 2 {
		t.Fatalf("refit totals = checks %d, failures %d", metrics.paletteChecks, metrics.paletteReuseFailures)
	}
	if metrics.paletteLengthEqualRefits != 1 || metrics.paletteEqualRefits != 1 || metrics.paletteLUTEqualRefits != 1 {
		t.Fatalf("equality totals = length %d, palette %d, LUT %d", metrics.paletteLengthEqualRefits, metrics.paletteEqualRefits, metrics.paletteLUTEqualRefits)
	}
	if metrics.paletteNoOpRefits != 1 || metrics.paletteChangedEntries != 3 {
		t.Fatalf("result totals = no-op %d, changed entries %d", metrics.paletteNoOpRefits, metrics.paletteChangedEntries)
	}
	if metrics.paletteUpdates != 1 || metrics.paletteOSCEntries != 16 || metrics.paletteOSCSequences != 3 || metrics.paletteOSCBytes != 384 || metrics.paletteFullRepaints != 1 {
		t.Fatalf("update totals = updates %d, entries %d, sequences %d, bytes %d, repaints %d", metrics.paletteUpdates, metrics.paletteOSCEntries, metrics.paletteOSCSequences, metrics.paletteOSCBytes, metrics.paletteFullRepaints)
	}
	if metrics.outBytes != 2200 {
		t.Fatalf("out bytes = %d, want 2200", metrics.outBytes)
	}
	if !reflect.DeepEqual(metrics.paletteFitMS, []float64{4, 6}) || !reflect.DeepEqual(metrics.paletteOSCMS, []float64{0.3}) {
		t.Fatalf("duration samples = fit %v, OSC %v", metrics.paletteFitMS, metrics.paletteOSCMS)
	}
}

func TestPaletteSessionMetricsSummaryStatisticsAndRates(t *testing.T) {
	var metrics paletteSessionMetrics
	for i := 10; i > 0; i-- {
		metrics.record(true, paletteFrameObservation{
			paletteChecks:        1,
			paletteReuseFailures: 1,
			paletteUpdates:       1,
			paletteOSCBytes:      20,
			paletteCheckMS:       float64(i),
			paletteFitMS:         float64(i * 10),
			paletteOSCMS:         float64(i) / 10,
		}, 100)
	}

	got := metrics.summaryFragment(2)
	for _, want := range []string{
		"palette_updates_per_s=5.0",
		"palette_osc_bytes_per_s=100.0",
		"out_bytes_per_s=500.0",
		"palette_check_mean_ms=5.500 palette_check_p90_ms=9.000 palette_check_max_ms=10.000",
		"palette_fit_mean_ms=55.000 palette_fit_p90_ms=90.000 palette_fit_max_ms=100.000",
		"palette_osc_mean_ms=0.550 palette_osc_p90_ms=0.900 palette_osc_max_ms=1.000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not contain %q", got, want)
		}
	}
	if want := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}; !reflect.DeepEqual(metrics.paletteCheckMS, want) {
		t.Fatalf("summary did not sort check samples: got %v, want %v", metrics.paletteCheckMS, want)
	}
}

func TestPaletteSessionMetricsSummaryZeroWallRates(t *testing.T) {
	metrics := paletteSessionMetrics{
		paletteUpdates:  3,
		paletteOSCBytes: 900,
		outBytes:        1200,
	}
	got := metrics.summaryFragment(0)
	for _, want := range []string{
		"palette_updates_per_s=0.0",
		"palette_osc_bytes_per_s=0.0",
		"out_bytes_per_s=0.0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("zero-wall summary %q does not contain %q", got, want)
		}
	}
}

func TestPaletteSessionMetricsSummaryFieldOrderAndUniqueness(t *testing.T) {
	got := (&paletteSessionMetrics{}).summaryFragment(1)
	if strings.ContainsRune(got, '\n') {
		t.Fatalf("summary fragment contains a newline: %q", got)
	}

	wantFields := []string{
		"palette_checks", "palette_reuse_failures", "palette_length_equal_refits",
		"palette_equal_refits", "palette_lut_equal_refits", "palette_noop_refits",
		"palette_changed_entries", "palette_updates", "palette_updates_per_s",
		"palette_osc_entries", "palette_osc_sequences", "palette_osc_bytes",
		"palette_osc_bytes_per_s", "palette_full_repaints", "out_bytes", "out_bytes_per_s",
		"palette_check_mean_ms", "palette_check_p90_ms", "palette_check_max_ms",
		"palette_fit_mean_ms", "palette_fit_p90_ms", "palette_fit_max_ms",
		"palette_osc_mean_ms", "palette_osc_p90_ms", "palette_osc_max_ms",
	}
	fields := strings.Fields(got)
	if len(fields) != len(wantFields) {
		t.Fatalf("summary has %d fields, want %d: %q", len(fields), len(wantFields), got)
	}
	seen := make(map[string]bool, len(fields))
	for i, field := range fields {
		name, _, ok := strings.Cut(field, "=")
		if !ok {
			t.Fatalf("field %q has no value separator", field)
		}
		if name != wantFields[i] {
			t.Fatalf("field %d = %q, want %q", i, name, wantFields[i])
		}
		if seen[name] {
			t.Fatalf("field %q occurs more than once in %q", name, got)
		}
		seen[name] = true
	}
}

func completePaletteObservation() paletteFrameObservation {
	return paletteFrameObservation{
		paletteChecks:            1,
		paletteReuseFailures:     1,
		paletteLengthEqualRefits: 1,
		paletteEqualRefits:       1,
		paletteLUTEqualRefits:    1,
		paletteNoOpRefits:        1,
		paletteChangedEntries:    2,
		paletteUpdates:           1,
		paletteOSCEntries:        16,
		paletteOSCSequences:      3,
		paletteOSCBytes:          384,
		paletteFullRepaints:      1,
		paletteCheckMS:           0.2,
		paletteFitMS:             4,
		paletteOSCMS:             0.3,
	}
}
