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

	"github.com/wimpysworld/nyala/internal/render"
)

// sixelBudgetDefault is the per-frame Sixel byte budget under tmux, on every
// tmux version. It is a throughput cap for tmux's parse and re-encode
// pipeline, not merely a parser-acceptance cap: live tmux 3.7b measurements
// showed ~4 MB frames blocking each pty write for hundreds of milliseconds
// (223 ms mean, 410 ms max, ~3 fps). It also keeps a safety margin under
// tmux's default 1 MiB input buffer, whose overflow discards the DCS
// silently.
const sixelBudgetDefault = 900_000

// tmuxInputBufferSize is the value handed to `tmux set -s input-buffer-size`
// on tmux 3.6 and later: 16 MiB. The raise is a safety net against silent DCS
// discard for the occasional frame that overshoots the budget (a ladder-floor
// overshoot or a probe frame); it never changes the budget itself.
const tmuxInputBufferSize = "16777216"

// debugTimingEnv is the live timing control the budgeted Sixel path reads at
// configure time: set to any non-empty value, each budgeted frame reports its
// encoded byte size and active downscale factor on stderr. It is the same
// NYALA_DEBUG_TIMING switch the engine's load-milestone trace uses.
const debugTimingEnv = "NYALA_DEBUG_TIMING"

// sixelProbeInterval is how many budgeted frames pass before the encoder
// probes one ladder step weaker, so quality recovers once content calms down
// while steady state pays no double encode.
const sixelProbeInterval = 60

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

	sinceProbe int // budgeted frames since the last weaker-quality probe

	buf    bytes.Buffer // reused whole-frame encode buffer the budget measures
	small  *image.RGBA  // reused downscale destination
	fitted *image.RGBA  // reused upscale-back destination

	// debug arms the per-frame stderr trace of the encoded byte size and the
	// active factor. It follows NYALA_DEBUG_TIMING, the existing live timing
	// control, resolved once at configure time.
	debug bool
}

// renderSixelBudgeted encodes one Sixel frame under the byte budget: it
// encodes at the remembered ladder factor, escalates the factor until the
// output fits, and sends the smallest encode anyway when even the ladder
// floor exceeds the budget. A factor above 1 downscales the source and
// upscales it back to the exact input extent before encoding, so the DCS
// raster W;H is unchanged and the page keeps covering the full grid; the
// round trip acts as a blur that shortens the encoded output on noisy
// content. Every sixelProbeInterval frames it retries one step weaker so
// quality recovers when content calms down. The whole frame lands in the
// reused scratch buffer first, so the caller sees one write.
func (s *State) renderSixelBudgeted(img image.Image, w io.Writer) error {
	b := &s.sixelBudget
	b.sinceProbe++
	if b.idx > 0 && b.sinceProbe >= sixelProbeInterval {
		b.idx--
		b.sinceProbe = 0
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
	}
	if b.debug {
		fmt.Fprintf(os.Stderr, "nyala timing: sixel frame_bytes=%d downscale_factor=%g\n",
			buf.Len(), sixelDownscaleLadder[b.idx])
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

// resolveTmuxSixelBudget returns the per-frame Sixel byte budget under tmux:
// always sixelBudgetDefault, on every version, because the budget is a
// throughput cap for tmux's parse and re-encode pipeline. On tmux 3.6 and
// later it also raises the input-buffer-size server option to 16 MiB through
// run, as a safety net against silent DCS discard for overshoot frames; the
// call's failure is tolerated silently and never changes the budget. The
// option is never restored: it persists for the tmux server lifetime.
func resolveTmuxSixelBudget(termID string, run func(args ...string) error) int {
	major, minor, ok := parseTmuxVersion(termID)
	if ok && (major > 3 || (major == 3 && minor >= 6)) {
		_ = run("set", "-s", "input-buffer-size", tmuxInputBufferSize)
	}
	return sixelBudgetDefault
}

// runTmux runs one tmux client command. The binary runs inside tmux, so the
// client reaches the hosting server socket through the TMUX environment.
func runTmux(args ...string) error {
	return exec.CommandContext(context.Background(), "tmux", args...).Run()
}
