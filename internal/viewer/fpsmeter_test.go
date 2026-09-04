package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
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

func TestFPSMeterBarTextRebuildsAtMostOncePerSecond(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	m.tick(base, true)
	first := m.barText(base)
	if !strings.Contains(first, "↑ ") {
		t.Fatalf("fps text missing the output rate: %q", first)
	}
	// A draw 500ms later reuses the cached string.
	m.tick(base.Add(500*time.Millisecond), true)
	if got := m.barText(base.Add(500 * time.Millisecond)); got != first {
		t.Fatalf("fps text rebuilt inside the interval: %q vs %q", got, first)
	}
	// A draw a full second on rebuilds it.
	m.tick(base.Add(1100*time.Millisecond), true)
	if got := m.barText(base.Add(1100 * time.Millisecond)); got == "" {
		t.Fatal("fps text empty after the interval")
	}
}

// TestFPSMeterWarmupGate checks the rate window starts at the first frame after
// main-frame FCP, so the pre-paint load stall is excluded and reported as warm-up.
func TestFPSMeterWarmupGate(t *testing.T) {
	m := &fpsMeter{window: time.Second, start: time.Unix(0, 0)}
	base := time.Unix(0, 0)
	// Two pre-FCP load frames, including a 1.7s stall, precede the first usable frame.
	m.tick(base, false)
	m.tick(base.Add(1700*time.Millisecond), false)
	if m.gated {
		t.Fatal("gate opened before an after-FCP frame")
	}
	if len(m.times) != 0 {
		t.Fatalf("pre-FCP frames entered the live FPS window: %d", len(m.times))
	}
	// First after-FCP frame at 1.8s opens the gate; it is the first counted frame.
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
	m.ingest = fakeIngest{stats: render.IngestStats{
		Frames: 40, MeanFPS: 23.6, MinFPS: 6.8, MaxFPS: 59.4,
		MedianFPS: 50.1, WorstGapMS: 147.4, DecodeMeanMS: 8.2, DecodeMaxMS: 21.0,
		DecodeDrops: 3, DecodeLatestError: `jpeg decode: marker "bad"`,
		AckRTTMeanMS: 3.4, AckRTTMaxMS: 12.6,
	}}
	var b strings.Builder
	m.printSummary(&b)
	out := b.String()
	for _, want := range []string{
		"warmup_ms=1800", "in_frames=40", "in_mean=23.6",
		"in_min=6.8", "in_max=59.4", "in_median=50.1", "in_worst_gap_ms=147.4",
		"in_decode_mean_ms=8.2", "in_decode_max_ms=21.0",
		`in_decode_drops=3`, `in_decode_latest_error="jpeg decode: marker \"bad\""`,
		"out_frames=4", "wall=0.30s", "out_worst_gap_ms=100.0", "out_median=10.0",
		"ack_rtt_mean_ms=3.4", "ack_rtt_max_ms=12.6",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary %q missing %q", out, want)
		}
	}
	for _, field := range []string{"in_decode_drops=", "in_decode_latest_error="} {
		if got := strings.Count(out, field); got != 1 {
			t.Fatalf("summary contains %q %d times, want once: %q", field, got, out)
		}
	}
}

// fakeIngest is a stub ingestReporter for the meter tests: it returns fixed In
// values so the badge and summary shape can be asserted without a real engine. fps
// feeds the live badge; stats feeds the quit summary.
type fakeIngest struct {
	fps   float64
	stats render.IngestStats
}

func (f fakeIngest) IngestFPS(time.Time) float64       { return f.fps }
func (f fakeIngest) IngestSummary() render.IngestStats { return f.stats }

