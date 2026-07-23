package viewer

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

// sixelBudgetDefault is the per-frame Sixel byte budget fallback under tmux
// where the input-buffer-size raise failed on a 3.6-or-later server. It keeps
// a safety margin under tmux's default 1 MiB input buffer, whose overflow
// discards the DCS silently. tmux below 3.6 never reaches it: the version
// gate (tmuxSixelSupported) refuses Sixel there outright, because the 1 MiB
// cap is compiled in with no option to raise it and this margin cannot
// guarantee the cap on large grids.
const sixelBudgetDefault = 900_000

// sixelBudgetRaised is the per-frame Sixel byte budget under tmux once the
// input-buffer-size raise succeeded: 15 MiB, riding under the 16 MiB raised
// buffer. What tmux needs is rate pacing, not byte capping: live tmux 3.7b
// runs with no per-frame erase sustained ~4 MB full-quality frames
// indefinitely at the tmuxSixelMinInterval pacing, tmux consuming each
// frame in about 230 ms. The raised budget keeps tmux 3.7 and later at full
// quality; tmux 3.6.x takes sixelBudgetLegacyRate instead and the downscale
// ladder stays the compatibility fallback for older tmux.
const sixelBudgetRaised = 15 << 20

// sixelBudgetLegacyRate is the per-frame Sixel byte budget under tmux 3.6.x
// once the input-buffer-size raise succeeded: 1.5 MB, an output-stage rate
// cap of ~5 MB/s at the 300 ms legacy pacing floor. The raised budget is too
// generous for 3.6.x: live 3.6a with the ladder settled at factor 2 (2.5 MB
// frames, 300 ms pacing, ~8.3 MB/s) kept its pty writes fully drained at
// ~21 ms, yet quitting replayed a queued backlog, and an earlier factor-3
// run at the 250 ms floor (~8.8 MB/s) burst identically. The bottleneck is
// 3.6's re-encoded output toward the host terminal, invisible to every
// write-duration signal, and factor 3 trims only ~10 percent off factor 2 on
// such content, so the fix is a byte-rate cap, not a deeper ladder factor.
// The ladder fit loop enforces the cap per frame: the heaviest observed
// factor-4 encode (~1.45 MB) fits and calm pages pass at factor 1.
const sixelBudgetLegacyRate = 1_500_000

// tmuxInputBufferSize is the value handed to `tmux set -s input-buffer-size`
// on tmux 3.6 and later: 16 MiB. The raised budgets ride on this raise: a
// successful set returns sixelBudgetRaised (3.7+) or sixelBudgetLegacyRate
// (3.6.x) from resolveTmuxSixelBudget, and a failure keeps the
// sixelBudgetDefault fallback under tmux's default 1 MiB input buffer.
const tmuxInputBufferSize = "16777216"

// debugTimingEnv is the live timing control the budgeted Sixel path reads at
// configure time: set to any non-empty value, each budgeted frame reports its
// encoded byte size and active downscale factor on stderr. It is the same
// NYALA_DEBUG_TIMING switch the engine's load-milestone trace uses.
const debugTimingEnv = "NYALA_DEBUG_TIMING"

// sixelProbeInterval is how many budgeted frames pass before the encoder
// probes one ladder step weaker, so quality recovers once content calms down
// while steady state pays no double encode. Ten frames is about three
// seconds at the tmux emit pacing, so a scrolled text page climbs back to
// sharp soon after scrolling stops. The same count is the minimum length of
// the probe trial: the sharper factor survives only once the cadence has
// elapsed AND the stability window shows the writes flat or falling with
// every one inside the probe band. A window still rising at the cadence
// keeps the trial pending: live tmux 3.6a at the 300 ms floor ramped
// 30 -> 146 ms across the ten-frame trial, all inside the 180 ms band, so a
// cadence-only trial declared survival while tmux's buffer absorption still
// hid the saturation, the writes then climbed to 250-296 ms, the escalation
// backstop fired, and the ladder hunted on a cool-down cycle. No
// trial-length cap is needed: a ramp either stabilises (true survival) or
// crosses the band (immediate retreat in recordWrite).
const sixelProbeInterval = 10

