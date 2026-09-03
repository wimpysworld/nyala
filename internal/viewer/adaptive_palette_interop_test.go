//go:build blockinterop

package viewer

import (
	"bytes"
	"image"
	"image/color"
	"io"
	"testing"

	"github.com/wimpysworld/nyala/internal/blockinterop"
	"github.com/wimpysworld/nyala/internal/palette"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// adaptivePaletteInteropState returns a Block state in the adaptive mode whose
// engine holds no palette yet. The engine must quantise for the first time
// inside the frame path, because a palette built outside it would name
// registers the stream never wrote, which no production run can produce.
func adaptivePaletteInteropState(t *testing.T) *State {
	t.Helper()
	state := NewState(20, 11)
	st := &state
	st.Backend = BackendBlock
	st.Colour = ColourXTerm256Adaptive
	st.SetDiff(&block.GridDiff{})
	return st
}

// adaptiveInteropFrame renders one frame and returns its stream bytes together
// with the grid the terminal must show: the exact grid the writer consumed,
// with every cell colour resolved through the palette the engine holds after
// the frame. That resolution is the writer's own rule, so the compare tests the
// pairing of registers with cells rather than the choice of registers.
func adaptiveInteropFrame(t *testing.T, st *State, src image.Image) ([]byte, block.Grid) {
	t.Helper()
	var out bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: src}, &out); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	eng := st.adaptivePalette()
	composed := st.scratch.out
	intended := block.Grid{Cols: composed.Cols, Rows: composed.Rows, Cells: make([]block.Cell, len(composed.Cells))}
	for i, cell := range composed.Cells {
		intended.Cells[i] = cell
		intended.Cells[i].FG = adaptiveInteropEntry(t, eng, cell.FG)
		intended.Cells[i].BG = adaptiveInteropEntry(t, eng, cell.BG)
	}
	return out.Bytes(), intended
}

// adaptiveInteropEntry returns the palette entry the engine's table names for a
// composed cell colour, which is the colour the terminal shows once it applies
// the registers the same frame wrote.
func adaptiveInteropEntry(t *testing.T, eng *palette.Engine, c color.RGBA) color.RGBA {
	t.Helper()
	entry := int(block.PaletteIndex(eng.LUT, c)) - eng.Base()
	if entry < 0 || entry >= len(eng.Palette) {
		t.Fatalf("colour %v maps to entry %d, outside the %d-entry palette", c, entry, len(eng.Palette))
	}
	r, g, b, _ := eng.Palette[entry].RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff} //nolint:gosec // RGBA reports 16-bit channels, so the shift bounds each to 255.
}

// adaptiveInteropRewrite feeds one frame's bytes through the tracker, appends
// the resolved bytes to the running stream, and returns the palette events the
// frame carried. One tracker across every frame is what a terminal holds: the
// registers a frame writes stay in force until another frame changes them.
func adaptiveInteropRewrite(t *testing.T, tracker *blockinterop.PaletteTracker, stream *bytes.Buffer, payload []byte) []blockinterop.PaletteEvent {
	t.Helper()
	rewritten, events, err := tracker.Rewrite(payload)
	if err != nil {
		t.Fatalf("PaletteTracker.Rewrite: %v", err)
	}
	stream.Write(rewritten)
	return events
}

// adaptiveInteropDecode decodes the resolved stream so far and returns the
// visible grid, which holds the colour every cell shows under the registers in
// force where that cell was written.
func adaptiveInteropDecode(t *testing.T, st *State, stream *bytes.Buffer) block.Grid {
	t.Helper()
	grid, err := blockinterop.Decode(stream.Bytes(), st.Cols, engineRows(st.Rows))
	if err != nil {
		t.Fatalf("blockinterop.Decode: %v", err)
	}
	return grid
}

// adaptiveInteropWrite asserts that a frame carried the chunked OSC 4 palette
// write as one contiguous run of sequences whose registers stay inside the
// budget range, and returns that run folded into one event. The fold gives the
// span the negative control moves and the entries in register order, so a
// chunked write and a single write make the same assertions.
func adaptiveInteropWrite(t *testing.T, events []blockinterop.PaletteEvent, minEntries int) blockinterop.PaletteEvent {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("the frame carried no palette sequence, want a palette write")
	}
	folded := blockinterop.PaletteEvent{Offset: events[0].Offset}
	for i, event := range events {
		if event.Reset {
			t.Fatal("the frame carried a palette restore, want a palette write")
		}
		if len(event.Entries) > palette.MaxOSC4Entries {
			t.Fatalf("palette sequence %d set %d registers, want at most %d", i, len(event.Entries), palette.MaxOSC4Entries)
		}
		if want := folded.Offset + folded.Length; event.Offset != want {
			t.Fatalf("palette sequence %d starts at offset %d, want %d, so the chunks are not contiguous", i, event.Offset, want)
		}
		folded.Length += event.Length
		folded.Entries = append(folded.Entries, event.Entries...)
	}
	if want := (len(folded.Entries) + palette.MaxOSC4Entries - 1) / palette.MaxOSC4Entries; len(events) != want {
		t.Fatalf("the frame carried %d palette sequences for %d registers, want %d", len(events), len(folded.Entries), want)
	}
	if len(folded.Entries) < minEntries {
		t.Fatalf("the palette write set %d registers, want at least %d", len(folded.Entries), minEntries)
	}
	for i, entry := range folded.Entries {
		if want := palette.BudgetBase + i; entry.Index != want {
			t.Fatalf("palette entry %d names register %d, want %d", i, entry.Index, want)
		}
		if entry.Index > 255 {
			t.Fatalf("palette entry %d names register %d, outside the 0..255 range", i, entry.Index)
		}
	}
	return folded
}

