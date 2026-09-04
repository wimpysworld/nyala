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

// osc4Introducer opens an OSC 4 palette write. The tests count it to prove that
// one re-quantisation writes the chunked palette the emitter produces, not one
// sequence per entry and not a single oversized sequence.
const osc4Introducer = "\x1b]4;"

// wantPaletteChunks returns the number of OSC 4 sequences the emitter writes for
// the palette the state's engine holds now, which is one per palette.MaxOSC4Entries
// entries.
func wantPaletteChunks(t *testing.T, st *State) int {
	t.Helper()
	entries := len(st.adaptivePalette().Palette)
	if entries == 0 {
		t.Fatal("the engine holds no palette, so the sequence count proves nothing")
	}
	return (entries + palette.MaxOSC4Entries - 1) / palette.MaxOSC4Entries
}

// paletteTestImage builds a gradient the quantiser can split into many
// registers, so the engine's table differs from the static xterm-256 table.
func paletteTestImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(x * 255 / max(w-1, 1))   //nolint:gosec // the division bounds the value to 255.
			img.Pix[i+1] = uint8(y * 255 / max(h-1, 1)) //nolint:gosec // the division bounds the value to 255.
			img.Pix[i+2] = uint8((x + y) % 256)         //nolint:gosec // the modulus bounds the value to 255.
			img.Pix[i+3] = 0xff
		}
	}
	return img
}

// paletteSolidImage builds a flat image the quantiser collapses onto a near-zero
// mapping error, so a gradient frame that follows drifts far past the reuse
// threshold and must re-quantise.
func paletteSolidImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 0xff
	}
	return img
}

// paletteTwoToneImage adds a stable final source row to a flat frame.
func paletteTwoToneImage(w, h int, main, final color.RGBA) *image.RGBA {
	img := paletteSolidImage(w, h, main)
	for y := max(0, h-4); y < h; y++ {
		for x := range w {
			img.SetRGBA(x, y, final)
		}
	}
	return img
}

// paletteTestState returns a Block state in the 256 colour mode, carrying the
// given OSC 4 palette-query verdict, whose adaptive engine already holds a
// quantised palette, so a compose has a table to attach. Production sets the
// verdict once before any frame, so a test that needs both verdicts builds one
// state per verdict rather than flipping the field mid-run.
func paletteTestState(t *testing.T, paletteOK bool) (*State, []uint8) {
	t.Helper()
	state := NewState(20, 11)
	st := &state
	st.Backend = BackendBlock
	st.Colour = ColourXTerm256
	st.paletteQueryOK = paletteOK
	st.SetDiff(&block.GridDiff{})
	sample := paletteTestImage(48, 48)
	engine := st.adaptivePalette()
	engine.Requantise(sample.Bounds(), sample, palette.BudgetColours)
	if len(engine.LUT) == 0 {
		t.Fatal("engine holds no palette table after a requantisation")
	}
	return st, engine.LUT
}

// blockFrame returns a source image sized to the state's page grid, the extent
// the engine captures at for the Block path.
func blockFrame(t *testing.T, st *State) *image.RGBA {
	t.Helper()
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))
	if pxW <= 0 || pxH <= 0 {
		t.Fatalf("degenerate capture extent %dx%d", pxW, pxH)
	}
	return paletteTestImage(pxW, pxH)
}

// TestComposeBlockFrameAttachesAdaptivePalette proves the compose attaches the
// colour engine's table in the 256 mode and the luma ramp engine's table in the
// grey mode when the OSC 4 probe answered, and attaches none in truecolour or
// when the probe went unanswered, so only those combinations draw through a
// written palette.
func TestComposeBlockFrameAttachesAdaptivePalette(t *testing.T) {
	st, lut := paletteTestState(t, true)
	src := blockFrame(t, st)

	if st.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	got := st.blockRenderer.PaletteLUT
	if len(got) != len(lut) || &got[0] != &lut[0] {
		t.Fatal("adaptive compose attached a table other than the engine's")
	}

	st.Colour = ColourTrue
	if st.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	if st.blockRenderer.PaletteLUT != nil {
		t.Error("truecolour attached a palette table")
	}

	// The grey compose fits the luma ramp on this first grey frame, so the
	// attached table is the one the ramp engine holds after that fit.
	st.Colour = ColourXTermGrey
	if st.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	greyLUT := st.greyEngine.LUT
	if len(greyLUT) == 0 {
		t.Fatal("the grey compose left the luma ramp engine without a table")
	}
	got = st.blockRenderer.PaletteLUT
	if len(got) != len(greyLUT) || &got[0] != &greyLUT[0] {
		t.Fatal("grey compose attached a table other than the luma ramp engine's")
	}
	if &got[0] == &lut[0] {
		t.Fatal("grey compose attached the colour engine's table")
	}

	for _, mode := range []ColourMode{ColourXTerm256, ColourXTermGrey} {
		unanswered, _ := paletteTestState(t, false)
		unanswered.Colour = mode
		if unanswered.ComposeBlockFrame(render.Frame{Img: src}) == nil {
			t.Fatal("ComposeBlockFrame returned nil")
		}
		if unanswered.blockRenderer.PaletteLUT != nil {
			t.Errorf("mode %d attached a palette table on an unanswered probe", mode)
		}
	}
}