// sixelSlowWriteStreak is how many consecutive slow writes must land before
// the ladder escalates one step, so a single stall (a host terminal hiccup, a
// one-off giant frame) does not blur the stream. A write is slow when its
// effective pacing interval (duration plus tmuxSixelWriteMargin) reaches the
// resolved pacing floor, that is a write of 200 ms or more at the 250/50
// values. The criterion is whether tmux ever idles between
// frames, not whether a write exceeds some multiple of the floor: a healthy
// drained pipe shows tiny writes (live tmux 3.7b at the floor: 33 ms mean,
// 40 ms max), while sustained writes near the floor mean tmux runs at exact
// capacity with zero slack, so backlog built during earlier bursts never
// drains and replays on quit (live tmux 3.6a under a 1.4x-floor threshold
// held ~215 ms writes at factor 2, under that threshold, and never drained
// its backlog). The same count is the number of post-escalation write
// samples recordWrite skips before a fresh streak can build, so backlog
// encoded at the pre-escalation factor cannot chain escalations.
const sixelSlowWriteStreak = 2

// sixelProbeWriteFraction bounds the weaker-quality probe: it fires only
// while the last measured write duration sits under this fraction of the
// resolved pacing floor (60 percent: 150 ms at the 250 ms floor, 180 ms at
// the legacy 300 ms floor), comfortably clear of the pacing-floor escalation
// threshold in recordWrite. The gap between the two bands is the hysteresis
// that keeps a system saturating at one factor from oscillating between it
// and the next sharper step. The same band judges a pending probe trial: any
// recorded write reaching it fails the trial immediately (recordWrite),
// because live tmux 3.6a showed tmux's buffers absorbing a failing sharper
// factor while writes ramped 20 -> 107 ms across the cadence without ever
// reaching the 200 ms escalation threshold, so the escalation streak blamed
// the next, sharper probe instead. probeBand carries the absolute value.
const sixelProbeWriteFraction = 0.6

// sixelProbeStabilityWindow is how many recent recorded writes the sharper
// probe inspects before it may fire: all must sit inside the probe band and
// they must not rise monotonically across the window. Steady-state jitter
// (18, 20, 19 ms) passes; a monotonic ramp (69, 83, 90 ms) is the signature
// of tmux buffering content the current pipe cannot sustain, so probing
// sharper mid-ramp would tip the accumulated queue.
const sixelProbeStabilityWindow = 3

// sixelProbeCooldown is how many budgeted frames the weaker-quality probe
// stays suppressed after a latency-driven ladder escalation or an episode
// resume, roughly ten seconds at the paced rate. A saturated pipe drains
// once the ladder steps, so the very next writes are tiny and would pass the
// probe band within the normal sixelProbeInterval cadence, re-sharpen the
// frame, re-saturate the pipe, and flap the ladder. Byte-budget escalations
// do not arm the cool-down: they carry no latency signal, so the plain
// cadence applies.
const sixelProbeCooldown = 40

// sixelDownscaleLadder is the escalation ladder of source downscale factors
// the budget loop steps through until the encoded frame fits. Index 0 encodes
// the source untouched; the last entry is the floor, sent even when it still
// exceeds the budget.
var sixelDownscaleLadder = [...]float64{1, 1.5, 2, 3, 4}

