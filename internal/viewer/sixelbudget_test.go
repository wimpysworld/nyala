package viewer

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// noisyImage builds a w x h opaque RGBA image of seeded random noise, the
// worst case for the Sixel encoder: no runs, no flat regions, so the encoded
// frame is as large as the geometry allows and the byte-budget escalation has
// real work to do. The fixed seed keeps the measured sizes stable run to run.
func noisyImage(w, h int) *image.RGBA {
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic test fixture
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256)) //nolint:gosec // bounded by Intn(256)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff
	}
	return img
}

// tmuxSixelState builds a Sixel viewer state under tmux with the default
// byte budget armed, sized so the engine grid extent is 640x464 (80x30 cells
// at the 8x16 default with the bottom row reserved).
func tmuxSixelState() State {
	st := NewState(80, 30)
	st.mux = muxTmux
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel
	st.sixelBudget.limit = sixelBudgetDefault
	return st
}

// TestSixelBudgetDownscalesNoisyFrameUnderBudget drives the byte-budget loop
// with a seeded noisy frame whose full-quality encode exceeds the tmux budget
// (measured 1479170 bytes at factor 1 and 925905 at 1.5). The loop must
// escalate until the frame fits, keep the decoded raster at the full grid
// extent (a smaller raster would shrink the on-screen page), and remember the
// working factor so the next frame encodes once without re-escalating.
func TestSixelBudgetDownscalesNoisyFrameUnderBudget(t *testing.T) {
	st := tmuxSixelState()
	gridW, gridH := 640, 464
	img := noisyImage(gridW, gridH)

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(tmux sixel) = %v", err)
	}
	if out.Len() > sixelBudgetDefault {
		t.Fatalf("budgeted frame is %d bytes, want <= %d", out.Len(), sixelBudgetDefault)
	}
	if out.Len() == 0 {
		t.Fatal("budgeted frame wrote no bytes")
	}
	decoded, err := renderdecode.DecodeSixel(out.Bytes())
	if err != nil {
		t.Fatalf("DecodeSixel: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != gridW || b.Dy() != gridH {
		t.Fatalf("budgeted raster = %dx%d, want the unchanged %dx%d grid extent", b.Dx(), b.Dy(), gridW, gridH)
	}
	if st.sixelBudget.idx == 0 {
		t.Fatal("budget loop did not escalate the downscale factor on an over-budget frame")
	}

	// The working factor is remembered: a second frame stays at the settled
	// ladder index (one encode, no re-escalation from full quality).
	settled := st.sixelBudget.idx
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(second frame) = %v", err)
	}
	if st.sixelBudget.idx != settled {
		t.Fatalf("second frame moved the ladder index to %d, want the remembered %d", st.sixelBudget.idx, settled)
	}
	if out.Len() > sixelBudgetDefault {
		t.Fatalf("second budgeted frame is %d bytes, want <= %d", out.Len(), sixelBudgetDefault)
	}
}

// TestSixelBudgetProbesWeakerPeriodically pins the quality-recovery probe:
// once sixelProbeInterval budgeted frames pass, the loop retries one ladder
// step weaker, so calmed content climbs back toward full quality instead of
// staying blurred forever.
func TestSixelBudgetProbesWeakerPeriodically(t *testing.T) {
	st := tmuxSixelState()
	// A flat frame fits at any factor. Start one frame before a probe from
	// the most blurred motion factor.
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st.sixelBudget.idx = sixelMotionMaxIdx
	st.sixelBudget.sinceProbe = sixelProbeInterval - 1

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if got, want := st.sixelBudget.idx, sixelMotionMaxIdx-1; got != want {
		t.Fatalf("probe frame ladder index = %d, want the one-step-weaker %d", got, want)
	}
}

