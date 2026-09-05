package viewer

import "time"

const (
	// Sampling one write in eight keeps the steady frame path close to one branch
	// while still checking terminal pressure several times each second.
	writePressureSampleStride = 8
	// Four discarded samples keep the observer out of startup work. Four later
	// consecutive slow samples require about one second of pressure at 60 fps.
	writePressureStartupSamples   = 4
	writePressureSustainedSamples = 4
	// Fifteen milliseconds is 90% of the 16.67 ms frame budget. It separates the
	// measured 16.7 ms True Colour writes from the 7.2 ms adaptive writes.
	writePressureThreshold = 15 * time.Millisecond
)

// writePressureObserver samples eligible completed writes and reports sustained
// pressure once. Its zero value is ready to use.
type writePressureObserver struct {
	eligibleWrites uint64
	sampledWrites  uint64
	slowSamples    uint8
	reported       bool
}

// writePressureEligible reports whether this session can degrade an automatic
// True Colour Block view to the adaptive 240-colour mode.
func (s *streamSession) writePressureEligible() bool {
	return s.st != nil && s.st.automaticColourFallback &&
		s.st.Backend == BackendBlock && s.st.Colour == ColourTrue && s.st.paletteQueryOK &&
		!s.st.staticPaletteSelected
}

// nextWritePressureSample selects a completed frame write for timing and tags
// it with the current reset generation.
func (s *streamSession) nextWritePressureSample() (bool, uint64) {
	sample := s.writePressure.sample(s.writePressureEligible())
	return sample, s.writePressureGen
}

// resetWritePressure discards samples from the previous render lifecycle.
func (s *streamSession) resetWritePressure() {
	s.writePressure = writePressureObserver{}
	s.writePressureGen++
}

// observeWritePressure applies the one-way automatic fallback after a current,
// eligible sample reports sustained terminal write pressure.
func (s *streamSession) observeWritePressure(item freeItem) {
	if !item.pressureSample || item.pressureGen != s.writePressureGen ||
		!s.writePressureEligible() || !s.writePressure.observe(item.writeDur) {
		return
	}
	s.st.Colour = ColourXTerm256
	s.recomposeBlock(true)
}

// sample reports whether the next eligible write needs timing.
func (o *writePressureObserver) sample(eligible bool) bool {
	if !eligible || o.reported {
		return false
	}
	o.eligibleWrites++
	return o.eligibleWrites%writePressureSampleStride == 0
}

// observe records one sampled completed write and reports the first sustained
// pressure verdict. Startup samples do not affect the sustained run.
func (o *writePressureObserver) observe(d time.Duration) bool {
	if o.reported {
		return false
	}
	o.sampledWrites++
	if o.sampledWrites <= writePressureStartupSamples {
		return false
	}
	if d < writePressureThreshold {
		o.slowSamples = 0
		return false
	}
	o.slowSamples++
	if o.slowSamples < writePressureSustainedSamples {
		return false
	}
	o.reported = true
	return true
}