// sixelBudget is the tmux Sixel byte-budget state the viewer keeps across
// frames. A zero limit disables the machinery, so every non-tmux path pays
// one integer check. The encode buffer and the two scale destinations are
// reused across frames on the single draw goroutine.
type sixelBudget struct {
	limit int // per-frame byte budget; 0 disables budgeting
	idx   int // active sixelDownscaleLadder index, remembered across frames

	// minInterval is the resolved tmux Sixel pacing floor, written once at
	// configure time by resolveTmuxSixelMinInterval (250 ms on tmux 3.7 and
	// later, 300 ms below 3.7 or when the version is unknown) and read only
	// on the producer goroutine through floor(). The zero value (non-tmux
	// paths, directly constructed test states) falls back to
	// tmuxSixelMinInterval.
	minInterval time.Duration

	sinceProbe int // budgeted frames since the last weaker-quality probe

	// lastWriteDur is the most recent measured tmux Sixel frame write
	// duration, folded in through recordWrite on the producer goroutine. It
	// gates the weaker-quality probe. The zero value (no write measured yet)
	// leaves the probe free to fire, matching the pre-latency behaviour.
	lastWriteDur time.Duration

	// recentWrites holds the last sixelProbeStabilityWindow recorded write
	// durations, oldest first. The stability gate reads it before a sharper
	// probe may fire; an incomplete window (fresh state, or just after a
	// settle emit) leaves the probe free to fire.
	recentWrites []time.Duration

	// preProbeIdx is the ladder index the last fired sharper probe stepped
	// from. A failed trial retreats straight back to it instead of walking
	// the ladder through escalations.
	preProbeIdx int

	// slowStreak counts consecutive slow writes toward the latency
	// escalation; any write under the threshold resets it.
	slowStreak int

	// escalationSkips counts write samples still ignored after a ladder
	// escalation or probe retreat. Frames encoded at the previous factor are
	// in flight through the writer handoff, and tmux itself holds backlog
	// encoded at that factor, so the first write durations after the step
	// measure the old factor's cost. Counting them would chain escalations
	// before the new factor is ever observed (live tmux 3.6a stepped
	// 1.5 -> 4 within four frames). Any escalation or retreat arms it to
	// sixelSlowWriteStreak. It never filters the probe-band trial check:
	// that check runs only while a probe is pending, and a pending probe
	// cannot coexist with a fresh escalation (the band breach retreats it
	// first).
	escalationSkips int

	// probeCooldown is how many budgeted frames the weaker-quality probe
	// stays suppressed. A latency escalation and an episode resume arm it
	// to sixelProbeCooldown; each budgeted frame spends one.
	probeCooldown int

	// probePending marks a fired weaker-quality probe whose outcome is
	// undecided. Any recorded write reaching the probe band while pending
	// fails the trial: the ladder retreats to preProbeIdx and probing
	// switches off for the rest of the episode. Survival needs the full
	// sixelProbeInterval cadence AND a stable window (probeWindowStable):
	// writes flat or falling, all inside the band. A window still rising at
	// the cadence keeps the trial pending until it stabilises or breaches
	// the band; surviving keeps probing enabled.
	probePending bool

	// probesExhausted disables the weaker-quality probe for the remainder
	// of the motion episode once a sharper probe trial fails. The ladder
	// watched that factor fail, so re-testing it during sustained motion
	// only buys a saturation ramp per retry (live tmux 3.6a: factor 2 just
	// over capacity bred a ~15 s quality-breathing cycle). The settle emit
	// that ends the episode clears it, so the next episode probes afresh.
	probesExhausted bool

	// resumeIdx is the ladder index the last settle emit stepped down from.
	// The first budgeted frame of the next motion episode resumes there
	// instead of at index 0, so resuming motion does not re-run the whole
	// discovery cascade the previous episode already paid for (live tmux
	// 3.6a: 1 -> 1.5 -> 2 -> 3 with 400-535 ms writes after every settle).
	// Consuming it clears it.
	resumeIdx int

	buf    bytes.Buffer // reused whole-frame encode buffer the budget measures
	small  *image.RGBA  // reused downscale destination
	fitted *image.RGBA  // reused upscale-back destination

	// lastImg retains the source of the last budgeted emit so the idle
	// settle path can re-encode it at full quality. Every budgeted frame
	// replaces it and renderSixelSettle drops it, so a settle cannot re-fire
	// until a new budgeted frame goes out.
	lastImg image.Image

	// debug arms the per-frame stderr trace of the encoded byte size and the
	// active factor. It follows NYALA_DEBUG_TIMING, the existing live timing
	// control, resolved once at configure time.
	debug bool
}

// floor returns the resolved tmux Sixel pacing floor: the configure-time
// minInterval when one landed, otherwise the tmuxSixelMinInterval default,
// so non-tmux paths and directly constructed test states keep the 250 ms
// behaviour.
func (b *sixelBudget) floor() time.Duration {
	if b.minInterval > 0 {
		return b.minInterval
	}
	return tmuxSixelMinInterval
}

// probeBand returns the write-duration ceiling of the probe machinery:
// sixelProbeWriteFraction of the resolved pacing floor (150 ms at the 250 ms
// floor, 180 ms at the legacy 300 ms floor). It gates firing a sharper probe
// and judges a pending trial.
func (b *sixelBudget) probeBand() time.Duration {
	return time.Duration(sixelProbeWriteFraction * float64(b.floor()))
}