func TestSixelBudgetHardLimitBlurOnlyAffectsOversizedFrame(t *testing.T) {
	const width, height = 640, 464
	const limit = 100_000
	heavy := render.Frame{Img: noisyImage(width, height)}
	motionOnly := tmuxSixelState()
	motionOnly.sixelBudget.limit = limit
	var out bytes.Buffer
	if err := motionOnly.RenderStreamFrame(heavy, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() <= limit {
		t.Fatalf("test frame fits in %d bytes at the motion limit, want more than %d", out.Len(), limit)
	}

	st := tmuxSixelState()
	b := &st.sixelBudget
	b.limit, b.hardLimit = limit, limit
	// Start at the motion ceiling to isolate the hard-limit retry from
	// motion escalations, which reset the probe cadence.
	b.idx = sixelMotionMaxIdx
	b.sinceProbe = 2
	out.Reset()
	if err := st.RenderStreamFrame(heavy, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 || out.Len() > limit {
		t.Fatalf("oversized frame wrote %d bytes, want a non-empty frame within %d bytes", out.Len(), limit)
	}
	if b.idx != sixelMotionMaxIdx || b.sinceProbe != 3 || b.escalationSkips != 0 {
		t.Fatalf("hard-limit retry changed motion index, probe cadence or sample skips: %d, %d, %d", b.idx, b.sinceProbe, b.escalationSkips)
	}
	if !st.sixelSettleNeeded() {
		t.Fatal("blurred frame did not arm a settle")
	}

	// Motion factor 4 preserves stripes four pixels wide. The extra blur
	// that fits the oversized frame would blend adjacent stripes.
	stripes := image.NewRGBA(image.Rect(0, 0, width, height))
	colours := [...]color.RGBA{{R: 255, A: 255}, {B: 255, A: 255}}
	for y := range height {
		for x := range width {
			stripes.SetRGBA(x, y, colours[(x/4)%len(colours)])
		}
	}
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: stripes}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() > limit {
		t.Fatalf("ordinary frame wrote %d bytes, want at most %d", out.Len(), limit)
	}
	decoded, err := renderdecode.DecodeSixel(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Bounds(); got.Dx() != width || got.Dy() != height {
		t.Fatalf("ordinary frame bounds = %v, want %dx%d", got, width, height)
	}
	leftRed, _, leftBlue, _ := decoded.At(1, 0).RGBA()
	rightRed, _, rightBlue, _ := decoded.At(5, 0).RGBA()
	if leftRed <= leftBlue || rightBlue <= rightRed {
		t.Fatal("ordinary frame lost stripe detail after the oversized frame")
	}
}

// TestSixelBudgetEscalatesOnConsecutiveSlowWrites pins the latency-driven
// escalation: a write whose effective pacing interval (duration plus margin)
// reaches the pacing floor counts as slow (write >= 200 ms at the 250/50
// values), two consecutive slow writes escalate the ladder exactly one step,
// a single slow write does not, and a sub-threshold write resets the streak.
// The slow duration mirrors the live measurement: tmux 3.6a held ~215 ms
// writes indefinitely, at exact capacity with zero drain slack, while a
// 150 ms write (200 ms effective, under the floor) leaves tmux idle time.
func TestSixelBudgetEscalatesOnConsecutiveSlowWrites(t *testing.T) {
	const slow = 215 * time.Millisecond
	const fast = 150 * time.Millisecond
	st := tmuxSixelState()
	b := &st.sixelBudget

	// Sub-floor writes never escalate: full quality is preserved.
	for range 4 {
		b.recordWrite(fast)
	}
	if b.idx != 0 {
		t.Fatalf("fast writes moved the ladder index to %d, want untouched 0", b.idx)
	}

	// One slow write is not enough.
	b.recordWrite(slow)
	if b.idx != 0 {
		t.Fatalf("a single slow write moved the ladder index to %d, want 0", b.idx)
	}

	// A fast write in between resets the streak.
	b.recordWrite(fast)
	b.recordWrite(slow)
	if b.idx != 0 {
		t.Fatalf("an interrupted slow streak moved the ladder index to %d, want 0", b.idx)
	}

	// The second consecutive slow write escalates exactly one step.
	b.recordWrite(slow)
	if b.idx != 1 {
		t.Fatalf("two consecutive slow writes set the ladder index to %d, want 1", b.idx)
	}

	// The post-escalation sample skip holds the ladder: the very next slow
	// write is backlog encoded at the old factor and does not step again.
	b.recordWrite(slow)
	if b.idx != 1 {
		t.Fatalf("the first slow write after an escalation set the ladder index to %d, want 1", b.idx)
	}
}

// TestSixelBudgetEscalationSkipsBacklogSamples pins the anti-chaining rule:
// the first sixelSlowWriteStreak write samples after an escalation measure
// backlog encoded at the pre-escalation factor (live tmux 3.6a chained
// 1.5 -> 2 -> 3 -> 4 within four frames on them), so they are skipped, and
// only a fresh slow streak measured at the new factor escalates again.
func TestSixelBudgetEscalationSkipsBacklogSamples(t *testing.T) {
	const slow = 215 * time.Millisecond
	st := tmuxSixelState()
	b := &st.sixelBudget

	b.recordWrite(slow)
	b.recordWrite(slow)
	if b.idx != 1 {
		t.Fatalf("two slow writes set the ladder index to %d, want 1", b.idx)
	}

	// The in-flight backlog samples are skipped: no second escalation.
	for range sixelSlowWriteStreak {
		b.recordWrite(slow)
	}
	if b.idx != 1 {
		t.Fatalf("backlog samples chained the ladder index to %d, want the held 1", b.idx)
	}

	// A fresh streak at the new factor still needs the full streak length.
	b.recordWrite(slow)
	if b.idx != 1 {
		t.Fatalf("one post-skip slow write set the ladder index to %d, want 1", b.idx)
	}
	b.recordWrite(slow)
	if b.idx != 2 {
		t.Fatalf("a fresh slow streak at the new factor set the ladder index to %d, want 2", b.idx)
	}
}

// TestSixelBudgetFailedProbeDisablesProbingForEpisode pins the episode
// semantics: one band-breached sharper trial silences all further probes
// until the next settle emit. Even with the cool-down spent, tiny writes
// refilling the stability window, and a full cadence accrued, no second
// probe fires: the ladder watched that factor fail, so a timed retry during
// sustained motion buys one saturation ramp per attempt (live tmux 3.6a at
// factor 2 breathed quality on a ~15 s period under retried probes).
func TestSixelBudgetFailedProbeDisablesProbingForEpisode(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.idx = 2
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	b.sinceProbe = sixelProbeInterval - 1

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if b.idx != 1 || !b.probePending {
		t.Fatalf("probe did not fire: ladder index = %d, pending = %v", b.idx, b.probePending)
	}

	// One band breach fails the trial and exhausts the episode's probes.
	b.recordWrite(160 * time.Millisecond)
	if b.idx != 2 {
		t.Fatalf("band breach left the ladder index at %d, want the pre-probe 2", b.idx)
	}
	if !b.probesExhausted {
		t.Fatal("band breach did not exhaust the episode's probes")
	}

	// Ideal recovery conditions: cool-down spent, stable tiny writes, a
	// full cadence accrued. The probe must stay silent for the episode.
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	b.probeCooldown = 0
	b.sinceProbe = sixelProbeInterval - 1
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(post-failure frame) = %v", err)
	}
	if b.idx != 2 {
		t.Fatalf("probe fired in an exhausted episode: ladder index = %d, want the held 2", b.idx)
	}
}

// TestSixelSettleReenablesProbingAndResumesAtSustainedFactor pins the
// episode boundary: the settle emit ends an exhausted episode at factor 1,
// the first budgeted frame of the next episode resumes at the remembered
// pre-settle ladder index behind the base cool-down (live tmux 3.6a resumed
// at factor 1 and re-ran the whole 1 -> 1.5 -> 2 -> 3 discovery cascade
// with 400-535 ms writes after every settle), and probing is freshly
// enabled, so one probe fires in the new episode.
func TestSixelSettleReenablesProbingAndResumesAtSustainedFactor(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.idx = 2

	// A budgeted frame retains the settle source; the episode then fails
	// its probe.
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(budgeted frame) = %v", err)
	}
	b.probesExhausted = true

	out.Reset()
	if err := st.renderSixelSettle(&out); err != nil {
		t.Fatalf("renderSixelSettle = %v", err)
	}
	if b.idx != 0 {
		t.Fatalf("settle emitted at ladder index %d, want the sharp 0", b.idx)
	}
	if b.resumeIdx != 2 {
		t.Fatalf("settle remembered resume index %d, want the pre-settle 2", b.resumeIdx)
	}
	if b.probesExhausted {
		t.Fatal("settle left the episode's probes exhausted")
	}

	// The first budgeted frame of the new episode resumes at the sustained
	// factor with the base cool-down armed (one frame already spent).
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(resume frame) = %v", err)
	}
	if b.idx != 2 {
		t.Fatalf("resume frame ladder index = %d, want the remembered 2", b.idx)
	}
	if b.resumeIdx != 0 {
		t.Fatalf("resume frame left resumeIdx at %d, want the consumed 0", b.resumeIdx)
	}
	if b.probeCooldown != sixelProbeCooldown-1 {
		t.Fatalf("resume frame left the cool-down at %d, want the base %d minus the spent frame", b.probeCooldown, sixelProbeCooldown)
	}

	// Probing is fresh: once the cool-down is spent and the cadence
	// accrues on stable tiny writes, a probe fires in the new episode.
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	b.probeCooldown = 0
	b.sinceProbe = sixelProbeInterval - 1
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(new-episode probe frame) = %v", err)
	}
	if b.idx != 1 {
		t.Fatalf("probe did not fire in the new episode: ladder index = %d, want 1", b.idx)
	}
}

// TestSixelBudgetProbeRetreatsOnBandBreach pins the band-based trial: while
// a sharper probe is pending, one recorded write crossing the 150 ms probe
// band (well under the 200 ms escalation threshold, no streak) fails the
// trial at once, retreats the ladder straight to the pre-probe factor, and
// silences probing for the rest of the episode. Live tmux 3.6a evidence:
// tmux's buffers absorb a failing sharper factor, so writes ramped 20 ->
// 107 ms across the cadence without ever reaching the escalation threshold,
// the trial was judged survived, and the next, sharper probe tipped the
// accumulated queue and took the blame.
func TestSixelBudgetProbeRetreatsOnBandBreach(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.idx = 2
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	b.sinceProbe = sixelProbeInterval - 1

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if b.idx != 1 || !b.probePending {
		t.Fatalf("probe did not fire: ladder index = %d, pending = %v", b.idx, b.probePending)
	}

	b.recordWrite(160 * time.Millisecond)
	if b.idx != 2 {
		t.Fatalf("band breach left the ladder index at %d, want the pre-probe 2", b.idx)
	}
	if b.probePending {
		t.Fatal("band breach left the probe pending")
	}
	if !b.probesExhausted {
		t.Fatal("band breach did not exhaust the episode's probes")
	}
	if b.escalationSkips != sixelSlowWriteStreak {
		t.Fatalf("band breach armed %d sample skips, want %d for the sharper-factor backlog", b.escalationSkips, sixelSlowWriteStreak)
	}
}

