package viewer

import (
	"bytes"
	"image"
	"image/color"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/palette"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// greyTestState returns a Block state in the grey colour mode carrying the
// given OSC 4 palette-query verdict. Unlike paletteTestState it fits nothing in
// advance: the luma ramp engine fits on the first grey frame, which is the
// production order, and the tests read that first frame. Production sets the
// verdict once before any frame, so a test that needs both verdicts builds one
// state per verdict.
func greyTestState(paletteOK bool) *State {
	state := NewState(20, 11)
	st := &state
	st.Backend = BackendBlock
	st.Colour = ColourXTermGrey
	st.paletteQueryOK = paletteOK
	st.SetDiff(&block.GridDiff{})
	return st
}

// greyTestImage builds a neutral luma gradient sized to the state's page grid,
// rising left to right and top to bottom, so the composed cells carry many
// distinct grey levels and the ramp engine fits a ramp that differs from the
// fixed xterm ramp. Every channel carries the same value, so the luma of a
// pixel is that value.
func greyTestImage(t *testing.T, st *State) *image.RGBA {
	t.Helper()
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))
	if pxW <= 0 || pxH <= 0 {
		t.Fatalf("degenerate capture extent %dx%d", pxW, pxH)
	}
	img := image.NewRGBA(image.Rect(0, 0, pxW, pxH))
	for y := range pxH {
		for x := range pxW {
			// 4..194 leaves room for the drift shifts below without saturation.
			v := uint8(4 + x*150/max(pxW-1, 1) + y*40/max(pxH-1, 1)) //nolint:gosec // The divisions bound the value to 194.
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 0xff})
		}
	}
	return img
}

// greyShiftImage returns a copy of src with every channel raised by delta
// inside region, saturating at 255. A grey source stays grey, so the luma of
// every pixel in region moves by exactly delta.
func greyShiftImage(src *image.RGBA, region image.Rectangle, delta uint8) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			c := dst.RGBAAt(x, y)
			c.R = uint8(min(int(c.R)+int(delta), 255)) //nolint:gosec // Clamped to 255.
			c.G = uint8(min(int(c.G)+int(delta), 255)) //nolint:gosec // Clamped to 255.
			c.B = uint8(min(int(c.B)+int(delta), 255)) //nolint:gosec // Clamped to 255.
			dst.SetRGBA(x, y, c)
		}
	}
	return dst
}

// greySGRIndexes returns every palette index the payload names through a
// 256-colour foreground or background SGR, in payload order. A prefix that is
// not followed by digits and the SGR terminator fails the test, because it
// would mean the writer, not the parser, is wrong.
func greySGRIndexes(t *testing.T, payload string) []int {
	t.Helper()
	var indexes []int
	for _, prefix := range []string{fg256Prefix, bg256Prefix} {
		for rest := payload; ; {
			start := strings.Index(rest, prefix)
			if start < 0 {
				break
			}
			body := rest[start+len(prefix):]
			end := strings.IndexByte(body, 'm')
			if end < 0 {
				t.Fatalf("SGR %q has no terminator", prefix)
			}
			idx, err := strconv.Atoi(body[:end])
			if err != nil {
				t.Fatalf("SGR index %q: %v", body[:end], err)
			}
			indexes = append(indexes, idx)
			rest = body[end+1:]
		}
	}
	if len(indexes) == 0 {
		t.Fatal("the payload names no 256-colour register, so the index check proves nothing")
	}
	return indexes
}

// greyOSC4Run returns the base register and the entry count of the palette
// the payload wrote across every OSC 4 sequence. The registers must form one
// contiguous ascending run, because the emitter numbers them from the engine
// base; any other layout fails the test.
func greyOSC4Run(t *testing.T, payload string) (base, count int) {
	t.Helper()
	for rest := payload; ; {
		start := strings.Index(rest, osc4Introducer)
		if start < 0 {
			break
		}
		body := rest[start+len(osc4Introducer):]
		end := strings.Index(body, "\x1b\\")
		if end < 0 {
			t.Fatal("OSC 4 sequence has no ST terminator")
		}
		fields := strings.Split(body[:end], ";")
		if len(fields)%2 != 0 {
			t.Fatalf("OSC 4 body has %d fields, want an even count", len(fields))
		}
		for i := 0; i < len(fields); i += 2 {
			reg, err := strconv.Atoi(fields[i])
			if err != nil {
				t.Fatalf("OSC 4 register %q: %v", fields[i], err)
			}
			if count == 0 {
				base = reg
			} else if reg != base+count {
				t.Fatalf("OSC 4 register %d is not contiguous from base %d", reg, base)
			}
			count++
		}
		rest = body[end+len("\x1b\\"):]
	}
	if count == 0 {
		t.Fatal("the payload wrote no OSC 4 palette")
	}
	return base, count
}

