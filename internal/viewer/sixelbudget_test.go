package viewer

import (
	"bytes"
	"errors"
	"image"
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
	// A flat frame fits at any factor; pin the state as if noise had settled
	// the ladder at its floor, one frame short of the probe.
	flat := image.NewRGBA(image.Rect(0, 0, 640, 464))
	for i := range flat.Pix {
		flat.Pix[i] = 0xff
	}
	st.sixelBudget.idx = len(sixelDownscaleLadder) - 1
	st.sixelBudget.sinceProbe = sixelProbeInterval - 1

	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, &out); err != nil {
		t.Fatalf("RenderStreamFrame(probe frame) = %v", err)
	}
	if got, want := st.sixelBudget.idx, len(sixelDownscaleLadder)-2; got != want {
		t.Fatalf("probe frame ladder index = %d, want the one-step-weaker %d", got, want)
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

// TestResolveTmuxSixelBudget pins the resolution through a seamed command
// runner: no shell-out happens in tests, the budget is the 900 KB throughput
// cap on every version, and the version gate only controls whether the
// input-buffer-size safety-net raise runs (3.6 and later run it, older and
// unknown versions do not, and its failure is tolerated silently).
func TestResolveTmuxSixelBudget(t *testing.T) {
	cases := []struct {
		name    string
		termID  string
		runErr  error
		want    int
		wantRun bool
	}{
		{"3.5a keeps the budget without running tmux", "tmux 3.5a", nil, sixelBudgetDefault, false},
		{"3.6 raises the option, budget unchanged", "tmux 3.6", nil, sixelBudgetDefault, true},
		{"3.7b raises the option, budget unchanged", "tmux 3.7b", nil, sixelBudgetDefault, true},
		{"unknown version keeps the budget without running tmux", "", nil, sixelBudgetDefault, false},
		{"failed option set keeps the budget", "tmux 3.6", errTmuxRun, sixelBudgetDefault, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			run := func(args ...string) error {
				gotArgs = args
				return tc.runErr
			}
			if got := resolveTmuxSixelBudget(tc.termID, run); got != tc.want {
				t.Fatalf("budget = %d, want %d", got, tc.want)
			}
			if ran := gotArgs != nil; ran != tc.wantRun {
				t.Fatalf("tmux command ran = %v, want %v", ran, tc.wantRun)
			}
			if tc.wantRun {
				want := []string{"set", "-s", "input-buffer-size", tmuxInputBufferSize}
				if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
					t.Fatalf("tmux command args = %v, want %v", gotArgs, want)
				}
			}
		})
	}
}

// errTmuxRun stands in for a failed tmux client invocation.
var errTmuxRun = errors.New("tmux set failed")

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
	keys <- keyEvent{b: 'q'}

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
