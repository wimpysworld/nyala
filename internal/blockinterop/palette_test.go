package blockinterop

import (
	"image/color"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestDecodeWithPaletteResolvesAtWriteTime proves the tracker resolves a cell
// through the registers in force where the cell was written, not through the
// registers the stream ends with. Both cells name register 16; a second palette
// write between them redefines it, so a decoder with one fixed table would
// report the same colour twice.
func TestDecodeWithPaletteResolvesAtWriteTime(t *testing.T) {
	payload := []byte("\x1b]4;16;rgb:ff/00/00\x1b\\\x1b[1;1H\x1b[38;5;16;48;5;17mA" +
		"\x1b]4;16;rgb:00/00/ff\x1b\\\x1b[1;2H\x1b[38;5;16;48;5;17mB")
	grid, events, err := DecodeWithPalette(payload, 2, 1)
	if err != nil {
		t.Fatalf("DecodeWithPalette: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("decoded %d palette events, want 2", len(events))
	}
	want := []color.RGBA{{R: 0xff, A: 0xff}, {B: 0xff, A: 0xff}}
	for col, expected := range want {
		if got := grid.At(col, 0).FG; got != expected {
			t.Errorf("cell %d foreground = %v, want %v", col, got, expected)
		}
		if got, def := grid.At(col, 0).BG, block.XTerm256RGBA(17); got != def {
			t.Errorf("cell %d background = %v, want the untouched register %v", col, got, def)
		}
	}
}

// TestPaletteTrackerRestoresOnBareReset proves a bare OSC 104 returns every
// register to the reference table, which is the property the teardown and the
// backend switch depend on.
func TestPaletteTrackerRestoresOnBareReset(t *testing.T) {
	tracker := NewPaletteTracker()
	if _, _, err := tracker.Rewrite([]byte("\x1b]4;16;rgb:ff/00/00\x1b\\")); err != nil {
		t.Fatalf("Rewrite the palette write: %v", err)
	}
	if got := tracker.Entry(16); got != (color.RGBA{R: 0xff, A: 0xff}) {
		t.Fatalf("register 16 = %v, want the written colour", got)
	}
	_, events, err := tracker.Rewrite([]byte("\x1b]104\x1b\\"))
	if err != nil {
		t.Fatalf("Rewrite the restore: %v", err)
	}
	if len(events) != 1 || !events[0].Bare() {
		t.Fatalf("restore events = %+v, want one bare reset", events)
	}
	if got := tracker.Entry(16); got != block.XTerm256RGBA(16) {
		t.Fatalf("register 16 after the restore = %v, want %v", got, block.XTerm256RGBA(16))
	}
}

// TestPaletteTrackerReportsSequenceExtent proves Offset and Length locate the
// whole sequence, so a test can move a palette write and prove the frame
// consistency check rejects the result.
func TestPaletteTrackerReportsSequenceExtent(t *testing.T) {
	const write = "\x1b]4;16;rgb:ff/00/00\x1b\\"
	payload := []byte("\x1b[1;1H" + write + "A")
	_, events, err := NewPaletteTracker().Rewrite(payload)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("decoded %d palette events, want 1", len(events))
	}
	got := string(payload[events[0].Offset : events[0].Offset+events[0].Length])
	if got != write {
		t.Fatalf("event extent = %q, want %q", got, write)
	}
}

// TestPaletteTrackerPassesOtherSequencesThrough proves the rewrite touches the
// palette only: a truecolour cell, a title write, and a cursor move reach the
// emulator unchanged, so the existing round-trip decode still holds.
func TestPaletteTrackerPassesOtherSequencesThrough(t *testing.T) {
	payload := []byte("\x1b]0;nyala\x1b\\\x1b[1;1H\x1b[38;2;1;2;3;48;2;4;5;6mA")
	rewritten, events, err := NewPaletteTracker().Rewrite(payload)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("decoded %d palette events, want 0", len(events))
	}
	if string(rewritten) != string(payload) {
		t.Fatalf("rewritten stream = %q, want it unchanged", rewritten)
	}
}

// TestPaletteTrackerAcceptsFourDigitChannels proves the parser reads the wide
// channel form a terminal reports as well as the two-digit form the emitter
// writes, so a recorded reply decodes on the same path.
func TestPaletteTrackerAcceptsFourDigitChannels(t *testing.T) {
	tracker := NewPaletteTracker()
	if _, _, err := tracker.Rewrite([]byte("\x1b]4;20;rgb:ffff/0000/8080\x1b\\")); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if got, want := tracker.Entry(20), (color.RGBA{R: 0xff, G: 0x00, B: 0x80, A: 0xff}); got != want {
		t.Fatalf("register 20 = %v, want %v", got, want)
	}
}

// TestPaletteTrackerRejectsMalformedSequences proves the tracker fails loudly
// rather than decoding a stream it cannot model, because a silent skip would
// make a broken palette write look like a passing frame.
func TestPaletteTrackerRejectsMalformedSequences(t *testing.T) {
	cases := map[string]string{
		"odd field count":   "\x1b]4;16\x1b\\",
		"register too high": "\x1b]4;256;rgb:ff/00/00\x1b\\",
		"unknown spec form": "\x1b]4;16;#ff0000\x1b\\",
		"short channel run": "\x1b]4;16;rgb:ff/00\x1b\\",
		"unterminated OSC":  "\x1b]4;16;rgb:ff/00/00",
		"unterminated CSI":  "\x1b[38;5;16",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := NewPaletteTracker().Rewrite([]byte(payload)); err == nil {
				t.Fatalf("Rewrite(%q) returned nil error", payload)
			}
		})
	}
}

// TestPaletteTrackerDropsPaletteBytes proves the rewritten stream carries no
// palette control bytes, so the emulator never applies a palette write of its
// own on top of the resolved truecolour cells.
func TestPaletteTrackerDropsPaletteBytes(t *testing.T) {
	payload := []byte("\x1b]4;16;rgb:ff/00/00\x1b\\\x1b[38;5;16mA\x1b]104\x1b\\")
	rewritten, _, err := NewPaletteTracker().Rewrite(payload)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if strings.Contains(string(rewritten), "\x1b]4") || strings.Contains(string(rewritten), "\x1b]104") {
		t.Fatalf("rewritten stream = %q, want no palette control bytes", rewritten)
	}
	if !strings.Contains(string(rewritten), "\x1b[38;2;255;0;0m") {
		t.Fatalf("rewritten stream = %q, want the resolved truecolour cell", rewritten)
	}
}