// TestFPSMeterBarTextInOut checks the compact fps text `↓ NN.n │ ↑ NN.n │ ≌ N`
// shows both rates when an ingest reporter is wired, the ↑ output rate alone
// when it is not, and carries no escapes or positioning: the bar owns
// placement and style.
func TestFPSMeterBarTextInOut(t *testing.T) {
	base := time.Unix(0, 0)

	withIn := &fpsMeter{window: time.Second, ingest: fakeIngest{fps: 24}}
	withIn.countReceived(2)
	withIn.tick(base, true)
	withIn.tick(base.Add(100*time.Millisecond), true)
	b := withIn.barText(base.Add(100 * time.Millisecond))
	if !strings.Contains(b, "↓ 24.0 "+barSeparator+" ↑ 10.0") {
		t.Fatalf("fps text %q missing the ↓/↑ rates with a reporter", b)
	}
	if !strings.Contains(b, barSeparator+" ≌ 2") {
		t.Fatalf("fps text %q missing the ≌ cumulative received total", b)
	}
	if strings.ContainsRune(b, '\x1b') {
		t.Fatalf("fps text %q carries an escape; the bar owns placement", b)
	}

	outOnly := &fpsMeter{window: time.Second}
	outOnly.tick(base, true)
	b = outOnly.barText(base)
	if strings.Contains(b, "↓") || !strings.Contains(b, "↑ ") {
		t.Fatalf("fps text %q should be output-only with no reporter", b)
	}
	if !strings.Contains(b, "≌ 0") {
		t.Fatalf("fps text %q missing the cumulative received total without a reporter", b)
	}
}

// TestFPSMeterTextFreePageGatesOnFCP checks that text is irrelevant to the gate.
func TestFPSMeterTextFreePageGatesOnFCP(t *testing.T) {
	m := &fpsMeter{window: time.Second, start: time.Unix(0, 0)}
	base := time.Unix(0, 0)
	// A text-free page can emit frames before FCP; time alone never opens the gate.
	m.tick(base.Add(1*time.Second), false)
	m.tick(base.Add(30*time.Second), false)
	if m.gated {
		t.Fatal("gate opened without FCP on a text-free page")
	}
	// The first frame stamped after FCP opens the gate even though it has no text.
	m.tick(base.Add(30100*time.Millisecond), true)
	m.tick(base.Add(30200*time.Millisecond), true)
	if !m.gated {
		t.Fatal("text-free page did not gate on FCP")
	}
	if m.frames != 2 {
		t.Fatalf("counted frames: got %d, want 2", m.frames)
	}
}