// TestSixelBudgetProbeStabilityGate pins the pre-fire stability gate: a
// monotonic write ramp inside the band (69, 83, 90 ms, tmux buffers filling
// on content the pipe cannot sustain) blocks the sharper probe, while
// jittering flat writes (18, 20, 19 ms) let it fire.
func TestSixelBudgetProbeStabilityGate(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.idx = 1

	for _, d := range []time.Duration{69 * time.Millisecond, 83 * time.Millisecond, 90 * time.Millisecond} {
		b.recordWrite(d)
	}
	b.sinceProbe = sixelProbeInterval - 1
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(rising ramp) = %v", err)
	}
	if b.idx != 1 {
		t.Fatalf("probe fired on a rising ramp: ladder index = %d, want the held 1", b.idx)
	}

	for _, d := range []time.Duration{18 * time.Millisecond, 20 * time.Millisecond, 19 * time.Millisecond} {
		b.recordWrite(d)
	}
	b.sinceProbe = sixelProbeInterval - 1
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(flat jitter) = %v", err)
	}
	if b.idx != 0 {
		t.Fatalf("probe did not fire on flat jitter: ladder index = %d, want 0", b.idx)
	}
}

// TestSixelBudgetSurvivingProbeKeepsProbingEnabled pins the trial's success
// side: a probe whose writes stay flat inside the probe band survives
// exactly at the sixelProbeInterval cadence (the flat window clears the
// stability check), probing stays enabled within the episode, and the next
// sharper step fires on the accrued cadence.
func TestSixelBudgetSurvivingProbeKeepsProbingEnabled(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.idx = 2
	b.sinceProbe = sixelProbeInterval - 1
	b.recordWrite(30 * time.Millisecond)

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if b.idx != 1 || !b.probePending {
		t.Fatalf("probe did not fire: ladder index = %d, pending = %v", b.idx, b.probePending)
	}

	// A full cadence of quiet frames, every recorded write inside the band,
	// marks the probe survived; the last frame of the cadence then probes
	// the next sharper step in the same episode.
	for i := range sixelProbeInterval {
		b.recordWrite(30 * time.Millisecond)
		out.Reset()
		if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
			t.Fatalf("RenderStreamFrame(quiet frame %d) = %v", i, err)
		}
	}
	if b.probesExhausted {
		t.Fatal("surviving probe exhausted the episode's probes")
	}
	if b.idx != 0 {
		t.Fatalf("next sharper probe did not fire in the same episode: ladder index = %d, want 0", b.idx)
	}
}

// TestSixelBudgetRampingTrialStaysPendingThenRetreats pins the live tmux
// 3.6a failure the stability requirement fixes: at the 300 ms legacy floor
// (180 ms band) a sharper probe's writes ramp 30 -> 146 ms through the
// ten-frame cadence, all inside the band, so a cadence-only rule would
// declare survival while tmux's buffer absorption still hides the
// saturation. The rising window must keep the trial pending past the
// cadence, and the 194 ms write must convict it the moment it crosses the
// band: the ladder retreats to the pre-probe factor and the episode's
// probes are exhausted.
func TestSixelBudgetRampingTrialStaysPendingThenRetreats(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.minInterval = tmuxSixelMinIntervalLegacy
	b.idx = 2
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	b.sinceProbe = sixelProbeInterval - 1

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if b.idx != 1 || !b.probePending {
		t.Fatalf("probe did not fire: ladder index = %d, pending = %v", b.idx, b.probePending)
	}

	// The observed ramp: every write inside the 180 ms band, rising through
	// the cadence and beyond it.
	ramp := []time.Duration{30, 50, 58, 77, 86, 92, 112, 125, 146, 161, 177}
	for i, d := range ramp {
		b.recordWrite(d * time.Millisecond)
		out.Reset()
		if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
			t.Fatalf("RenderStreamFrame(ramp frame %d) = %v", i, err)
		}
	}
	if b.sinceProbe < sixelProbeInterval {
		t.Fatalf("ramp accrued only %d frames, want the cadence spent", b.sinceProbe)
	}
	if !b.probePending {
		t.Fatal("rising ramp let the trial survive at the cadence")
	}
	if b.idx != 1 {
		t.Fatalf("pending ramp trial moved the ladder index to %d, want the held 1", b.idx)
	}

	// The ramp crosses the band: immediate retreat, episode probes spent.
	b.recordWrite(194 * time.Millisecond)
	if b.idx != 2 {
		t.Fatalf("band breach left the ladder index at %d, want the pre-probe 2", b.idx)
	}
	if b.probePending {
		t.Fatal("band breach left the trial pending")
	}
	if !b.probesExhausted {
		t.Fatal("band breach did not exhaust the episode's probes")
	}
}

// TestSixelBudgetTrialStabilisingAfterCadenceSurvives pins the pending
// trial's late-survival side: writes rising under the band through the
// cadence (60, 90, 120 ms) keep the trial pending past the cadence, and the
// first flat-or-falling window after it (118 ms turns the trend) declares
// survival with probing still enabled.
func TestSixelBudgetTrialStabilisingAfterCadenceSurvives(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget
	b.idx = 1
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	b.sinceProbe = sixelProbeInterval - 1

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if b.idx != 0 || !b.probePending {
		t.Fatalf("probe did not fire: ladder index = %d, pending = %v", b.idx, b.probePending)
	}

	// A ramp under the band through the cadence keeps the trial pending.
	for i, d := range []time.Duration{60, 90, 120} {
		b.recordWrite(d * time.Millisecond)
		out.Reset()
		if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
			t.Fatalf("RenderStreamFrame(rising frame %d) = %v", i, err)
		}
	}
	for b.sinceProbe < sixelProbeInterval {
		out.Reset()
		if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
			t.Fatalf("RenderStreamFrame(cadence frame) = %v", err)
		}
	}
	if !b.probePending {
		t.Fatal("rising ramp let the trial survive at the cadence")
	}

	// The ramp flattens: the first stable window after the cadence survives
	// the trial without exhausting the episode's probes.
	b.recordWrite(118 * time.Millisecond)
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(stabilised frame) = %v", err)
	}
	if b.probePending {
		t.Fatal("stabilised window left the trial pending")
	}
	if b.probesExhausted {
		t.Fatal("late survival exhausted the episode's probes")
	}
	if b.idx != 0 {
		t.Fatalf("surviving trial moved the ladder index to %d, want the held 0", b.idx)
	}
}