// TestComposeBlockFrameYCbCrAttachesTheSamePalette proves the native YCbCr
// compose entry carries the same table as the RGBA entry, so the Block draw and
// the diagnostics cannot diverge on which palette a frame used.
func TestComposeBlockFrameYCbCrAttachesTheSamePalette(t *testing.T) {
	st, lut := paletteTestState(t, true)

	if st.ComposeBlockFrame(render.Frame{Img: blockFrame(t, st)}) == nil {
		t.Fatal("ComposeBlockFrame returned nil for the RGBA frame")
	}
	fromRGBA := st.blockRenderer.PaletteLUT

	// Size the native source above the page-grid extent, which is the condition
	// ComposeBlockFrame takes the direct YCbCr entry on.
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))
	src := image.NewYCbCr(image.Rect(0, 0, pxW*2, pxH*2), image.YCbCrSubsampleRatio420)
	for i := range src.Y {
		src.Y[i] = uint8(i % 256) //nolint:gosec // the modulus bounds the value to 255.
	}
	if st.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil for the YCbCr frame")
	}
	fromYCbCr := st.blockRenderer.PaletteLUT

	if len(fromYCbCr) != len(lut) || &fromYCbCr[0] != &lut[0] || &fromYCbCr[0] != &fromRGBA[0] {
		t.Fatal("the two compose entries attached different palette tables")
	}
}

// TestBlockSGRMapsThroughTheAdaptivePalette proves the cell writers emit the
// registers the engine's table names once a compose attached it, and that the
// registers stay inside the budget range the terminal palette write covers.
func TestBlockSGRMapsThroughTheAdaptivePalette(t *testing.T) {
	st, lut := paletteTestState(t, true)
	if st.ComposeBlockFrame(render.Frame{Img: blockFrame(t, st)}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	fg, bg := st.blockSGR()

	adaptive := 0
	for _, c := range []color.RGBA{
		{A: 0xff},
		{R: 255, G: 255, B: 255, A: 0xff},
		{R: 12, G: 200, B: 91, A: 0xff},
		{R: 200, G: 7, B: 43, A: 0xff},
		{R: 40, G: 40, B: 210, A: 0xff},
	} {
		want := lut[uint16(c.R>>3)<<10|uint16(c.G>>3)<<5|uint16(c.B>>3)]
		if want < palette.BudgetBase {
			t.Fatalf("engine table names register %d, below the budget base", want)
		}
		if got, expect := string(fg(nil, c)), string(appendSGR256(nil, fg256Prefix, want)); got != expect {
			t.Errorf("foreground for %v = %q, want %q", c, got, expect)
		}
		if got, expect := string(bg(nil, c)), string(appendSGR256(nil, bg256Prefix, want)); got != expect {
			t.Errorf("background for %v = %q, want %q", c, got, expect)
		}
		if want != block.XTerm256(c) {
			adaptive++
		}
	}
	if adaptive == 0 {
		t.Fatal("the engine table named the static xterm-256 register every time, so the test proves nothing")
	}
}

// TestBlockSGRRebuildsOnAModeChange proves the cached writer pair follows the
// colour mode, so a mode change never writes the previous mode's registers. A
// cycle to grey on an answered probe maps through the luma ramp table the grey
// compose fitted; on an unanswered probe it maps through the fixed xterm ramp.
func TestBlockSGRRebuildsOnAModeChange(t *testing.T) {
	st, _ := paletteTestState(t, true)
	src := blockFrame(t, st)
	if st.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	c := color.RGBA{R: 12, G: 200, B: 91, A: 0xff}
	fg, _ := st.blockSGR()
	adaptive := string(fg(nil, c))

	st.Colour = ColourXTermGrey
	if st.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	fg, _ = st.blockSGR()
	grey := string(fg(nil, c))
	greyLUT := st.greyEngine.LUT
	if len(greyLUT) == 0 {
		t.Fatal("the grey compose left the luma ramp engine without a table")
	}
	register := block.PaletteIndex(greyLUT, c)
	if register < palette.BudgetBase {
		t.Fatalf("luma ramp table names register %d, below the budget base", register)
	}
	if want := string(appendSGR256(nil, fg256Prefix, register)); grey != want {
		t.Fatalf("grey foreground = %q, want %q", grey, want)
	}
	if adaptive == grey {
		t.Fatal("the adaptive and the grey writers produced the same register, so the rebuild is untested")
	}

	unanswered, _ := paletteTestState(t, false)
	unanswered.Colour = ColourXTermGrey
	if unanswered.ComposeBlockFrame(render.Frame{Img: src}) == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	fg, _ = unanswered.blockSGR()
	fixed := string(fg(nil, c))
	if want := string(appendSGR256(nil, fg256Prefix, block.XTermGrey(c))); fixed != want {
		t.Fatalf("grey foreground on an unanswered probe = %q, want %q", fixed, want)
	}
}

// TestRenderBlockXTerm256IgnoresTheEngine proves a quantised engine cannot leak
// into the 256 mode on an unanswered OSC 4 probe: the frame bytes match a state
// whose engine never quantised, so --color=256 output on such a terminal is
// unchanged.
func TestRenderBlockXTerm256IgnoresTheEngine(t *testing.T) {
	quantised, _ := paletteTestState(t, false)
	src := blockFrame(t, quantised)
	var withEngine bytes.Buffer
	if err := quantised.RenderStreamFrame(render.Frame{Img: src}, &withEngine); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}

	plainState := NewState(quantised.Cols, quantised.Rows)
	plain := &plainState
	plain.Backend = BackendBlock
	plain.Colour = ColourXTerm256
	plain.SetDiff(&block.GridDiff{})
	var withoutEngine bytes.Buffer
	if err := plain.RenderStreamFrame(render.Frame{Img: src}, &withoutEngine); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}

	if withEngine.Len() == 0 {
		t.Fatal("the frame wrote no bytes")
	}
	if !bytes.Equal(withEngine.Bytes(), withoutEngine.Bytes()) {
		t.Fatal("a quantised engine changed the --color=256 frame bytes")
	}
}