// greyWantPaletteChunks returns the number of OSC 4 sequences the emitter
// writes for the ramp the state's luma engine holds now.
func greyWantPaletteChunks(t *testing.T, st *State) int {
	t.Helper()
	entries := len(st.greyEngine.Palette)
	if entries == 0 {
		t.Fatal("the luma ramp engine holds no ramp, so the sequence count proves nothing")
	}
	return (entries + palette.MaxOSC4Entries - 1) / palette.MaxOSC4Entries
}

// TestRenderBlockGreyWritesTheLumaRampOnTheFirstFrame proves the grey mode on a
// terminal that answered the OSC 4 probe writes the fitted luma ramp ahead of
// the first frame's cells, and that every register the cells name lies inside
// the budget range and inside the run of registers that write covered. A cell
// naming a register outside the written run would draw the terminal's stale
// value for it.
func TestRenderBlockGreyWritesTheLumaRampOnTheFirstFrame(t *testing.T) {
	st := greyTestState(true)
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: greyTestImage(t, st)}, &out); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	got := out.String()

	if n, want := strings.Count(got, osc4Introducer), greyWantPaletteChunks(t, st); n != want {
		t.Fatalf("the first grey frame wrote %d OSC 4 sequences, want exactly %d", n, want)
	}
	if !strings.HasPrefix(got, osc4Introducer) {
		t.Fatal("the first grey frame does not open with the palette write")
	}
	base, count := greyOSC4Run(t, got)
	if base < palette.BudgetBase || base+count-1 > 255 {
		t.Fatalf("the ramp covers registers %d..%d, want a run inside %d..255", base, base+count-1, palette.BudgetBase)
	}
	if count != len(st.greyEngine.Palette) {
		t.Fatalf("the payload wrote %d registers, the engine holds %d ramp entries", count, len(st.greyEngine.Palette))
	}
	for _, idx := range greySGRIndexes(t, got) {
		if idx < base || idx >= base+count {
			t.Fatalf("a cell names register %d, outside the written run %d..%d", idx, base, base+count-1)
		}
	}
}

// TestRenderBlockGreyUnansweredProbeKeepsTheFixedRamp proves the grey mode on a
// terminal that answered no OSC 4 probe writes no palette and no restore, names
// only the fixed xterm greyscale registers 232..255, and produces the same bytes
// as a second state built the same way, so --color=grey output on such a
// terminal is unchanged by the ramp engine.
func TestRenderBlockGreyUnansweredProbeKeepsTheFixedRamp(t *testing.T) {
	st := greyTestState(false)
	src := greyTestImage(t, st)
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &out); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	got := out.String()

	if got == "" {
		t.Fatal("the frame wrote no bytes")
	}
	if strings.Contains(got, "\x1b]4") {
		t.Fatal("an unanswered probe wrote an OSC 4 palette")
	}
	if strings.Contains(got, "\x1b]104") {
		t.Fatal("an unanswered probe wrote an OSC 104 restore")
	}
	for _, idx := range greySGRIndexes(t, got) {
		if idx < 232 || idx > 255 {
			t.Fatalf("a cell names register %d, want the fixed ramp 232..255", idx)
		}
	}
	if st.paletteDisturbed() {
		t.Fatal("an unanswered probe recorded a palette write")
	}

	again := greyTestState(false)
	var second bytes.Buffer
	if err := again.RenderStreamFrame(render.Frame{Img: src}, &second); err != nil {
		t.Fatalf("RenderStreamFrame for the second state: %v", err)
	}
	if !bytes.Equal(out.Bytes(), second.Bytes()) {
		t.Fatal("two states built the same way wrote different grey frames")
	}
}

// TestRenderBlockGreyReusedRampWritesNoPaletteBytes proves the ramp engine's
// reuse verdict, not a frame counter, gates the grey palette write: a repeated
// frame that keeps the retained ramp writes no palette bytes and, because no
// cell changed, no bytes at all.
func TestRenderBlockGreyReusedRampWritesNoPaletteBytes(t *testing.T) {
	st := greyTestState(true)
	src := greyTestImage(t, st)

	var first bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &first); err != nil {
		t.Fatalf("RenderStreamFrame for the first frame: %v", err)
	}
	if !strings.Contains(first.String(), osc4Introducer) {
		t.Fatal("the first frame wrote no palette, so the reuse test proves nothing")
	}

	var second bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &second); err != nil {
		t.Fatalf("RenderStreamFrame for the repeated frame: %v", err)
	}
	if strings.Contains(second.String(), osc4Introducer) {
		t.Fatal("a reused-ramp frame wrote palette bytes")
	}
	if second.Len() != 0 {
		t.Fatalf("a reused-ramp frame wrote %d bytes, want 0", second.Len())
	}
	if st.greyEngine.RequantCount != 1 {
		t.Fatalf("RequantCount = %d after a repeated frame, want 1", st.greyEngine.RequantCount)
	}
}

