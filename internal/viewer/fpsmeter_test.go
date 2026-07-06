package viewer

import (
	"strings"
	"testing"
	"time"
)

func TestNewFPSMeterSwitch(t *testing.T) {
	if newFPSMeter([]string{"FOO=bar"}) != nil {
		t.Fatal("meter armed with the switch unset")
	}
	if newFPSMeter([]string{"NYALA_FPS_TIMING="}) != nil {
		t.Fatal("meter armed with an empty switch value")
	}
	if newFPSMeter([]string{"NYALA_FPS_TIMING=1"}) == nil {
		t.Fatal("meter not armed with the switch set")
	}
	// The fps switch is independent of the load-milestone trace switch.
	if newFPSMeter([]string{"NYALA_DEBUG_TIMING=1"}) != nil {
		t.Fatal("meter armed by NYALA_DEBUG_TIMING")
	}
}

func TestFPSMeterWindowRate(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	// Five draws at 100ms spacing: four intervals over 400ms is 10 fps.
	for i := range 5 {
		m.tick(base.Add(time.Duration(i)*100*time.Millisecond), true)
	}
	if got := m.windowRate(); got < 9.99 || got > 10.01 {
		t.Fatalf("window rate: got %.3f, want 10", got)
	}
}

func TestFPSMeterBadgeRebuildsAtMostOncePerSecond(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	m.tick(base, true)
	first := m.badge(base)
	if !strings.Contains(first, "fps") {
		t.Fatalf("badge missing text: %q", first)
	}
	// A draw 500ms later reuses the cached string.
	m.tick(base.Add(500*time.Millisecond), true)
	if got := m.badge(base.Add(500 * time.Millisecond)); got != first {
		t.Fatalf("badge rebuilt inside the interval: %q vs %q", got, first)
	}
	// A draw a full second on rebuilds it.
	m.tick(base.Add(1100*time.Millisecond), true)
	if got := m.badge(base.Add(1100 * time.Millisecond)); got == "" {
		t.Fatal("badge empty after the interval")
	}
}

// TestFPSMeterWarmupGate checks the rate window starts at the first text-bearing
// frame, so the pre-text load stall is excluded and reported as warm-up.
func TestFPSMeterWarmupGate(t *testing.T) {
	m := &fpsMeter{window: time.Second, start: time.Unix(0, 0)}
	base := time.Unix(0, 0)
	// Two text-free load frames, including a 1.7s stall, precede the first text.
	m.tick(base, false)
	m.tick(base.Add(1700*time.Millisecond), false)
	if m.gated {
		t.Fatal("gate opened before any text frame")
	}
	// First text frame at 1.8s opens the gate; it is the first counted frame.
	m.tick(base.Add(1800*time.Millisecond), true)
	// Steady animation: three more frames at 100ms spacing.
	m.tick(base.Add(1900*time.Millisecond), true)
	m.tick(base.Add(2000*time.Millisecond), true)
	m.tick(base.Add(2100*time.Millisecond), true)

	if m.frames != 4 {
		t.Fatalf("counted frames: got %d, want 4 (load frames excluded)", m.frames)
	}
	if got := m.warmup; got != 1800*time.Millisecond {
		t.Fatalf("warmup: got %v, want 1.8s", got)
	}
	// The 1.7s stall is pre-gate, so the worst gap is the 100ms animation gap.
	if m.maxGap != 100*time.Millisecond {
		t.Fatalf("worst gap: got %v, want 100ms (load stall excluded)", m.maxGap)
	}
	var b strings.Builder
	m.printSummary(&b)
	out := b.String()
	for _, want := range []string{"warmup_ms=1800", "frames=4", "wall=0.30s", "worst_gap_ms=100.0", "median=10.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary %q missing %q", out, want)
		}
	}
}

// TestFPSMeterWarmupFallback checks a text-free page still measures once the
// fallback bound elapses, so the gate never wedges shut.
func TestFPSMeterWarmupFallback(t *testing.T) {
	m := &fpsMeter{window: time.Second, start: time.Unix(0, 0)}
	base := time.Unix(0, 0)
	// A text-free page: frames before the fallback bound are held out of the stats.
	m.tick(base.Add(1*time.Second), false)
	m.tick(base.Add(3*time.Second), false)
	if m.gated {
		t.Fatal("gate opened before the fallback bound on a text-free page")
	}
	// A frame at the fallback bound opens the gate.
	m.tick(base.Add(fpsWarmupFallback), false)
	m.tick(base.Add(fpsWarmupFallback+100*time.Millisecond), false)
	if !m.gated {
		t.Fatal("gate did not open at the fallback bound")
	}
	if m.frames != 2 {
		t.Fatalf("counted frames: got %d, want 2", m.frames)
	}
}

func TestFPSMeterMedian(t *testing.T) {
	// Four gaps [100,500,100,100]ms. Sorted [100,100,100,500], the two middle gaps
	// are both 100ms, so the median gap is 100ms -> 10 fps. The median ignores the
	// one 500ms stall a mean would drag down.
	gaps := []time.Duration{
		100 * time.Millisecond,
		500 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
	}
	if got := medianFPS(gaps); got < 9.99 || got > 10.01 {
		t.Fatalf("median fps: got %.3f, want 10", got)
	}
	// Odd count picks the middle gap: [100,200,300] -> 200ms -> 5 fps.
	odd := []time.Duration{200 * time.Millisecond, 100 * time.Millisecond, 300 * time.Millisecond}
	if got := medianFPS(odd); got < 4.99 || got > 5.01 {
		t.Fatalf("median fps odd: got %.3f, want 5", got)
	}
	if got := medianFPS(nil); got != 0 {
		t.Fatalf("median of no gaps: got %.3f, want 0", got)
	}
}

func TestFPSMeterSummaryNilAndEmptyAreSilent(t *testing.T) {
	var nilMeter *fpsMeter
	var b strings.Builder
	nilMeter.printSummary(&b)
	(&fpsMeter{window: time.Second, start: time.Unix(0, 0)}).printSummary(&b)
	if b.Len() != 0 {
		t.Fatalf("expected silence, got %q", b.String())
	}
}