// TestRenderBlockPairsThePaletteWriteWithTheRepaint proves the frame-consistency
// invariant: a re-quantisation writes the chunked OSC 4 palette, then repaints
// every cell, in one ordered frame buffer with every palette byte ahead of every
// cell byte. A palette write that reached the terminal after the cells would
// draw the frame in the previous frame's colours.
func TestRenderBlockPairsThePaletteWriteWithTheRepaint(t *testing.T) {
	st, _ := paletteTestState(t, true)
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))

	flat := paletteSolidImage(pxW, pxH, color.RGBA{R: 8, G: 8, B: 40, A: 0xff})
	if err := st.RenderStreamFrame(render.Frame{Img: flat}, io.Discard); err != nil {
		t.Fatalf("RenderStreamFrame for the flat frame: %v", err)
	}
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: paletteTestImage(pxW, pxH)}, &out); err != nil {
		t.Fatalf("RenderStreamFrame for the gradient frame: %v", err)
	}

	got := out.String()
	if n, want := strings.Count(got, osc4Introducer), wantPaletteChunks(t, st); n != want {
		t.Fatalf("the re-quantised frame wrote %d OSC 4 sequences, want exactly %d", n, want)
	}
	if !strings.HasPrefix(got, osc4Introducer) {
		t.Fatal("the frame does not open with the palette write")
	}
	// The chunks are contiguous, so the last one ends the whole palette write.
	last := strings.LastIndex(got, osc4Introducer)
	terminator := strings.Index(got[last:], "\x1b\\")
	if terminator < 0 {
		t.Fatal("the last palette sequence carries no ST terminator")
	}
	end := last + terminator
	if cell := strings.Index(got, fg256Prefix); cell < 0 || cell < end {
		t.Fatalf("the first cell colour is at %d, want it after the palette write ending at %d", cell, end)
	}
	// A re-quantisation renumbers every register, so no retained cell still
	// matches the terminal and every row must repaint. A full repaint emits the
	// first cell of each row, which costs one CUP at column one.
	for row := 1; row <= engineRows(st.Rows); row++ {
		if cup := "\x1b[" + strconv.Itoa(row) + ";1H"; !strings.Contains(got, cup) {
			t.Fatalf("row %d did not repaint after the re-quantisation", row)
		}
	}
}