// TestRenderBlockGreySmallLumaDriftReusesTheRamp proves a small luma shift keeps
// the retained ramp: a +1 shift over the whole frame, then a +2 shift on one
// region, each write no palette bytes after the first frame. A ramp that
// re-fitted on such motion would rewrite every register and repaint every cell
// on every scroll or fade.
func TestRenderBlockGreySmallLumaDriftReusesTheRamp(t *testing.T) {
	st := greyTestState(true)
	src := greyTestImage(t, st)
	whole := greyShiftImage(src, src.Bounds(), 1)
	region := greyShiftImage(src, image.Rect(8, 8, 20, 20), 2)

	var first bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &first); err != nil {
		t.Fatalf("RenderStreamFrame for the first frame: %v", err)
	}
	if !strings.Contains(first.String(), osc4Introducer) {
		t.Fatal("the first frame wrote no palette, so the drift test proves nothing")
	}

	for _, tc := range []struct {
		name string
		img  *image.RGBA
	}{
		{"+1 over the frame", whole},
		{"+2 on a region", region},
	} {
		var out bytes.Buffer
		if err := st.RenderStreamFrame(render.Frame{Img: tc.img}, &out); err != nil {
			t.Fatalf("RenderStreamFrame for %s: %v", tc.name, err)
		}
		if strings.Contains(out.String(), osc4Introducer) {
			t.Fatalf("the %s frame wrote palette bytes", tc.name)
		}
		if st.greyEngine.RequantCount != 1 {
			t.Fatalf("RequantCount = %d after the %s frame, want 1 (must reuse the ramp)", st.greyEngine.RequantCount, tc.name)
		}
	}
}

// TestRecomposeBlockRestoresThePaletteOnAGreyColourCycle proves a colour cycle
// between 256 and grey on a terminal that answered the OSC 4 probe restores the
// palette in both directions, because the registers hold the old mode's meaning
// and the new mode's engine has fitted nothing yet, and that a scheme cycle
// inside grey keeps them, because the next frame reuses the retained ramp.
func TestRecomposeBlockRestoresThePaletteOnAGreyColourCycle(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)

	st := greyTestState(true)
	src := greyTestImage(t, st)
	if err := st.RenderStreamFrame(render.Frame{Img: src}, io.Discard); err != nil {
		t.Fatalf("RenderStreamFrame for the grey frame: %v", err)
	}
	if !st.paletteDisturbed() {
		t.Fatal("the grey frame wrote no palette, so the cycle test proves nothing")
	}

	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: st, tty: out, ctl: newControl(eng, done)}
	s.firstDrawn.Store(true)
	s.recomposeBlock(true)
	if got := out.String(); strings.Contains(got, palette.OSC104) {
		t.Fatalf("a scheme cycle inside the grey mode restored the palette: %q", got)
	}

	out.Reset()
	st.Colour = ColourXTerm256
	s.recomposeBlock(true)
	if got := out.String(); !strings.Contains(got, palette.OSC104) {
		t.Fatalf("a cycle from grey to 256 = %q, missing %q", got, palette.OSC104)
	}
	if again := st.releasePalette(); again != "" {
		t.Fatalf("a second release after the grey to 256 cycle = %q, want no restore", again)
	}

	// The 256 frame after the cycle must write its own palette, because the
	// release dropped the retained ramp and the colour engine has fitted nothing.
	var colour bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &colour); err != nil {
		t.Fatalf("RenderStreamFrame for the 256 frame: %v", err)
	}
	if !strings.Contains(colour.String(), osc4Introducer) {
		t.Fatal("the first 256 frame after the cycle wrote no palette")
	}

	out.Reset()
	st.Colour = ColourXTermGrey
	s.recomposeBlock(true)
	if got := out.String(); !strings.Contains(got, palette.OSC104) {
		t.Fatalf("a cycle from 256 to grey = %q, missing %q", got, palette.OSC104)
	}
	if again := st.releasePalette(); again != "" {
		t.Fatalf("a second release after the 256 to grey cycle = %q, want no restore", again)
	}
	if !st.paletteDisturbed() {
		t.Fatal("the cycle cleared the record the teardown reads")
	}

	// The grey frame after the cycle re-fits and rewrites the ramp.
	var grey bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &grey); err != nil {
		t.Fatalf("RenderStreamFrame for the grey frame after the cycle: %v", err)
	}
	if !strings.Contains(grey.String(), osc4Introducer) {
		t.Fatal("the first grey frame after the cycle wrote no palette")
	}
	if st.greyEngine.RequantCount != 2 {
		t.Fatalf("RequantCount = %d after the cycle back to grey, want 2", st.greyEngine.RequantCount)
	}
}