// adaptiveInteropMatches reports whether the decoded grid and the intended grid
// agree on every visible cell. It is the same compare assertVisibleGridEqual
// makes, returned rather than fatal, so the negative control can require a
// mismatch.
func adaptiveInteropMatches(got, want block.Grid) bool {
	if got.Cols != want.Cols || got.Rows != want.Rows {
		return false
	}
	for row := range want.Rows {
		for col := range want.Cols {
			g, w := got.At(col, row), want.At(col, row)
			if g.Wide != w.Wide || g.FG != w.FG || g.BG != w.BG || visibleText(g) != visibleText(w) {
				return false
			}
		}
	}
	return true
}

// TestBlockTerminalRoundTripAdaptivePaletteAcrossRequantise is the frame
// consistency proof. It drives three adaptive Block frames through the
// production writer, decodes the stream with a decoder that tracks the terminal
// palette, and checks the colour every cell shows against the colour the
// composed frame intended, after each frame. A cell resolves through the
// registers in force where the writer emitted it, so a palette that reaches the
// terminal after the cells it belongs to, or a repaint that leaves a cell
// holding a renumbered register, fails the compare.
//
// The third frame repeats the second, so the engine reuses its palette. That
// frame must contribute no palette bytes at all.
func TestBlockTerminalRoundTripAdaptivePaletteAcrossRequantise(t *testing.T) {
	st := adaptivePaletteInteropState(t)
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))
	tracker := blockinterop.NewPaletteTracker()
	var stream, raw bytes.Buffer

	// The flat frame quantises onto a near-zero mapping error, so the gradient
	// that follows drifts far past the reuse threshold and must re-quantise.
	flatBytes, flatIntended := adaptiveInteropFrame(t, st, paletteSolidImage(pxW, pxH, color.RGBA{R: 8, G: 8, B: 40, A: 0xff}))
	adaptiveInteropWrite(t, adaptiveInteropRewrite(t, tracker, &stream, flatBytes), 1)
	raw.Write(flatBytes)
	assertVisibleGridEqual(t, adaptiveInteropDecode(t, st, &stream), flatIntended)

	gradientBytes, gradientIntended := adaptiveInteropFrame(t, st, paletteTestImage(pxW, pxH))
	write := adaptiveInteropWrite(t, adaptiveInteropRewrite(t, tracker, &stream, gradientBytes), 2)
	raw.Write(gradientBytes)
	assertVisibleGridEqual(t, adaptiveInteropDecode(t, st, &stream), gradientIntended)

	// A decoder that assumes fixed register meanings reads the same stream as a
	// different frame, so the palette-tracking decode above is what carries the
	// proof and no assertion here can be met by the old decoder.
	fixed, err := blockinterop.Decode(raw.Bytes(), st.Cols, engineRows(st.Rows))
	if err != nil {
		t.Fatalf("blockinterop.Decode without palette tracking: %v", err)
	}
	if adaptiveInteropMatches(fixed, gradientIntended) {
		t.Fatal("a fixed-table decode matched the frame, so the palette-tracking decode proves nothing")
	}

	repeatBytes, repeatIntended := adaptiveInteropFrame(t, st, paletteTestImage(pxW, pxH))
	if events := adaptiveInteropRewrite(t, tracker, &stream, repeatBytes); len(events) != 0 {
		t.Fatalf("the reused-palette frame carried %d palette sequences, want 0", len(events))
	}
	if len(repeatBytes) != 0 {
		t.Fatalf("the reused-palette frame wrote %d bytes, want 0", len(repeatBytes))
	}
	assertVisibleGridEqual(t, adaptiveInteropDecode(t, st, &stream), repeatIntended)

	// The negative control. Moving the re-quantisation's palette write to the end
	// of its own frame leaves every cell of that frame resolving through the
	// previous frame's registers, which is what an unpaired write produces on a
	// terminal. The check must reject it, or it proves nothing above.
	moved := make([]byte, 0, len(gradientBytes))
	moved = append(moved, gradientBytes[:write.Offset]...)
	moved = append(moved, gradientBytes[write.Offset+write.Length:]...)
	moved = append(moved, gradientBytes[write.Offset:write.Offset+write.Length]...)
	unpaired := blockinterop.NewPaletteTracker()
	var unpairedStream bytes.Buffer
	adaptiveInteropRewrite(t, unpaired, &unpairedStream, flatBytes)
	adaptiveInteropRewrite(t, unpaired, &unpairedStream, moved)
	if adaptiveInteropMatches(adaptiveInteropDecode(t, st, &unpairedStream), gradientIntended) {
		t.Fatal("a palette write moved after its own cells still matched the frame, so the consistency check is inert")
	}
}