// TestRenderBlockReusedPaletteWritesNoPaletteBytes proves the engine's reuse
// verdict, not a frame counter, gates the palette write: a frame the drift test
// lets keep the retained palette writes no palette bytes at all.
func TestRenderBlockReusedPaletteWritesNoPaletteBytes(t *testing.T) {
	st, _ := paletteTestState(t, true)
	src := blockFrame(t, st)

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
		t.Fatal("a reused-palette frame wrote palette bytes")
	}
	// The repeated frame changes no cell either, so the reuse also kept the
	// differ's retained grid rather than forcing a repaint.
	if second.Len() != 0 {
		t.Fatalf("a reused-palette frame wrote %d bytes, want 0", second.Len())
	}
}

// TestRenderBlockExactPaletteRefitKeepsTheCellDiff proves that an exact refit
// emits no palette bytes and still emits a changed cell through the retained
// differ.
func TestRenderBlockExactPaletteRefitKeepsTheCellDiff(t *testing.T) {
	state := NewState(20, 11)
	st := &state
	st.Backend = BackendBlock
	st.Colour = ColourXTerm256
	st.paletteQueryOK = true
	st.SetDiff(&block.GridDiff{})
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))
	stable := color.RGBA{R: 80, G: 80, B: 80, A: 0xff}
	src := paletteTwoToneImage(pxW, pxH, color.RGBA{R: 40, G: 40, B: 40, A: 0xff}, stable)
	foreground := color.RGBA{R: 23, G: 67, B: 149, A: 0xff}
	first := render.Frame{Img: src, Text: []render.GlyphRun{{Col: 0, Row: 0, Text: "A", FG: foreground}}}
	if err := st.RenderStreamFrame(first, io.Discard); err != nil {
		t.Fatalf("RenderStreamFrame for the first frame: %v", err)
	}

	eng := st.adaptivePalette()
	before := eng.RequantCount
	mode := st.paletteMode

	var out bytes.Buffer
	drift := paletteTwoToneImage(pxW, pxH, color.RGBA{R: 43, G: 43, B: 43, A: 0xff}, stable)
	changed := render.Frame{Img: drift, Text: []render.GlyphRun{{Col: 0, Row: 0, Text: "B", FG: foreground}}}
	if err := st.RenderStreamFrame(changed, &out); err != nil {
		t.Fatalf("RenderStreamFrame for the changed frame: %v", err)
	}
	if eng.RequantCount != before+1 {
		t.Fatalf("RequantCount = %d, want %d", eng.RequantCount, before+1)
	}
	if strings.Contains(out.String(), osc4Introducer) {
		t.Fatal("an exact palette refit wrote OSC 4 bytes")
	}
	if out.Len() == 0 {
		t.Fatal("the changed frame emitted no cell diff")
	}
	lastRow := "\x1b[" + strconv.Itoa(engineRows(st.Rows)) + ";1H"
	if strings.Contains(out.String(), lastRow) {
		t.Fatal("the exact palette refit reset the differ and repainted the stable final row")
	}
	if st.paletteMode != mode {
		t.Fatalf("paletteMode = %d after the exact refit, want %d", st.paletteMode, mode)
	}
}

// TestPublishPaletteRefitWritesEveryNonExactResult proves that each equality
// difference publishes the complete palette through the common publication path.
func TestPublishPaletteRefitWritesEveryNonExactResult(t *testing.T) {
	colours := color.Palette{
		color.RGBA{R: 1, G: 2, B: 3, A: 0xff},
		color.RGBA{R: 4, G: 5, B: 6, A: 0xff},
	}
	want := palette.AppendOSC4(nil, colours, palette.BudgetBase)
	lut := make([]uint8, 1<<15)
	cases := map[string]palette.RefitResult{
		"palette": {PriorValid: true, LengthEqual: true, LUTEqual: true, ChangedEntries: 1},
		"length":  {PriorValid: true, ChangedEntries: 1},
		"LUT":     {PriorValid: true, LengthEqual: true, PaletteEqual: true},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			state := NewState(2, 3)
			st := &state
			st.Colour = ColourXTerm256
			st.paletteQueryOK = true
			st.publishPaletteRefit(result, lut, colours, palette.BudgetBase)

			if !bytes.Equal(st.paletteOSC4, want) {
				t.Fatal("the refit did not publish the complete palette")
			}
			if st.paletteMode != ColourXTerm256 {
				t.Fatalf("paletteMode = %d, want %d", st.paletteMode, ColourXTerm256)
			}
			if !sameTable(st.sgrLUT, lut) {
				t.Fatal("the refit did not refresh the indexed SGR table")
			}
		})
	}
}