func TestFPSMeterFCPGateIsBackendNeutral(t *testing.T) {
	for _, name := range []string{"block", "kitty", "text-free"} {
		t.Run(name, func(t *testing.T) {
			base := time.Unix(0, 0)
			m := &fpsMeter{window: time.Second, start: base}
			m.tick(base.Add(10*time.Second), false)
			if m.frames != 0 {
				t.Fatalf("%s pre-FCP frame entered FPS stats", name)
			}
			m.tick(base.Add(10100*time.Millisecond), true)
			if m.frames != 1 || m.warmup != 10100*time.Millisecond {
				t.Fatalf("%s gate = frames %d warm-up %v", name, m.frames, m.warmup)
			}
		})
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

// TestFPSMeterRefreshDecaysIdle checks the heartbeat refresh ages the fps text
// out to a live 0 once a burst is older than the peak-hold window, instead of
// freezing it, and counts no frame.
func TestFPSMeterRefreshDecaysIdle(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	// A scroll burst: five draws at 100ms spacing reads 10 fps.
	for i := range 5 {
		m.tick(base.Add(time.Duration(i)*100*time.Millisecond), true)
	}
	if got := m.barText(base.Add(400 * time.Millisecond)); !strings.Contains(got, "↑ 10.0") {
		t.Fatalf("fps text after burst: got %q, want ↑ 10.0", got)
	}
	framesAfterBurst := m.frames
	// Frames stop. A heartbeat past the fpsPeakHold window (last burst sample at
	// 400ms, hold 3s) ages every sample out, so the peak decays to idle ("--") rather
	// than freezing at 10 fps.
	m.refresh(base.Add(3600 * time.Millisecond))
	if got := m.barText(base.Add(3600 * time.Millisecond)); !strings.Contains(got, "↑ --") {
		t.Fatalf("fps text after idle refresh: got %q, want idle ↑ --", got)
	}
	if m.frames != framesAfterBurst {
		t.Fatalf("refresh counted a phantom frame: got %d, want %d", m.frames, framesAfterBurst)
	}
}

// TestFPSMeterPeakHold checks the fps text holds a burst's rate across the hold
// window, so a sub-second scroll burst stays readable, then fades once it ages
// out.
func TestFPSMeterPeakHold(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	// A short burst reads 10 fps, then frames stop.
	for i := range 5 {
		m.tick(base.Add(time.Duration(i)*100*time.Millisecond), true)
	}
	// One second on, well inside the 3s hold, the text still shows the burst peak
	// even though the instantaneous window has decayed.
	m.refresh(base.Add(1400 * time.Millisecond))
	if got := m.barText(base.Add(1400 * time.Millisecond)); !strings.Contains(got, "↑ 10.0") {
		t.Fatalf("fps text inside hold: got %q, want the held ↑ 10.0", got)
	}
	// Past the hold window the peak fades to idle ("--").
	m.refresh(base.Add(3600 * time.Millisecond))
	if got := m.barText(base.Add(3600 * time.Millisecond)); !strings.Contains(got, "↑ --") {
		t.Fatalf("fps text past hold: got %q, want idle ↑ --", got)
	}
}

// TestFPSMeterPaintSplitSummary checks the paint-phase split reaches the summary:
// encode and write means and maxes over the post-gate frames, and that a pre-gate
// (load-period) paint is dropped from the budget.
func TestFPSMeterPaintSplitSummary(t *testing.T) {
	m := &fpsMeter{window: time.Second, start: time.Unix(0, 0)}
	base := time.Unix(0, 0)

	// A text-free load frame before the gate: its paint must not enter the budget.
	m.tick(base, false)
	m.recordPaint(999, 999)

	// Gate opens on the first text frame, then two more steady frames. Encode
	// 10/20/30 -> mean 20, max 30; write 150/160/140 -> mean 150, max 160.
	m.tick(base.Add(100*time.Millisecond), true)
	m.recordPaint(10, 150)
	m.tick(base.Add(200*time.Millisecond), true)
	m.recordPaint(20, 160)
	m.tick(base.Add(300*time.Millisecond), true)
	m.recordPaint(30, 140)

	if m.paintFrames != 3 {
		t.Fatalf("paintFrames = %d, want 3 (pre-gate paint dropped)", m.paintFrames)
	}
	var b strings.Builder
	m.printSummary(&b)
	out := b.String()
	for _, want := range []string{
		"out_encode_mean_ms=20.0", "out_encode_max_ms=30.0",
		"out_write_mean_ms=150.0", "out_write_max_ms=160.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary %q missing %q", out, want)
		}
	}
}

func TestFPSMeterSummaryReportsUnavailableWithoutFCP(t *testing.T) {
	var nilMeter *fpsMeter
	var b strings.Builder
	nilMeter.printSummary(&b)
	if b.Len() != 0 {
		t.Fatalf("nil meter wrote %q", b.String())
	}
	(&fpsMeter{window: time.Second, start: time.Unix(0, 0)}).printSummary(&b)
	if got := b.String(); got != "nyala fps: warmup_ms=unavailable\n" {
		t.Fatalf("summary without FCP = %q", got)
	}
}

// TestFPSMeterSummaryReportsZeroDecodeDrops checks a session with no failures
// keeps explicit zero values and writes only the one quit-summary line.
func TestFPSMeterSummaryReportsZeroDecodeDrops(t *testing.T) {
	m := &fpsMeter{window: time.Second, start: time.Unix(0, 0)}
	m.tick(time.Unix(0, 0), true)

	var b strings.Builder
	m.printSummary(&b)
	out := b.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("summary wrote %d lines, want 1: %q", strings.Count(out, "\n"), out)
	}
	if !strings.Contains(out, `in_decode_drops=0 in_decode_latest_error=""`) {
		t.Fatalf("zero-drop summary = %q", out)
	}
}