// TestBlockTerminalRoundTripAdaptivePaletteRestoreOnExit proves the teardown
// hands the terminal back its own theme. The restore must be a bare OSC 104,
// because a run cannot know which registers the user configured, and a run that
// wrote no register must write no restore.
func TestBlockTerminalRoundTripAdaptivePaletteRestoreOnExit(t *testing.T) {
	tracker := blockinterop.NewPaletteTracker()
	if _, _, err := tracker.Rewrite(palette.AppendOSC4(nil, color.Palette{color.RGBA{R: 0xff, A: 0xff}}, palette.BudgetBase)); err != nil {
		t.Fatalf("PaletteTracker.Rewrite the palette write: %v", err)
	}

	_, events, err := tracker.Rewrite([]byte(exitTerminal(24, true, false, true, true)))
	if err != nil {
		t.Fatalf("PaletteTracker.Rewrite the teardown: %v", err)
	}
	if len(events) != 1 || !events[0].Bare() {
		t.Fatalf("the teardown carried %+v, want one bare OSC 104", events)
	}
	if got, want := tracker.Entry(palette.BudgetBase), block.XTerm256RGBA(palette.BudgetBase); got != want {
		t.Fatalf("register %d after the teardown = %v, want the terminal default %v", palette.BudgetBase, got, want)
	}

	if _, events, err = tracker.Rewrite([]byte(exitTerminal(24, true, false, true, false))); err != nil {
		t.Fatalf("PaletteTracker.Rewrite the undisturbed teardown: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a teardown that disturbed no register carried %+v, want no palette sequence", events)
	}
}

// TestBlockTerminalRoundTripAdaptivePaletteRestoreOnBackendSwitch proves a
// backend switch away from an adaptive Block stint hands the registers back in
// the same control buffer as the switch, and that the restore is bare. No later
// Block frame writes those registers, so a switch that kept them would leave
// the next backend and the shell drawing in the frame's colours.
func TestBlockTerminalRoundTripAdaptivePaletteRestoreOnBackendSwitch(t *testing.T) {
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)

	st := adaptivePaletteInteropState(t)
	st.SetEnabled([]Backend{BackendBlock, BackendSixel})
	pxW, pxH := schemePixels(st.Scheme, st.Cols, engineRows(st.Rows))
	var frame bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: paletteTestImage(pxW, pxH)}, &frame); err != nil {
		t.Fatalf("RenderStreamFrame: %v", err)
	}
	tracker := blockinterop.NewPaletteTracker()
	_, events, err := tracker.Rewrite(frame.Bytes())
	if err != nil {
		t.Fatalf("PaletteTracker.Rewrite the Block frame: %v", err)
	}
	adaptiveInteropWrite(t, events, 1)

	out := &bytes.Buffer{}
	session := &streamSession{eng: eng, st: st, tty: out, ctl: newControl(eng, done)}
	session.firstDrawn.Store(true)
	session.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend == BackendBlock {
		t.Fatal("the cycle stayed on Block, so the switch is untested")
	}

	if _, events, err = tracker.Rewrite(out.Bytes()); err != nil {
		t.Fatalf("PaletteTracker.Rewrite the switch buffer: %v", err)
	}
	if len(events) != 1 || !events[0].Bare() {
		t.Fatalf("the switch buffer carried %+v, want one bare OSC 104", events)
	}
	if got, want := tracker.Entry(palette.BudgetBase), block.XTerm256RGBA(palette.BudgetBase); got != want {
		t.Fatalf("register %d after the switch = %v, want the terminal default %v", palette.BudgetBase, got, want)
	}

	// A stint that wrote no register must leave the user's theme alone.
	plain := adaptivePaletteInteropState(t)
	plain.Colour = ColourXTerm256
	plain.SetEnabled([]Backend{BackendBlock, BackendSixel})
	if err = plain.RenderStreamFrame(render.Frame{Img: paletteTestImage(pxW, pxH)}, io.Discard); err != nil {
		t.Fatalf("RenderStreamFrame in the static mode: %v", err)
	}
	static := &bytes.Buffer{}
	staticSession := &streamSession{eng: eng, st: plain, tty: static, ctl: newControl(eng, done)}
	staticSession.firstDrawn.Store(true)
	staticSession.handleKey(keyEvent{act: actionCycleBackend})
	if _, events, err = blockinterop.NewPaletteTracker().Rewrite(static.Bytes()); err != nil {
		t.Fatalf("PaletteTracker.Rewrite the static switch buffer: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a static 256-colour switch carried %+v, want no palette sequence", events)
	}
}