// recordWrite folds one measured tmux Sixel frame write duration into the
// ladder state. The write blocks at tmux's own read pace, so the duration is
// a direct consumption signal. While a sharper probe trial is pending, any
// write reaching the probe band fails the trial at once and retreats the
// ladder: tmux's buffers absorb a failing sharper factor, so live 3.6a
// writes ramped 20 -> 107 ms across a cadence without ever reaching the
// 200 ms escalation threshold, and waiting for the escalation streak blamed
// the next, sharper probe instead. The band check is never filtered by the
// post-step sample skip, which only follows a ladder step. Outside a trial,
// when the effective pacing interval (duration plus tmuxSixelWriteMargin)
// reaches the resolved pacing floor for sixelSlowWriteStreak
// consecutive writes, tmux never idles between frames, so the ladder
// escalates one step and arms the sixelProbeCooldown, trading one mild blur
// step for frame rate on a tmux running at exact capacity: the hard
// backstop for content that saturates a settled factor. A sub-threshold
// write resets the streak, and the duration also gates the weaker-quality
// probe in renderSixelBudgeted. The first escalationSkips samples after any
// escalation or retreat are ignored for the streak: they measure backlog
// encoded at the previous factor, so counting them would chain escalations
// before the new factor's true cost is observed. The producer goroutine
// calls this while folding in the writer's free-channel result, so the
// ladder state stays single-goroutine.
func (b *sixelBudget) recordWrite(d time.Duration) {
	b.lastWriteDur = d
	if len(b.recentWrites) == sixelProbeStabilityWindow {
		copy(b.recentWrites, b.recentWrites[1:])
		b.recentWrites[len(b.recentWrites)-1] = d
	} else {
		b.recentWrites = append(b.recentWrites, d)
	}
	if b.probePending && d >= b.probeBand() {
		b.retreatProbe()
		return
	}
	if b.escalationSkips > 0 {
		b.escalationSkips--
		return
	}
	if d+tmuxSixelWriteMargin < b.floor() {
		b.slowStreak = 0
		return
	}
	b.slowStreak++
	if b.slowStreak < sixelSlowWriteStreak {
		return
	}
	b.slowStreak = 0
	if b.idx < len(sixelDownscaleLadder)-1 {
		b.idx++
		b.sinceProbe = 0
		b.escalationSkips = sixelSlowWriteStreak
		b.probeCooldown = sixelProbeCooldown
	}
}

// retreatProbe fails the pending sharper-probe trial: the ladder steps
// straight back to the pre-probe index (never sharper than where the
// byte-budget loop has meanwhile settled) and probing switches off for the
// rest of the motion episode, because the ladder just watched that sharper
// factor fail and retrying it during sustained motion buys one saturation
// ramp per retry. The sample skip is armed too: the writes that follow
// measure backlog encoded at the failed sharper factor.
func (b *sixelBudget) retreatProbe() {
	b.probePending = false
	b.probesExhausted = true
	b.idx = max(b.idx, b.preProbeIdx)
	b.sinceProbe = 0
	b.slowStreak = 0
	b.escalationSkips = sixelSlowWriteStreak
}

// probeWindowStable reports whether the recent-write window clears the
// sharper probe's stability gate: every recorded write sits inside the probe
// band and, once the window is full, the writes do not rise
// monotonically. Steady-state jitter (18, 20, 19 ms) passes; a monotonic
// ramp (69, 83, 90 ms) means tmux is buffering content the pipe cannot
// sustain, so a sharper probe would tip the accumulated queue. An
// incomplete window passes the trend check, matching the fresh-state
// probe-free-to-fire behaviour. The same check judges a pending trial's
// survival in renderSixelBudgeted once the cadence has elapsed.
func (b *sixelBudget) probeWindowStable() bool {
	band := b.probeBand()
	for _, d := range b.recentWrites {
		if d >= band {
			return false
		}
	}
	if len(b.recentWrites) < sixelProbeStabilityWindow {
		return true
	}
	for i := 1; i < len(b.recentWrites); i++ {
		if b.recentWrites[i] <= b.recentWrites[i-1] {
			return true
		}
	}
	return false
}