// TestSixelSettleResetsProbeState pins the settle reset: the one-shot settle
// emit clears the exhausted-probe flag, the pending trial, and the
// post-escalation sample skip along with the rest of the ladder state, and
// remembers the pre-settle ladder index for the next episode's resume.
func TestSixelSettleResetsProbeState(t *testing.T) {
	st := tmuxSixelState()
	b := &st.sixelBudget

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: noisyImage(640, 464)}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(over-budget frame) = %v", err)
	}
	if !st.sixelSettleNeeded() {
		t.Fatal("over-budget frame did not arm the settle emit")
	}
	b.recordWrite(30 * time.Millisecond)
	b.probesExhausted = true
	b.probePending = true
	b.escalationSkips = 1
	b.preProbeIdx = 2
	sustained := b.idx

	out.Reset()
	if err := st.renderSixelSettle(&out); err != nil {
		t.Fatalf("renderSixelSettle = %v", err)
	}
	if b.probesExhausted || b.probePending || b.escalationSkips != 0 {
		t.Fatalf("settle left exhausted = %v, pending = %v, skips = %d, want all cleared", b.probesExhausted, b.probePending, b.escalationSkips)
	}
	if b.preProbeIdx != 0 || len(b.recentWrites) != 0 {
		t.Fatalf("settle left preProbeIdx = %d, window = %v, want both cleared", b.preProbeIdx, b.recentWrites)
	}
	if b.resumeIdx != sustained {
		t.Fatalf("settle remembered resume index %d, want the pre-settle %d", b.resumeIdx, sustained)
	}
}

// TestSixelBudgetProbeHysteresis pins the recovery band: the periodic weaker
// probe fires only when the last recorded write duration sits under 60
// percent of the pacing floor, so a write between the probe band and the
// escalation threshold (write >= 200 ms) holds the ladder in place instead
// of oscillating between adjacent factors.
func TestSixelBudgetProbeHysteresis(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}

	// A write between the 60 percent band (150 ms) and the escalation
	// threshold blocks the probe.
	st := tmuxSixelState()
	st.sixelBudget.idx = 1
	st.sixelBudget.sinceProbe = sixelProbeInterval - 1
	st.sixelBudget.recordWrite(170 * time.Millisecond)
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(held probe) = %v", err)
	}
	if st.sixelBudget.idx != 1 {
		t.Fatalf("probe fired at a 170ms write: ladder index = %d, want the held 1", st.sixelBudget.idx)
	}

	// Writes comfortably under the band, jitter-flat, let the probe fire
	// once the 170ms sample has left the stability window.
	for range sixelProbeStabilityWindow {
		st.sixelBudget.recordWrite(100 * time.Millisecond)
	}
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(released probe) = %v", err)
	}
	if st.sixelBudget.idx != 0 {
		t.Fatalf("probe did not fire at a 100ms write: ladder index = %d, want 0", st.sixelBudget.idx)
	}
}

// TestSixelBudgetProbeCooldownAfterLatencyEscalation pins the post-escalation
// cool-down: once measured latency steps the ladder, the pipe drains and the
// next writes are tiny, so without the cool-down the probe would re-sharpen
// within sixelProbeInterval frames and flap the ladder. The probe must stay
// suppressed for sixelProbeCooldown budgeted frames even with tiny writes,
// then fire on the next eligible frame.
func TestSixelBudgetProbeCooldownAfterLatencyEscalation(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st := tmuxSixelState()
	b := &st.sixelBudget

	// The 3.6a-style write pair escalates and arms the cool-down.
	b.recordWrite(215 * time.Millisecond)
	b.recordWrite(215 * time.Millisecond)
	if b.idx != 1 {
		t.Fatalf("two 215ms writes set the ladder index to %d, want 1", b.idx)
	}

	// The drained pipe writes tiny frames, well inside the probe band,
	// enough of them to fill the stability window.
	for range sixelProbeStabilityWindow {
		b.recordWrite(30 * time.Millisecond)
	}
	var out bytes.Buffer
	for i := range sixelProbeCooldown {
		out.Reset()
		if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
			t.Fatalf("RenderStreamFrame(cool-down frame %d) = %v", i, err)
		}
		if b.idx != 1 {
			t.Fatalf("probe fired on cool-down frame %d: ladder index = %d, want the held 1", i, b.idx)
		}
	}

	// The first frame past the cool-down probes on the accumulated cadence.
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(post-cool-down frame) = %v", err)
	}
	if b.idx != 0 {
		t.Fatalf("probe did not fire after the cool-down: ladder index = %d, want 0", b.idx)
	}
}

// TestSixelBudgetByteEscalationSkipsProbeCooldown scopes the cool-down to
// latency escalations: the byte-budget fit loop carries no latency signal, so
// its escalation leaves the cool-down unarmed and the probe keeps the normal
// sixelProbeInterval cadence.
func TestSixelBudgetByteEscalationSkipsProbeCooldown(t *testing.T) {
	st := tmuxSixelState()
	b := &st.sixelBudget

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: noisyImage(640, 464)}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(over-budget frame) = %v", err)
	}
	if b.idx == 0 {
		t.Fatal("over-budget frame did not escalate through the byte-budget loop")
	}
	if b.probeCooldown != 0 {
		t.Fatalf("byte-budget escalation armed the probe cool-down at %d, want 0", b.probeCooldown)
	}

	// The probe still fires on the plain cadence with a tiny write recorded.
	settled := b.idx
	b.sinceProbe = sixelProbeInterval - 1
	b.recordWrite(30 * time.Millisecond)
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	out.Reset()
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if b.idx != settled-1 {
		t.Fatalf("probe did not fire on the plain cadence: ladder index = %d, want %d", b.idx, settled-1)
	}
}

// TestRecordTmuxSixelWriteFeedsDownscaleLadder pins the wiring seam: the
// producer's recordTmuxSixelWrite folds the writer-measured duration into
// both the adaptive pacing state and the sixelBudget ladder, so two
// consecutive slow writes escalate through the session path.
func TestRecordTmuxSixelWriteFeedsDownscaleLadder(t *testing.T) {
	st := tmuxSixelState()
	s := &streamSession{st: &st}

	s.recordTmuxSixelWrite(310 * time.Millisecond)
	if got, want := st.sixelBudget.lastWriteDur, 310*time.Millisecond; got != want {
		t.Fatalf("budget lastWriteDur = %v, want the recorded %v", got, want)
	}
	if st.sixelBudget.idx != 0 {
		t.Fatalf("one slow session write moved the ladder index to %d, want 0", st.sixelBudget.idx)
	}
	s.recordTmuxSixelWrite(310 * time.Millisecond)
	if st.sixelBudget.idx != 1 {
		t.Fatalf("two slow session writes set the ladder index to %d, want 1", st.sixelBudget.idx)
	}
	if got, want := s.tmuxSixelInterval(), 310*time.Millisecond+tmuxSixelWriteMargin; got != want {
		t.Fatalf("pacing interval = %v, want the unchanged stretch %v", got, want)
	}
}

// TestSixelBudgetInactiveOutsideTmux asserts the zero-limit default keeps the
// direct streaming encode: a native-terminal Sixel frame over the tmux budget
// passes through unscaled.
func TestSixelBudgetInactiveOutsideTmux(t *testing.T) {
	st := NewState(80, 30)
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel
	img := noisyImage(640, 480)

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(native sixel) = %v", err)
	}
	if out.Len() <= sixelBudgetDefault {
		t.Fatalf("native noisy frame is %d bytes; expected the unbudgeted encode to exceed %d, so the budget ran where it must not", out.Len(), sixelBudgetDefault)
	}
	if st.sixelBudget.idx != 0 {
		t.Fatalf("native path moved the ladder index to %d, want untouched 0", st.sixelBudget.idx)
	}
}

