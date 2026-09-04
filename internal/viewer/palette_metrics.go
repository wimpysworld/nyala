package viewer

import (
	"fmt"
	"math"
	"slices"
)

// paletteFrameObservation contains the adaptive palette work for one rendered
// frame. A zero value reports no palette work.
type paletteFrameObservation struct {
	paletteChecks            int64
	paletteReuseFailures     int64
	paletteLengthEqualRefits int64
	paletteEqualRefits       int64
	paletteLUTEqualRefits    int64
	paletteNoOpRefits        int64
	paletteChangedEntries    int64
	paletteUpdates           int64
	paletteOSCEntries        int64
	paletteOSCSequences      int64
	paletteOSCBytes          int64
	paletteFullRepaints      int64

	paletteCheckMS float64
	paletteFitMS   float64
	paletteOSCMS   float64
}

// paletteSessionMetrics contains the post-FCP adaptive palette totals for one
// session. Duration samples stay in observation order until summaryFragment
// sorts them.
type paletteSessionMetrics struct {
	paletteChecks            int64
	paletteReuseFailures     int64
	paletteLengthEqualRefits int64
	paletteEqualRefits       int64
	paletteLUTEqualRefits    int64
	paletteNoOpRefits        int64
	paletteChangedEntries    int64
	paletteUpdates           int64
	paletteOSCEntries        int64
	paletteOSCSequences      int64
	paletteOSCBytes          int64
	paletteFullRepaints      int64
	outBytes                 int64

	paletteCheckSumMS float64
	paletteCheckMaxMS float64
	paletteCheckMS    []float64
	paletteFitSumMS   float64
	paletteFitMaxMS   float64
	paletteFitMS      []float64
	paletteOSCSumMS   float64
	paletteOSCMaxMS   float64
	paletteOSCMS      []float64
}

// record adds one complete frame after the FPS meter opens its FCP gate.
// A pre-gate call changes no totals and allocates no duration storage.
func (m *paletteSessionMetrics) record(gated bool, observation paletteFrameObservation, outBytes int) {
	if !gated {
		return
	}

	m.paletteChecks += observation.paletteChecks
	m.paletteReuseFailures += observation.paletteReuseFailures
	m.paletteLengthEqualRefits += observation.paletteLengthEqualRefits
	m.paletteEqualRefits += observation.paletteEqualRefits
	m.paletteLUTEqualRefits += observation.paletteLUTEqualRefits
	m.paletteNoOpRefits += observation.paletteNoOpRefits
	m.paletteChangedEntries += observation.paletteChangedEntries
	m.paletteUpdates += observation.paletteUpdates
	m.paletteOSCEntries += observation.paletteOSCEntries
	m.paletteOSCSequences += observation.paletteOSCSequences
	m.paletteOSCBytes += observation.paletteOSCBytes
	m.paletteFullRepaints += observation.paletteFullRepaints
	m.outBytes += int64(outBytes)

	if observation.paletteChecks != 0 {
		m.paletteCheckSumMS += observation.paletteCheckMS
		m.paletteCheckMaxMS = max(m.paletteCheckMaxMS, observation.paletteCheckMS)
		m.paletteCheckMS = append(m.paletteCheckMS, observation.paletteCheckMS)
	}
	if observation.paletteReuseFailures != 0 {
		m.paletteFitSumMS += observation.paletteFitMS
		m.paletteFitMaxMS = max(m.paletteFitMaxMS, observation.paletteFitMS)
		m.paletteFitMS = append(m.paletteFitMS, observation.paletteFitMS)
	}
	if observation.paletteUpdates != 0 {
		m.paletteOSCSumMS += observation.paletteOSCMS
		m.paletteOSCMaxMS = max(m.paletteOSCMaxMS, observation.paletteOSCMS)
		m.paletteOSCMS = append(m.paletteOSCMS, observation.paletteOSCMS)
	}
}

// summaryFragment formats the fields that the FPS summary appends. wallSeconds
// is the post-FCP output wall time that the FPS meter reports.
func (m *paletteSessionMetrics) summaryFragment(wallSeconds float64) string {
	updatesPerSecond := ratePerSecond(m.paletteUpdates, wallSeconds)
	oscBytesPerSecond := ratePerSecond(m.paletteOSCBytes, wallSeconds)
	outBytesPerSecond := ratePerSecond(m.outBytes, wallSeconds)
	checkMean, checkP90 := durationSummary(m.paletteCheckSumMS, m.paletteCheckMS)
	fitMean, fitP90 := durationSummary(m.paletteFitSumMS, m.paletteFitMS)
	oscMean, oscP90 := durationSummary(m.paletteOSCSumMS, m.paletteOSCMS)

	return fmt.Sprintf(" palette_checks=%d palette_reuse_failures=%d palette_length_equal_refits=%d palette_equal_refits=%d palette_lut_equal_refits=%d palette_noop_refits=%d palette_changed_entries=%d palette_updates=%d palette_updates_per_s=%.1f palette_osc_entries=%d palette_osc_sequences=%d palette_osc_bytes=%d palette_osc_bytes_per_s=%.1f palette_full_repaints=%d out_bytes=%d out_bytes_per_s=%.1f palette_check_mean_ms=%.3f palette_check_p90_ms=%.3f palette_check_max_ms=%.3f palette_fit_mean_ms=%.3f palette_fit_p90_ms=%.3f palette_fit_max_ms=%.3f palette_osc_mean_ms=%.3f palette_osc_p90_ms=%.3f palette_osc_max_ms=%.3f",
		m.paletteChecks, m.paletteReuseFailures, m.paletteLengthEqualRefits,
		m.paletteEqualRefits, m.paletteLUTEqualRefits, m.paletteNoOpRefits,
		m.paletteChangedEntries, m.paletteUpdates, updatesPerSecond,
		m.paletteOSCEntries, m.paletteOSCSequences, m.paletteOSCBytes,
		oscBytesPerSecond, m.paletteFullRepaints, m.outBytes, outBytesPerSecond,
		checkMean, checkP90, m.paletteCheckMaxMS,
		fitMean, fitP90, m.paletteFitMaxMS,
		oscMean, oscP90, m.paletteOSCMaxMS)
}

func ratePerSecond(total int64, wallSeconds float64) float64 {
	if wallSeconds <= 0 {
		return 0
	}
	return float64(total) / wallSeconds
}

func durationSummary(sum float64, samples []float64) (mean, p90 float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	slices.Sort(samples)
	index := int(math.Ceil(0.9*float64(len(samples)))) - 1
	return sum / float64(len(samples)), samples[index]
}