// renderSixelBudgeted encodes one Sixel frame under the byte budget: it
// encodes at the remembered ladder factor, escalates the factor until the
// output fits, and sends the smallest encode anyway when even the ladder
// floor exceeds the budget. A factor above 1 downscales the source and
// upscales it back to the exact input extent before encoding, so the DCS
// raster W;H is unchanged and the page keeps covering the full grid; the
// round trip acts as a blur that shortens the encoded output on noisy
// content. The first budgeted frame after a settle emit opens a new motion
// episode: the ladder resumes at the index the settle stepped down from, so
// resuming motion does not re-run the discovery cascade, and probing is
// freshly enabled behind the base cool-down. Every sixelProbeInterval frames
// it retries one step weaker so quality recovers when content calms down,
// but only while the episode has not exhausted its probes, only outside the
// post-escalation cool-down, and only when the stability gate passes: the
// last sixelProbeStabilityWindow recorded writes all inside the probe band
// with no monotonic rise, so a pipe that drained only because the ladder
// stepped, or one already ramping toward saturation, is not probed straight
// back into it. The fired probe is a judged trial: recordWrite retreats it
// on any band breach and silences probing until the next settle, while
// survival needs both the elapsed cadence and a stable window (writes flat
// or falling, all inside the band) and keeps probing enabled. A window
// still rising at the cadence keeps the trial pending; it either stabilises
// later (survival) or crosses the band (retreat), so no trial-length cap is
// needed. The whole frame lands in the reused scratch buffer first, so the
// caller sees one write.
func (s *State) renderSixelBudgeted(img image.Image, w io.Writer) error {
	b := &s.sixelBudget
	if b.resumeIdx > 0 {
		b.idx = max(b.idx, b.resumeIdx)
		b.resumeIdx = 0
		b.probeCooldown = sixelProbeCooldown
	}
	b.sinceProbe++
	if b.probePending && b.sinceProbe >= sixelProbeInterval && b.probeWindowStable() {
		b.probePending = false
	}
	if b.probeCooldown > 0 {
		b.probeCooldown--
	} else if !b.probesExhausted && b.idx > 0 && b.sinceProbe >= sixelProbeInterval &&
		b.lastWriteDur < b.probeBand() && b.probeWindowStable() {
		b.preProbeIdx = b.idx
		b.idx--
		b.sinceProbe = 0
		b.probePending = true
	}
	buf := &b.buf
	for {
		buf.Reset()
		if err := s.sixelEnc.Render(render.Frame{Img: s.sixelAtFactor(img, b.idx)}, buf); err != nil {
			return err
		}
		if buf.Len() <= b.limit || b.idx == len(sixelDownscaleLadder)-1 {
			break
		}
		b.idx++
		b.sinceProbe = 0
		b.escalationSkips = sixelSlowWriteStreak
	}
	b.lastImg = img
	if b.debug {
		fmt.Fprintf(os.Stderr, "nyala timing: sixel frame_bytes=%d downscale_factor=%g\n",
			buf.Len(), sixelDownscaleLadder[b.idx])
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// sixelSettleNeeded reports whether the idle settle-to-sharp emit is due:
// budgeting is armed, the last budgeted emit was blurred (ladder factor
// above 1), and its source is retained for the re-encode.
func (s *State) sixelSettleNeeded() bool {
	b := &s.sixelBudget
	return b.limit > 0 && b.idx > 0 && b.lastImg != nil
}

// renderSixelSettle re-encodes the retained last budgeted frame at ladder
// factor 1 and writes it, so an idle page settles to crisp text. It ends the
// motion episode: the pre-settle ladder index is remembered in resumeIdx so
// the next episode resumes at the sustained factor, the rest of the ladder
// state resets (probing re-enabled), and the retained source drops, so the
// settle cannot re-fire until a new budgeted frame goes out. The encode
// bypasses the byte budget deliberately: the settle is a one-shot
// full-quality emit inside the pacing gate.
func (s *State) renderSixelSettle(w io.Writer) error {
	b := &s.sixelBudget
	img := b.lastImg
	b.lastImg = nil
	b.resumeIdx = b.idx
	b.idx = 0
	b.sinceProbe = 0
	b.slowStreak = 0
	b.escalationSkips = 0
	b.probeCooldown = 0
	b.probePending = false
	b.probesExhausted = false
	b.preProbeIdx = 0
	b.recentWrites = b.recentWrites[:0]
	buf := &b.buf
	buf.Reset()
	if err := s.sixelEnc.Render(render.Frame{Img: img}, buf); err != nil {
		return err
	}
	if b.debug {
		fmt.Fprintf(os.Stderr, "nyala timing: sixel settle frame_bytes=%d downscale_factor=1\n", buf.Len())
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// sixelAtFactor returns img blurred by one ladder factor: area-average down
// by the factor, then nearest-fill back up to the exact input extent, both
// through the shared block scaler. Index 0 returns img untouched. The two
// destinations are reused across frames, so steady state allocates nothing.
func (s *State) sixelAtFactor(img image.Image, idx int) image.Image {
	if idx == 0 {
		return img
	}
	f := sixelDownscaleLadder[idx]
	b := img.Bounds()
	sw := max(int(float64(b.Dx())/f), 1)
	sh := max(int(float64(b.Dy())/f), 1)
	s.ensureScratchPool()
	s.sixelBudget.small = reuseRGBA(s.sixelBudget.small, sw, sh)
	s.scratch.downscale.DownscaleInto(s.sixelBudget.small, img, s.scratch.pool)
	s.sixelBudget.fitted = reuseRGBA(s.sixelBudget.fitted, b.Dx(), b.Dy())
	return s.scratch.downscale.DownscaleInto(s.sixelBudget.fitted, s.sixelBudget.small, s.scratch.pool)
}

// reuseRGBA returns cur when it already spans w x h, or a fresh RGBA image
// otherwise. The scale kernels fully overwrite every destination pixel, so a
// reused image carries no stale content.
func reuseRGBA(cur *image.RGBA, w, h int) *image.RGBA {
	if cur != nil && cur.Bounds().Dx() == w && cur.Bounds().Dy() == h {
		return cur
	}
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

// parseTmuxVersion extracts the major and minor version from a tmux XTVERSION
// identity such as "tmux 3.5a". A trailing patch letter is ignored. It
// tolerates a missing or foreign identity by reporting ok false.
func parseTmuxVersion(termID string) (major, minor int, ok bool) {
	fields := strings.Fields(termID)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "tmux") {
		return 0, 0, false
	}
	majorPart, minorPart, found := strings.Cut(fields[1], ".")
	if !found {
		return 0, 0, false
	}
	major, err := strconv.Atoi(leadingDigits(majorPart))
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(leadingDigits(minorPart))
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// leadingDigits returns the leading decimal-digit run of s, so "5a" parses as
// 5 while a digit-free token stays unparseable.
func leadingDigits(s string) string {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}

// resolveTmuxSixelMinInterval returns the version-tuned tmux Sixel pacing
// floor: tmuxSixelMinInterval (250 ms) on tmux 3.7 and later,
// tmuxSixelMinIntervalLegacy (300 ms) below 3.7 or when the version is
// unknown. Live tmux 3.6a at a settled factor 3 kept its pty writes fully
// drained (~20 ms) at the 250 ms floor yet showed periodic display stalls
// followed by a rapid release of several frames: the backlog sits in 3.6a's
// output scheduler toward the host terminal, invisible to the write-duration
// signal, and the same run at 300 ms showed no bursts.
func resolveTmuxSixelMinInterval(termID string) time.Duration {
	major, minor, ok := parseTmuxVersion(termID)
	if ok && (major > 3 || (major == 3 && minor >= 7)) {
		return tmuxSixelMinInterval
	}
	return tmuxSixelMinIntervalLegacy
}

// resolveTmuxSixelBudget returns the per-frame Sixel byte budget under tmux.
// On tmux 3.6 and later it raises the input-buffer-size server option to
// 16 MiB through run. A successful set returns a version-tuned budget: the
// raised full-quality budget on 3.7 and later, where rate pacing is all
// tmux's parse and re-encode pipeline needs (live 3.7b evidence: ~4 MB
// full-quality frames sustained indefinitely at the tmuxSixelMinInterval
// pacing), and the sixelBudgetLegacyRate cap on 3.6.x, whose output stage
// toward the host terminal saturates at higher byte rates with no
// write-duration signal to see it. The raise still runs on 3.6.x: it
// protects ladder-floor frames that overshoot the cap, which the fit loop
// sends anyway. Where the set fails the 900 KB fallback keeps a margin under
// tmux's default 1 MiB input buffer, whose overflow discards the DCS
// silently; that parser cap is then the tighter bound. The pre-3.6 branch is
// defensive only: the version gate (tmuxSixelSupported) refuses Sixel there,
// so configure never arms a budget for it. The option is never restored: it
// persists for the tmux server lifetime.
func resolveTmuxSixelBudget(termID string, run func(args ...string) error) int {
	major, minor, ok := parseTmuxVersion(termID)
	if !ok || major < 3 || (major == 3 && minor < 6) {
		return sixelBudgetDefault
	}
	if run("set", "-s", "input-buffer-size", tmuxInputBufferSize) != nil {
		return sixelBudgetDefault
	}
	if major > 3 || minor >= 7 {
		return sixelBudgetRaised
	}
	return sixelBudgetLegacyRate
}

// runTmux runs one tmux client command. The binary runs inside tmux, so the
// client reaches the hosting server socket through the TMUX environment.
func runTmux(args ...string) error {
	return exec.CommandContext(context.Background(), "tmux", args...).Run()
}