// TestParseTmuxVersion pins the XTVERSION identity parse, patch letters and
// absence included.
func TestParseTmuxVersion(t *testing.T) {
	cases := []struct {
		termID       string
		major, minor int
		ok           bool
	}{
		{"tmux 3.5a", 3, 5, true},
		{"tmux 3.6", 3, 6, true},
		{"tmux 3.7b", 3, 7, true},
		{"tmux 4.0", 4, 0, true},
		{"", 0, 0, false},
		{"tmux", 0, 0, false},
		{"tmux next", 0, 0, false},
		{"WezTerm 3.6", 0, 0, false},
	}
	for _, tc := range cases {
		major, minor, ok := parseTmuxVersion(tc.termID)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("parseTmuxVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.termID, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

func TestResolveTmuxSixelBudget(t *testing.T) {
	cases := []struct {
		name, termID, output string
		runErr               error
		wantTarget, wantHard int
	}{
		{"default buffer", "tmux 3.8", "1048576\n", nil, sixelBudgetDefault, sixelBudgetDefault},
		{"large legacy buffer", "tmux 3.6a", "16777216\n", nil, sixelBudgetLegacyRate, sixelBudgetRaised},
		{"large current buffer", "tmux 3.7b", "16777216\n", nil, sixelBudgetRaised, sixelBudgetRaised},
		{"unsupported buffer", "tmux 3.8", "524288\n", nil, 0, 0},
		{"non-power-of-two buffer", "tmux 3.8", "1572864\n", nil, sixelBudgetDefault, sixelBudgetDefault},
		{"malformed buffer", "tmux 3.8", "1048576 extra\n", nil, 0, 0},
		{"failed query", "tmux 3.8", "1048576\n", errors.New("tmux query failed"), 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := func(args ...string) (string, error) {
				if got := strings.Join(args, " "); got != "show-options -sv input-buffer-size" {
					t.Fatalf("tmux query = %q", got)
				}
				return tc.output, tc.runErr
			}
			target, hard := resolveTmuxSixelBudget(tc.termID, query)
			if target != tc.wantTarget || hard != tc.wantHard {
				t.Fatalf("budget = (%d, %d), want (%d, %d)", target, hard, tc.wantTarget, tc.wantHard)
			}
		})
	}
}

// TestResolveTmuxSixelMinInterval pins the version-tuned pacing floor: tmux
// 3.7 and later drain their output scheduler at the 250 ms floor, while older
// tmux and an unknown version pay the 300 ms legacy floor (live 3.6a at
// 250 ms showed stall-and-burst release toward the host terminal even with
// the pty writes fully drained; 300 ms showed none).
func TestResolveTmuxSixelMinInterval(t *testing.T) {
	cases := []struct {
		termID string
		want   time.Duration
	}{
		{"tmux 3.7b", tmuxSixelMinInterval},
		{"tmux 3.7", tmuxSixelMinInterval},
		{"tmux 4.0", tmuxSixelMinInterval},
		{"tmux 3.6a", tmuxSixelMinIntervalLegacy},
		{"tmux 3.5a", tmuxSixelMinIntervalLegacy},
		{"", tmuxSixelMinIntervalLegacy},
	}
	for _, tc := range cases {
		if got := resolveTmuxSixelMinInterval(tc.termID); got != tc.want {
			t.Errorf("resolveTmuxSixelMinInterval(%q) = %v, want %v", tc.termID, got, tc.want)
		}
	}
}

// TestConfigureBackendsResolvesTmuxSixelFloor drives the real configure seam:
// the temp TTY answers no XTVERSION, so under tmux the unknown identity fails
// the 3.6+ version gate and the floor stays unarmed at zero (no Sixel will
// flow, so no budget state is written), while zellij leaves the floor
// untouched at the zero-value 250 ms default. The floor resolution itself is
// pinned by TestResolveTmuxSixelMinInterval.
func TestConfigureBackendsResolvesTmuxSixelFloor(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want time.Duration
	}{
		{"tmux unknown version fails the gate and stays unarmed", map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}, 0},
		{"zellij leaves the zero-value default", map[string]string{"ZELLIJ": "1"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMuxEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			tty := newTempTTY(t)
			st := NewState(80, 24)
			if err := configureBackends(&st, tty, 80, 24, "auto", "auto"); err != nil {
				t.Fatalf("configureBackends: %v", err)
			}
			if got := st.sixelBudget.minInterval; got != c.want {
				t.Fatalf("sixelBudget.minInterval = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSixelBudgetLegacyFloorRaisesEscalationThreshold pins the escalation
// arithmetic at the resolved floor: a 210 ms write (260 ms effective) reaches
// the 250 ms default floor, so two of them escalate the ladder, while the
// same pair stays under the 300 ms legacy floor and leaves the ladder alone.
func TestSixelBudgetLegacyFloorRaisesEscalationThreshold(t *testing.T) {
	const write = 210 * time.Millisecond

	def := tmuxSixelState()
	def.sixelBudget.recordWrite(write)
	def.sixelBudget.recordWrite(write)
	if def.sixelBudget.idx != 1 {
		t.Fatalf("two 210ms writes at the default floor set the ladder index to %d, want 1", def.sixelBudget.idx)
	}

	legacy := tmuxSixelState()
	legacy.sixelBudget.minInterval = tmuxSixelMinIntervalLegacy
	legacy.sixelBudget.recordWrite(write)
	legacy.sixelBudget.recordWrite(write)
	if legacy.sixelBudget.idx != 0 {
		t.Fatalf("two 210ms writes at the legacy floor set the ladder index to %d, want the held 0", legacy.sixelBudget.idx)
	}
}

// TestSixelBudgetLegacyFloorWidensProbeBand pins the probe band at the
// resolved floor: a 160 ms in-trial write breaches the 150 ms band of the
// 250 ms default floor and retreats the pending probe, while the same write
// sits inside the 180 ms band of the 300 ms legacy floor and leaves the
// trial running.
func TestSixelBudgetLegacyFloorWidensProbeBand(t *testing.T) {
	const write = 160 * time.Millisecond

	arm := func(b *sixelBudget) {
		b.idx = 1
		b.preProbeIdx = 2
		b.probePending = true
	}

	def := tmuxSixelState()
	arm(&def.sixelBudget)
	def.sixelBudget.recordWrite(write)
	if def.sixelBudget.idx != 2 || def.sixelBudget.probePending {
		t.Fatalf("160ms in-trial write at the default floor left index = %d, pending = %v, want the retreated 2 and false",
			def.sixelBudget.idx, def.sixelBudget.probePending)
	}

	legacy := tmuxSixelState()
	legacy.sixelBudget.minInterval = tmuxSixelMinIntervalLegacy
	arm(&legacy.sixelBudget)
	legacy.sixelBudget.recordWrite(write)
	if legacy.sixelBudget.idx != 1 || !legacy.sixelBudget.probePending {
		t.Fatalf("160ms in-trial write at the legacy floor left index = %d, pending = %v, want the held 1 and true",
			legacy.sixelBudget.idx, legacy.sixelBudget.probePending)
	}
}

// TestTmuxSixelAdaptiveIntervalLegacyFloor pins the emit pacing at the legacy
// floor: a fresh session and a fast write both pace at 300 ms, a 210 ms
// write (260 ms effective, above the 250 ms default) stays clamped to the
// legacy floor, and a genuinely slow write still stretches past it.
func TestTmuxSixelAdaptiveIntervalLegacyFloor(t *testing.T) {
	st := tmuxSixelState()
	st.sixelBudget.minInterval = tmuxSixelMinIntervalLegacy
	s := &streamSession{st: &st}

	if got := s.tmuxSixelInterval(); got != tmuxSixelMinIntervalLegacy {
		t.Fatalf("fresh interval = %v, want the %v legacy floor", got, tmuxSixelMinIntervalLegacy)
	}
	s.recordTmuxSixelWrite(210 * time.Millisecond)
	if got := s.tmuxSixelInterval(); got != tmuxSixelMinIntervalLegacy {
		t.Fatalf("interval after a 210ms write = %v, want the %v legacy floor", got, tmuxSixelMinIntervalLegacy)
	}
	s.recordTmuxSixelWrite(400 * time.Millisecond)
	if got, want := s.tmuxSixelInterval(), 400*time.Millisecond+tmuxSixelWriteMargin; got != want {
		t.Fatalf("interval after a 400ms write = %v, want the stretched %v", got, want)
	}
}

// TestTmuxSixelDropsSyncAndErase compares assembled tmux and native Sixel
// frames. Under tmux, on every version, a steady Sixel frame carries the bare
// cursor-home prefix (no per-frame erase: the new full-frame image paints
// over the old one, and tmux's stored-image LRU bounds the stack) and no DEC
// 2026 markers (tmux does not defer image operations under an application
// sync). The native frame keeps the fence and the same bare cursor-home
// prefix.
func TestTmuxSixelDropsSyncAndErase(t *testing.T) {
	frame := render.Frame{Img: noisyImage(8, 16)}

	assembleSteady := func(st *State) []byte {
		t.Helper()
		s := &streamSession{st: st}
		s.firstDrawn.Store(true) // steady state: past the first draw
		var out bytes.Buffer
		if _, err := s.assemble(&out, frame); err != nil {
			t.Fatalf("assemble Sixel frame: %v", err)
		}
		return out.Bytes()
	}

	for _, name := range []string{"tmux 3.7b", "tmux 3.7", "tmux 3.6a", "tmux 3.5a", "unknown identity"} {
		t.Run(name, func(t *testing.T) {
			st := tmuxSixelState()
			st.compat.sixelSyncUpdatesDisabled = true // configureBackends sets this on every tmux version
			out := assembleSteady(&st)
			if !bytes.HasPrefix(out, []byte(cursorHome)) {
				t.Fatalf("tmux Sixel steady prefix = %q, want the bare %q", out[:16], cursorHome)
			}
			if bytes.Contains(out, []byte("\x1b[2J")) {
				t.Fatalf("tmux Sixel steady frame carries a per-frame erase: %q", out[:32])
			}
			if bytes.Contains(out, []byte(beginSyncUpdate)) || bytes.Contains(out, []byte(endSyncUpdate)) {
				t.Fatalf("tmux Sixel frame carries DEC 2026 markers: %q", out)
			}
		})
	}

	nativeState := NewState(80, 30)
	nativeState.SetEnabled([]Backend{BackendSixel, BackendBlock})
	nativeState.Backend = BackendSixel
	nativeOut := assembleSteady(&nativeState)
	if !bytes.HasPrefix(nativeOut, []byte(beginSyncUpdate+cursorHome)) {
		t.Fatalf("native Sixel steady prefix = %q, want fence then bare cursor home", nativeOut[:16])
	}
	if !bytes.HasSuffix(nativeOut, []byte(endSyncUpdate)) {
		t.Fatalf("native Sixel frame lost its DEC 2026 close: %q", nativeOut)
	}
}

// TestTmuxBlockKeepsSyncUpdatesAndHomePrefix scopes the tmux Sixel sync
// suppression to Sixel: a Block frame under the same tmux compatibility state
// keeps the DEC 2026 fence and the bare cursor-home steady prefix.
func TestTmuxBlockKeepsSyncUpdatesAndHomePrefix(t *testing.T) {
	st := tmuxSixelState()
	st.compat.sixelSyncUpdatesDisabled = true
	st.Backend = BackendBlock
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	s := &streamSession{st: &st, diff: diff}
	s.firstDrawn.Store(true)
	var out bytes.Buffer
	if _, err := s.assemble(&out, render.Frame{Img: noisyImage(16, 16)}); err != nil {
		t.Fatalf("assemble tmux Block frame: %v", err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte(beginSyncUpdate+cursorHome)) {
		t.Fatalf("tmux Block steady prefix = %q, want fence then bare cursor home", out.Bytes()[:16])
	}
	if !bytes.HasSuffix(out.Bytes(), []byte(endSyncUpdate)) {
		t.Fatalf("tmux Block frame lost its DEC 2026 close: %q", out.Bytes())
	}
}

// TestConfigureBackendsDisablesSixelSyncUpdatesUnderTmuxOnly drives the real
// configure seam: under TMUX the Sixel sync-update policy flips off on every
// version, while zellij keeps the normal fence.
func TestConfigureBackendsDisablesSixelSyncUpdatesUnderTmuxOnly(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"tmux", map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}, true},
		{"zellij", map[string]string{"ZELLIJ": "1"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMuxEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			tty := newTempTTY(t)
			st := NewState(80, 24)
			if err := configureBackends(&st, tty, 80, 24, "auto", "auto"); err != nil {
				t.Fatalf("configureBackends: %v", err)
			}
			if st.compat.sixelSyncUpdatesDisabled != c.want {
				t.Fatalf("sixelSyncUpdatesDisabled = %v, want %v", st.compat.sixelSyncUpdatesDisabled, c.want)
			}
		})
	}
}

// TestTmuxSixelPacingGateCoalesces pins the emit-rate gate: inside the
// minimum interval the gate holds the encode back (so pending frames keep
// coalescing latest-wins instead of queueing), it opens once the interval
// passes, and it never gates any other backend or environment.
func TestTmuxSixelPacingGateCoalesces(t *testing.T) {
	st := tmuxSixelState()
	s := &streamSession{st: &st}

	if s.tmuxSixelGated() {
		t.Fatal("fresh session is gated; the first frame must not wait")
	}
	s.assembledTmuxSixel = true
	s.markTmuxSixelHandoff()
	if !s.tmuxSixelGated() {
		t.Fatal("gate did not close after a tmux Sixel handoff")
	}
	if s.assembledTmuxSixel {
		t.Fatal("handoff did not consume the assembled-frame tag")
	}
	s.lastTmuxSixel = time.Now().Add(-tmuxSixelMinInterval)
	if s.tmuxSixelGated() {
		t.Fatal("gate stayed closed after the minimum interval elapsed")
	}

	// A non-Sixel backend and a non-tmux environment never gate.
	s.lastTmuxSixel = time.Now()
	st.Backend = BackendBlock
	if s.tmuxSixelGated() {
		t.Fatal("Block under tmux is gated")
	}
	st.Backend = BackendSixel
	st.mux = muxZellij
	if s.tmuxSixelGated() {
		t.Fatal("Sixel under zellij is gated")
	}
}

// TestTmuxSixelAdaptiveInterval pins the write-duration back-off: a fresh
// session paces at the floor, a slow recorded write stretches the next
// interval to the write duration plus the margin (and gates a frame the
// floor alone would release), a fast write returns the gate to the floor,
// the ceiling clamps a pathological write, the settle delay follows the
// effective interval, and no stretched interval gates another backend or
// environment.
func TestTmuxSixelAdaptiveInterval(t *testing.T) {
	st := tmuxSixelState()
	s := &streamSession{st: &st}

	if got := s.tmuxSixelInterval(); got != tmuxSixelMinInterval {
		t.Fatalf("fresh interval = %v, want the %v floor", got, tmuxSixelMinInterval)
	}

	// A slow write stretches the next interval to the write plus the margin.
	s.recordTmuxSixelWrite(400 * time.Millisecond)
	if got, want := s.tmuxSixelInterval(), 400*time.Millisecond+tmuxSixelWriteMargin; got != want {
		t.Fatalf("interval after a 400ms write = %v, want %v", got, want)
	}
	// A frame the floor alone would release stays gated behind the stretch.
	s.lastTmuxSixel = time.Now().Add(-tmuxSixelMinInterval)
	if !s.tmuxSixelGated() {
		t.Fatal("floor-aged handoff passed the gate despite the stretched interval")
	}

	// A fast write returns the gate to the floor.
	s.recordTmuxSixelWrite(30 * time.Millisecond)
	if got := s.tmuxSixelInterval(); got != tmuxSixelMinInterval {
		t.Fatalf("interval after a 30ms write = %v, want the %v floor", got, tmuxSixelMinInterval)
	}
	if s.tmuxSixelGated() {
		t.Fatal("floor-aged handoff stayed gated after a fast write")
	}

	// The ceiling clamps a pathological write, so the stream cannot freeze.
	s.recordTmuxSixelWrite(5 * time.Second)
	if got := s.tmuxSixelInterval(); got != tmuxSixelMaxInterval {
		t.Fatalf("interval after a 5s write = %v, want the %v ceiling", got, tmuxSixelMaxInterval)
	}

	// The settle deadline tracks the effective interval, not the floor, so a
	// settle emit never fires into a saturated pipe.
	if got, want := s.tmuxSixelSettleDelay(), tmuxSixelSettleIntervals*tmuxSixelMaxInterval; got != want {
		t.Fatalf("settle delay = %v, want %v (two effective intervals)", got, want)
	}

	// A stretched interval never gates another backend or environment.
	s.lastTmuxSixel = time.Now()
	st.Backend = BackendBlock
	if s.tmuxSixelGated() {
		t.Fatal("Block under tmux is gated by the stretched interval")
	}
	st.Backend = BackendSixel
	st.mux = muxZellij
	if s.tmuxSixelGated() {
		t.Fatal("Sixel under zellij is gated by the stretched interval")
	}
}

// TestStreamLoopTmuxSixelPacingCoalesces drives the real loop with a burst of
// tmux Sixel frames inside one pacing interval and asserts the gate coalesces
// them latest-wins: far fewer writes than frames land, the loop never queues,
// and it still quits promptly.
func TestStreamLoopTmuxSixelPacingCoalesces(t *testing.T) {
	tty := &slowWriter{}
	frames := make(chan render.Frame)
	eng := &pipeEngine{frames: frames}

	// A tiny grid keeps each encode cheap, so the whole burst lands well
	// inside one pacing interval. Grid extent 32x32: 4x3 cells at the 8x16
	// default with the tmux bottom row reserved.
	st := NewState(4, 3)
	st.mux = muxTmux
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel
	st.sixelBudget.limit = sixelBudgetDefault
	st.compat.sixelSyncUpdatesDisabled = true
	s := &streamSession{eng: eng, st: &st, tty: tty}

	keys := make(chan keyEvent, 1)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = s.loop(nil, keys)
	}()

	// A burst well inside one 100ms pacing interval: the first frame passes
	// ungated, the rest coalesce behind the closed gate.
	const pushed = 20
	img := noisyImage(32, 32)
	for range pushed {
		frames <- render.Frame{Img: img}
	}
	keys <- keyEvent{b: keyQuitCtrlQ}

	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not quit: the pacing gate blocked the consumer")
	}

	// The teardown flush may hand one gated frame over, so at most a handful
	// of writes land for the 20-frame burst.
	if got := tty.count(); got > 3 {
		t.Fatalf("writer presented %d frames for a %d-frame burst inside one pacing interval; the gate queued instead of coalescing", got, pushed)
	}
	if tty.count() == 0 {
		t.Fatal("writer presented no frames at all")
	}
}

// TestSixelSettleReencodesSharp pins the settle seam at the State level: an
// over-budget frame leaves the ladder above 1 with its source retained, the
// settle emit re-encodes at factor 1 (more bytes than the blurred emit) at
// the unchanged grid extent, resets the ladder, and drops the retained
// source so a second settle cannot fire without a new budgeted frame.
func TestSixelSettleReencodesSharp(t *testing.T) {
	st := tmuxSixelState()
	gridW, gridH := 640, 464
	img := noisyImage(gridW, gridH)

	var budgeted bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &budgeted); err != nil {
		t.Fatalf("RenderStreamFrame(tmux sixel) = %v", err)
	}
	if !st.sixelSettleNeeded() {
		t.Fatal("over-budget frame did not arm the settle emit")
	}

	var settled bytes.Buffer
	if err := st.renderSixelSettle(&settled); err != nil {
		t.Fatalf("renderSixelSettle = %v", err)
	}
	if settled.Len() <= budgeted.Len() {
		t.Fatalf("settle emit is %d bytes, want more than the %d-byte blurred emit (factor 1 carries more detail)", settled.Len(), budgeted.Len())
	}
	decoded, err := renderdecode.DecodeSixel(settled.Bytes())
	if err != nil {
		t.Fatalf("DecodeSixel(settle emit): %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != gridW || b.Dy() != gridH {
		t.Fatalf("settle raster = %dx%d, want the unchanged %dx%d grid extent", b.Dx(), b.Dy(), gridW, gridH)
	}
	if st.sixelBudget.idx != 0 {
		t.Fatalf("settle emit left the ladder at index %d, want the reset 0", st.sixelBudget.idx)
	}
	if st.sixelSettleNeeded() {
		t.Fatal("settle emit stayed armed; a second settle would loop without a new frame")
	}
}

func TestTmuxSixelHardLimit(t *testing.T) {
	st := tmuxSixelState()
	st.sixelBudget.hardLimit = sixelBudgetDefault
	s := &streamSession{st: &st}
	frame := render.Frame{Img: noisyImage(640, 464)}
	var motion, settle bytes.Buffer
	if _, err := s.assemble(&motion, frame); err != nil {
		t.Fatal(err)
	}
	dcsStart := bytes.Index(motion.Bytes(), []byte("\x1bP"))
	dcsEnd := bytes.Index(motion.Bytes(), []byte("\x1b\\"))
	if dcsStart < 0 || dcsEnd < dcsStart || dcsEnd+2-dcsStart > st.sixelBudget.hardLimit {
		t.Fatalf("motion Sixel DCS bounds: start %d, end %d, cap %d", dcsStart, dcsEnd, st.sixelBudget.hardLimit)
	}
	if !s.tmuxSixelSettlePending() {
		t.Fatal("motion did not arm a sharp settle")
	}
	if err := s.assembleSettle(&settle); err != nil {
		t.Fatal(err)
	}
	if settle.Len() != 0 || s.tmuxSixelSettlePending() {
		t.Fatal("oversized idle settle wrote bytes or remained pending")
	}

	// A skipped frame must not clear the terminal or arm a new settle.
	st.sixelBudget.limit = 32
	st.sixelBudget.hardLimit = 32
	if _, err := s.assemble(&motion, frame); err != nil {
		t.Fatal(err)
	}
	if motion.Len() != 0 || s.tmuxSixelSettlePending() {
		t.Fatal("oversized motion frame wrote bytes or armed a settle")
	}
	// The resize clears the displayed image but the session still counts
	// as drawn. A failure to paint the new grid must not leave it blank.
	st.Resize(st.Cols, st.Rows)
	if _, err := s.assemble(&motion, frame); err == nil || !strings.Contains(err.Error(), "--backend=block") {
		t.Fatalf("first oversized Sixel frame after resize error = %v, want Block advice", err)
	}
}

func TestSixelSettleKeepsLastFrameAfterHardLimitSkip(t *testing.T) {
	st := tmuxSixelState()
	st.sixelBudget.hardLimit = 20_000
	s := &streamSession{st: &st}
	// Both frames require fitting to the grid, so encoding the rejected frame
	// overwrites the scratch pixels that the displayed frame used.
	displayed := image.NewRGBA(image.Rect(0, 0, 320, 232))
	draw.Draw(displayed, displayed.Bounds(), image.NewUniform(color.RGBA{R: 255, A: 255}), image.Point{}, draw.Src)
	displayed.SetRGBA(4, 4, color.RGBA{B: 255, A: 255})
	frame := render.Frame{Img: displayed}
	var sharp, out bytes.Buffer
	if err := st.RenderStreamFrame(frame, &sharp); err != nil {
		t.Fatal(err)
	}
	st.sixelBudget.idx = 1
	if _, err := s.assemble(&out, frame); err != nil {
		t.Fatal(err)
	}
	if sharp.Len() == 0 || out.Len() == 0 || !s.tmuxSixelSettlePending() {
		t.Fatal("test did not produce a sharp reference and an accepted blurred frame")
	}

	rejected := render.Frame{Img: noisyImage(320, 232)}
	if _, err := s.assemble(&out, rejected); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !s.tmuxSixelSettlePending() {
		t.Fatal("over-limit frame wrote bytes or cleared the pending settle")
	}
	if err := st.renderSixelSettle(&out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), sharp.Bytes()) || s.tmuxSixelSettlePending() {
		t.Fatal("settle did not emit the displayed frame at full quality once")
	}
}

func TestSixelSettleKeepsPixelsAfterFrameRelease(t *testing.T) {
	st := tmuxSixelState()
	img := noisyImage(640, 464)
	var sharp, out bytes.Buffer
	st.sixelBudget.limit = sixelBudgetRaised
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &sharp); err != nil {
		t.Fatal(err)
	}
	st.sixelBudget.idx = 1
	frame := render.Frame{Img: img, Release: func() { clear(img.Pix) }}
	if err := st.RenderStreamFrame(frame, &out); err != nil {
		t.Fatal(err)
	}
	if !st.sixelSettleNeeded() || img.Pix[3] != 0 {
		t.Fatal("test did not arm a settle and release the pooled frame")
	}
	out.Reset()
	if err := st.renderSixelSettle(&out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), sharp.Bytes()) {
		t.Fatal("settle did not preserve the released frame's pixels")
	}
}