func TestFPSMeterSummaryIncludesPaletteMetricsOnOneLine(t *testing.T) {
	base := time.Unix(0, 0)
	m := &fpsMeter{window: time.Second, start: base}
	m.tick(base, true)
	m.palette.record(true, completePaletteObservation(), 600)
	m.tick(base.Add(2*time.Second), true)
	m.palette.record(true, paletteFrameObservation{paletteChecks: 1, paletteCheckMS: 0.4}, 400)

	var b strings.Builder
	m.printSummary(&b)
	out := b.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("summary wrote %d lines, want 1: %q", strings.Count(out, "\n"), out)
	}
	for _, want := range []string{
		"palette_checks=2", "palette_reuse_failures=1", "palette_noop_refits=1",
		"palette_changed_entries=2", "palette_updates=1", "palette_updates_per_s=0.5",
		"palette_osc_entries=16", "palette_osc_sequences=3", "palette_osc_bytes=384",
		"palette_osc_bytes_per_s=192.0", "palette_full_repaints=1",
		"out_bytes=1000", "out_bytes_per_s=500.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q does not contain %q", out, want)
		}
	}
	for _, field := range []string{
		"palette_checks=", "palette_reuse_failures=", "palette_updates=",
		"palette_osc_bytes=", "palette_full_repaints=", "out_bytes=",
	} {
		if got := strings.Count(out, field); got != 1 {
			t.Errorf("summary contains %q %d times, want once: %q", field, got, out)
		}
	}
}

func TestFPSMeterSummaryPaletteRatesAreZeroForTheGateFrame(t *testing.T) {
	base := time.Unix(0, 0)
	m := &fpsMeter{window: time.Second, start: base}
	m.tick(base, true)
	m.palette.record(m.gated, paletteFrameObservation{
		paletteChecks:   1,
		paletteUpdates:  1,
		paletteOSCBytes: 90,
	}, 120)

	var b strings.Builder
	m.printSummary(&b)
	for _, want := range []string{
		"palette_updates_per_s=0.0", "palette_osc_bytes_per_s=0.0", "out_bytes_per_s=0.0",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("zero-wall summary %q does not contain %q", b.String(), want)
		}
	}
}

func TestPrintStreamSummaryReportsDecodeDropsWithoutFPS(t *testing.T) {
	ingest := fakeIngest{stats: render.IngestStats{
		DecodeDrops: 2, DecodeLatestError: "jpeg decode: invalid marker",
	}}
	var b strings.Builder
	printStreamSummary(&b, nil, ingest)

	want := "nyala: warning: frame decode failures: dropped=2 latest_error=\"jpeg decode: invalid marker\"\n"
	if got := b.String(); got != want {
		t.Fatalf("default session summary = %q, want %q", got, want)
	}
}

func TestPrintStreamSummarySilentWithoutDecodeDrops(t *testing.T) {
	var b strings.Builder
	printStreamSummary(&b, nil, fakeIngest{})
	if b.Len() != 0 {
		t.Fatalf("clean default session wrote %q", b.String())
	}
}

func TestPrintStreamSummaryDoesNotDuplicateFPSDecodeDrops(t *testing.T) {
	ingest := fakeIngest{stats: render.IngestStats{
		DecodeDrops: 2, DecodeLatestError: "jpeg decode: invalid marker",
	}}
	m := &fpsMeter{window: time.Second, ingest: ingest}
	var b strings.Builder
	printStreamSummary(&b, m, ingest)
	out := b.String()

	if strings.Count(out, "\n") != 1 {
		t.Fatalf("timed session wrote %d lines, want 1: %q", strings.Count(out, "\n"), out)
	}
	if !strings.HasPrefix(out, "nyala fps: ") {
		t.Fatalf("timed session summary = %q, want fps summary", out)
	}
	if got := strings.Count(out, "in_decode_drops=2"); got != 1 {
		t.Fatalf("timed session reported decode drops %d times, want once: %q", got, out)
	}
	if strings.Contains(out, "nyala: warning:") {
		t.Fatalf("timed session also wrote the default warning: %q", out)
	}
}
