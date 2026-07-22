package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
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
		AckRTTMeanMS: 3.4, AckRTTMaxMS: 12.6,
	}}
	var b strings.Builder
	m.printSummary(&b)
	out := b.String()
	for _, want := range []string{
		"warmup_ms=1800", "in_frames=40", "in_mean=23.6",
		"in_min=6.8", "in_max=59.4", "in_median=50.1", "in_worst_gap_ms=147.4",
		"in_decode_mean_ms=8.2", "in_decode_max_ms=21.0",
		"out_frames=4", "wall=0.30s", "out_worst_gap_ms=100.0", "out_median=10.0",
		"ack_rtt_mean_ms=3.4", "ack_rtt_max_ms=12.6",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary %q missing %q", out, want)
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

// TestFPSMeterBadgeInOut checks the badge shows both streams when an ingest reporter
// is wired, and Out only when it is not.
func TestFPSMeterBadgeInOut(t *testing.T) {
	base := time.Unix(0, 0)

	withIn := &fpsMeter{window: time.Second, ingest: fakeIngest{fps: 24}}
	withIn.countReceived(2)
	withIn.tick(base, true)
	withIn.tick(base.Add(100*time.Millisecond), true)
	b := withIn.badge(base.Add(100 * time.Millisecond))
	if !strings.Contains(b, "In: 24.0") || !strings.Contains(b, "Out:") {
		t.Fatalf("badge %q missing In/Out with a reporter", b)
	}
	if !strings.Contains(b, "fps (2)") {
		t.Fatalf("badge %q missing the cumulative received total", b)
	}

	outOnly := &fpsMeter{window: time.Second}
	outOnly.tick(base, true)
	b = outOnly.badge(base)
	if strings.Contains(b, "In:") || !strings.Contains(b, "Out:") {
		t.Fatalf("badge %q should be Out-only with no reporter", b)
	}
	if !strings.Contains(b, "fps (0)") {
		t.Fatalf("badge %q missing the cumulative received total without a reporter", b)
	}
}

func TestFPSMeterKittyBadgeUsesAbsoluteHome(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	m.tick(base, true)

	badge := m.badgeAtHome(base)
	if strings.Contains(badge, "\x1b7") || strings.Contains(badge, "\x1b8") {
		t.Fatalf("Kitty badge %q consumed a saved-cursor slot", badge)
	}
	if strings.Count(badge, "\x1b[1;1H") != 2 || !strings.HasSuffix(badge, "\x1b[0m\x1b[1;1H") {
		t.Fatalf("Kitty badge %q did not use absolute home at both ends", badge)
	}

	heartbeat := m.refreshAtHome(base.Add(2 * time.Second))
	if strings.Contains(heartbeat, "\x1b7") || strings.Contains(heartbeat, "\x1b8") {
		t.Fatalf("Kitty heartbeat %q consumed a saved-cursor slot", heartbeat)
	}
	if strings.Count(heartbeat, "\x1b[1;1H") != 2 || !strings.HasSuffix(heartbeat, "\x1b[0m\x1b[1;1H") {
		t.Fatalf("Kitty heartbeat %q did not return to absolute home", heartbeat)
	}
}

// TestFPSMeterShortBadgeClearsPreviousWidth decodes a long badge followed by an
// idle badge through a VT. Every cell in the old footprint must carry the new
// badge or a reverse-video space, with no old glyph or style left at the tail.
func TestFPSMeterShortBadgeClearsPreviousWidth(t *testing.T) {
	const cols = 48
	base := time.Unix(0, 0)
	m := &fpsMeter{window: time.Second, ingest: fakeIngest{fps: 123.4}}
	m.tick(base, true)
	m.tick(base.Add(100*time.Millisecond), true)

	terminal := vt.NewEmulator(cols, 1)
	if _, err := terminal.Write([]byte(m.badge(base.Add(100 * time.Millisecond)))); err != nil {
		t.Fatalf("write long badge: %v", err)
	}
	longWidth := m.badgeWidth

	m.ingest = fakeIngest{}
	shortBody := " In: -- / Out: -- fps (0) "
	if longWidth <= len(shortBody) {
		t.Fatalf("long badge width = %d, want more than short width %d", longWidth, len(shortBody))
	}
	if _, err := terminal.Write([]byte(m.refresh(base.Add(4 * time.Second)))); err != nil {
		t.Fatalf("write short badge: %v", err)
	}

	wantText := shortBody + strings.Repeat(" ", longWidth-len(shortBody))
	wantStyle := terminal.CellAt(0, 0).Style
	for col, want := range []byte(wantText) {
		cell := terminal.CellAt(col, 0)
		if cell.Content != string(want) {
			t.Errorf("cell %d content = %q, want %q", col, cell.Content, string(want))
		}
		if !cell.Style.Equal(&wantStyle) {
			t.Errorf("cell %d style = %+v, want %+v", col, cell.Style, wantStyle)
		}
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

// TestFPSMeterRefreshDecaysIdle checks the heartbeat refresh ages the badge out to a
// live 0 once a burst is older than the peak-hold window, instead of freezing it,
// and counts no frame.
func TestFPSMeterRefreshDecaysIdle(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	// A scroll burst: five draws at 100ms spacing reads 10 fps.
	for i := range 5 {
		m.tick(base.Add(time.Duration(i)*100*time.Millisecond), true)
	}
	if got := m.badge(base.Add(400 * time.Millisecond)); !strings.Contains(got, "10.0 fps") {
		t.Fatalf("badge after burst: got %q, want 10.0 fps", got)
	}
	framesAfterBurst := m.frames
	// Frames stop. A heartbeat past the fpsPeakHold window (last burst sample at
	// 400ms, hold 3s) ages every sample out, so the peak decays to idle ("--") rather
	// than freezing at 10 fps.
	if got := m.refresh(base.Add(3600 * time.Millisecond)); !strings.Contains(got, "Out: -- fps") {
		t.Fatalf("badge after idle refresh: got %q, want idle Out: -- fps", got)
	}
	if m.frames != framesAfterBurst {
		t.Fatalf("refresh counted a phantom frame: got %d, want %d", m.frames, framesAfterBurst)
	}
}

// TestFPSMeterPeakHold checks the badge holds a burst's rate across the hold window,
// so a sub-second scroll burst stays readable, then fades once it ages out.
func TestFPSMeterPeakHold(t *testing.T) {
	m := &fpsMeter{window: time.Second}
	base := time.Unix(0, 0)
	// A short burst reads 10 fps, then frames stop.
	for i := range 5 {
		m.tick(base.Add(time.Duration(i)*100*time.Millisecond), true)
	}
	// One second on, well inside the 3s hold, the badge still shows the burst peak
	// even though the instantaneous window has decayed.
	if got := m.refresh(base.Add(1400 * time.Millisecond)); !strings.Contains(got, "10.0 fps") {
		t.Fatalf("badge inside hold: got %q, want the held 10.0 fps", got)
	}
	// Past the hold window the peak fades to idle ("--").
	if got := m.refresh(base.Add(3600 * time.Millisecond)); !strings.Contains(got, "Out: -- fps") {
		t.Fatalf("badge past hold: got %q, want idle Out: -- fps", got)
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