// TestExitTerminalRestoresTheAdaptivePalette proves the teardown pairs every
// OSC 4 write with the bare OSC 104 restore, and leaves the user's theme alone
// in a run that never wrote a register. exitTerminalWithTitle forwards the flag,
// so the restore reaches every exit path, including the fatal-signal teardown.
func TestExitTerminalRestoresTheAdaptivePalette(t *testing.T) {
	if got := exitTerminal(24, true, false, true, true); !strings.Contains(got, palette.OSC104) {
		t.Fatalf("teardown after a palette write = %q, missing %q", got, palette.OSC104)
	}
	if got := exitTerminal(24, true, false, true, false); strings.Contains(got, palette.OSC104) {
		t.Fatalf("teardown without a palette write = %q, must not restore the palette", got)
	}
	if got := exitTerminalWithTitle(24, true, false, true, true, false); !strings.Contains(got, palette.OSC104) {
		t.Fatalf("exitTerminalWithTitle after a palette write = %q, missing %q", got, palette.OSC104)
	}
}

// TestTabSwitchRestoresTheAdaptivePalette proves a backend switch away from a
// 256 Block stint on a terminal that answered the OSC 4 probe restores the
// terminal palette in the same control buffer as the switch clear, after the
// erase so no cell is on screen while the registers change, and that a stint on
// an unanswered probe wrote no register and so writes no restore.
func TestTabSwitchRestoresTheAdaptivePalette(t *testing.T) {
	for _, tc := range []struct {
		name     string
		adaptive bool
	}{
		{"probe answered", true},
		{"probe unanswered", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &switchOpEngine{}
			done := make(chan struct{})
			defer close(done)

			st, _ := paletteTestState(t, tc.adaptive)
			st.SetEnabled([]Backend{BackendBlock, BackendSixel})
			st.Backend = BackendBlock
			if err := st.RenderStreamFrame(render.Frame{Img: blockFrame(t, st)}, io.Discard); err != nil {
				t.Fatalf("RenderStreamFrame: %v", err)
			}
			adaptive := tc.adaptive
			if got := st.paletteDisturbed(); got != adaptive {
				t.Fatalf("palette written after a Block frame = %v, want %v", got, adaptive)
			}

			out := &bytes.Buffer{}
			s := &streamSession{eng: eng, st: st, tty: out, ctl: newControl(eng, done)}
			s.firstDrawn.Store(true)
			s.handleKey(keyEvent{act: actionCycleBackend})
			if st.Backend == BackendBlock {
				t.Fatal("the cycle stayed on Block, so the switch is untested")
			}

			got := out.String()
			if strings.Contains(got, palette.OSC104) != adaptive {
				t.Fatalf("switch buffer = %q, restore present = %v, want %v", got, !adaptive, adaptive)
			}
			if !adaptive {
				return
			}
			if strings.Index(got, palette.OSC104) < strings.Index(got, clearScreen) {
				t.Fatalf("the palette restore precedes the erase: %q", got)
			}
			if again := st.releasePalette(); again != "" {
				t.Fatalf("a second release after the switch = %q, want no restore", again)
			}
			if !st.paletteDisturbed() {
				t.Fatal("the switch cleared the record the teardown reads")
			}
		})
	}
}

// TestRecomposeBlockRestoresTheAdaptivePaletteOnAColourCycle proves a colour
// cycle from 256 to grey on a terminal that answered the OSC 4 probe restores
// the palette, because no later frame writes those registers, and that a cycle
// staying in 256 keeps them, because the next frame reuses the retained palette
// and writes none.
func TestRecomposeBlockRestoresTheAdaptivePaletteOnAColourCycle(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)

	st, _ := paletteTestState(t, true)
	st.Backend = BackendBlock
	if err := st.RenderStreamFrame(render.Frame{Img: blockFrame(t, st)}, io.Discard); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}

	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: st, tty: out, ctl: newControl(eng, done)}
	s.firstDrawn.Store(true)
	s.recomposeBlock(true)
	if got := out.String(); strings.Contains(got, palette.OSC104) {
		t.Fatalf("a scheme cycle inside the 256 mode restored the palette: %q", got)
	}

	out.Reset()
	st.Colour = ColourXTermGrey
	s.recomposeBlock(true)
	got := out.String()
	if !strings.Contains(got, palette.OSC104) {
		t.Fatalf("a cycle from 256 to grey = %q, missing %q", got, palette.OSC104)
	}
	if again := st.releasePalette(); again != "" {
		t.Fatalf("a second release after the cycle = %q, want no restore", again)
	}
	if !st.paletteDisturbed() {
		t.Fatal("the cycle cleared the record the teardown reads")
	}
}