func TestTmuxSixelInvalidatesPendingSettle(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*State)
	}{
		{"resize", func(st *State) { st.Resize(70, 28) }},
		{"resize at same grid", func(st *State) { st.Resize(st.Cols, st.Rows) }},
		{"cycle away and back", func(st *State) { st.CycleBackend(); st.CycleBackendReverse() }},
		{"replace enabled backends", func(st *State) {
			st.SetEnabled([]Backend{BackendBlock})
			st.SetEnabled([]Backend{BackendSixel, BackendBlock})
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			st := tmuxSixelState()
			s := &streamSession{st: &st}
			st.sixelBudget.idx = 2
			st.sixelBudget.lastImg = noisyImage(640, 464)
			st.sixelBudget.hasFrame = true
			if !s.tmuxSixelSettlePending() {
				t.Fatal("test did not arm a settle")
			}
			change.apply(&st)
			if s.tmuxSixelSettlePending() || st.sixelBudget.hasFrame {
				t.Fatal("old Sixel frame stayed valid across a resize or backend change")
			}
		})
	}
}

// TestStreamLoopTmuxSixelIdleSettles drives the real loop: one over-budget
// tmux Sixel frame blurs the ladder, then the idle timer emits exactly one
// full-quality settle re-encode, resets the ladder, and stays quiet with no
// further frames.
func TestStreamLoopTmuxSixelIdleSettles(t *testing.T) {
	tty := &slowWriter{}
	frames := make(chan render.Frame)
	eng := &pipeEngine{frames: frames}

	// The tiny pacing-test grid (engine grid extent 32x32) with a budget far
	// below the noisy frame's encode, so the ladder escalates and the settle
	// arms.
	st := NewState(4, 3)
	st.mux = muxTmux
	st.SetEnabled([]Backend{BackendSixel, BackendBlock})
	st.Backend = BackendSixel
	st.sixelBudget.limit = 256
	st.compat.sixelSyncUpdatesDisabled = true
	s := &streamSession{eng: eng, st: &st, tty: tty}

	keys := make(chan keyEvent, 1)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = s.loop(nil, keys)
	}()

	frames <- render.Frame{Img: noisyImage(32, 32)}

	// The budgeted frame goes out, then exactly one settle emit once the
	// idle threshold passes: two writes total.
	deadline := time.Now().Add(5 * time.Second)
	for tty.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := tty.count(); got != 2 {
		t.Fatalf("writer presented %d frames, want the budgeted frame plus one settle emit", got)
	}

	// No further settle emits without a new budgeted frame. The in-memory tty
	// writes instantly, so the effective interval sits at the floor and two
	// settle delays cover any timer slack.
	time.Sleep(2 * tmuxSixelSettleIntervals * tmuxSixelMinInterval)
	if got := tty.count(); got != 2 {
		t.Fatalf("idle stream presented %d frames, want the one-shot settle to stay at 2", got)
	}

	keys <- keyEvent{b: keyQuitCtrlQ}
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not quit after the settle emit")
	}
	if st.sixelBudget.idx != 0 {
		t.Fatalf("settle emit left the ladder at index %d, want the reset 0", st.sixelBudget.idx)
	}
	if st.sixelSettleNeeded() {
		t.Fatal("settle stayed armed after the one-shot emit")
	}
}
