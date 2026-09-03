package palette

import (
	"bytes"
	"image/color"
	"strings"
	"testing"
)

// testPalette returns a palette of n entries whose channels vary per entry, so
// a wrong index or a wrong channel order shows in the emitted bytes.
func testPalette(n int) color.Palette {
	entries := make(color.Palette, n)
	for i := range entries {
		entries[i] = color.RGBA{R: uint8(i), G: uint8(i + 1), B: uint8(i + 2), A: 0xff} //nolint:gosec // Deliberate 8-bit wrap for test data.
	}
	return entries
}

// TestAppendOSC4ChunksTheEntries proves the chunked form: N entries need
// ceil(N/MaxOSC4Entries) introducers, each with its own ST terminator, and
// every entry appears exactly once.
func TestAppendOSC4ChunksTheEntries(t *testing.T) {
	for _, n := range []int{1, 2, 7, 8, 16, 240, 256} {
		got := string(AppendOSC4(nil, testPalette(n), 0))
		want := (n + MaxOSC4Entries - 1) / MaxOSC4Entries
		if introducers := strings.Count(got, osc4Introducer); introducers != want {
			t.Errorf("%d entries: got %d introducers, want %d", n, introducers, want)
		}
		if terminators := strings.Count(got, "\x1b\\"); terminators != want {
			t.Errorf("%d entries: got %d terminators, want %d", n, terminators, want)
		}
		if pairs := strings.Count(got, ";rgb:"); pairs != n {
			t.Errorf("%d entries: got %d specs, want %d", n, pairs, n)
		}
	}
}

// TestAppendOSC4SequenceParameterCount proves the reason the emitter chunks at
// all: no sequence carries more than MaxOSC4Entries entries, and every sequence
// carries an odd parameter count no greater than fifteen. The vte parser behind
// Alacritty and Rio truncates at sixteen parameters and then drops a sequence
// whose count is even, so either breach loses the whole palette on those
// terminals.
func TestAppendOSC4SequenceParameterCount(t *testing.T) {
	for _, n := range []int{1, 6, 7, 8, 15, 240} {
		for _, sequence := range splitOSC4(t, string(AppendOSC4(nil, testPalette(n), 16))) {
			params := strings.Count(sequence, ";") + 1
			if entries := strings.Count(sequence, ";rgb:"); entries > MaxOSC4Entries {
				t.Errorf("%d entries: sequence %q carries %d entries, want at most %d", n, sequence, entries, MaxOSC4Entries)
			}
			if params%2 == 0 || params > 15 {
				t.Errorf("%d entries: sequence %q carries %d parameters, want an odd count of at most 15", n, sequence, params)
			}
		}
	}
}

// TestAppendOSC4ShortFinalChunk pins the chunk boundary for a nine-entry
// palette at base 16: a full sequence of MaxOSC4Entries entries, then a short
// one holding the remainder.
func TestAppendOSC4ShortFinalChunk(t *testing.T) {
	want := "\x1b]4;16;rgb:00/01/02;17;rgb:01/02/03;18;rgb:02/03/04;19;rgb:03/04/05;20;rgb:04/05/06;21;rgb:05/06/07;22;rgb:06/07/08\x1b\\" +
		"\x1b]4;23;rgb:07/08/09;24;rgb:08/09/0a\x1b\\"
	if got := string(AppendOSC4(nil, testPalette(9), 16)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// splitOSC4 returns the palette-set sequences in payload, terminators dropped,
// and fails the test on a sequence that no ST terminates.
func splitOSC4(t *testing.T, payload string) []string {
	t.Helper()
	var out []string
	for _, part := range strings.Split(payload, osc4Introducer) {
		if part == "" {
			continue
		}
		body, found := strings.CutSuffix(part, "\x1b\\")
		if !found {
			t.Fatalf("sequence %q has no ST terminator", part)
		}
		out = append(out, body)
	}
	return out
}

// TestAppendOSC4Bytes pins the exact sequence for a known two-entry palette at
// index base 16: the spec form is "rgb:RR/GG/BB", two lower-case hex digits per
// channel, and the terminator is ST, never BEL.
func TestAppendOSC4Bytes(t *testing.T) {
	entries := color.Palette{
		color.RGBA{R: 0x00, G: 0x11, B: 0x22, A: 0xff},
		color.RGBA{R: 0xab, G: 0xcd, B: 0xef, A: 0xff},
	}
	want := "\x1b]4;16;rgb:00/11/22;17;rgb:ab/cd/ef\x1b\\"
	if got := string(AppendOSC4(nil, entries, 16)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAppendOSC4AppendsToTheCallerBuffer proves the helper extends the given
// slice rather than replacing it, so the frame path can fold the write into one
// control buffer.
func TestAppendOSC4AppendsToTheCallerBuffer(t *testing.T) {
	entries := color.Palette{color.RGBA{R: 0x01, G: 0x02, B: 0x03, A: 0xff}}
	want := "prefix\x1b]4;0;rgb:01/02/03\x1b\\"
	if got := string(AppendOSC4([]byte("prefix"), entries, 0)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAppendOSC4EmptyPaletteEmitsNothing proves a frame that reuses its palette
// writes no palette bytes, not an empty OSC sequence.
func TestAppendOSC4EmptyPaletteEmitsNothing(t *testing.T) {
	for name, entries := range map[string]color.Palette{"nil": nil, "empty": {}} {
		if got := AppendOSC4(nil, entries, 16); len(got) != 0 {
			t.Errorf("%s palette: got %q, want no bytes", name, got)
		}
		if got := string(AppendOSC4([]byte("prefix"), entries, 16)); got != "prefix" {
			t.Errorf("%s palette: got %q, want the buffer unchanged", name, got)
		}
	}
}

// TestAppendOSC4IndexBase proves entry i sets terminal index base+i, so the
// budget mode's base of 16 reserves indexes 0..15 and the Sixel base of 0 does
// not.
func TestAppendOSC4IndexBase(t *testing.T) {
	tests := []struct {
		base int
		want string
	}{
		{0, "\x1b]4;0;rgb:00/01/02;1;rgb:01/02/03;2;rgb:02/03/04\x1b\\"},
		{16, "\x1b]4;16;rgb:00/01/02;17;rgb:01/02/03;18;rgb:02/03/04\x1b\\"},
		{253, "\x1b]4;253;rgb:00/01/02;254;rgb:01/02/03;255;rgb:02/03/04\x1b\\"},
	}
	for _, test := range tests {
		if got := string(AppendOSC4(nil, testPalette(3), test.base)); got != test.want {
			t.Errorf("base %d: got %q, want %q", test.base, got, test.want)
		}
	}
}

// TestAppendOSC4TopBudgetIndex proves a 240-entry palette based at 16 ends at
// terminal index 255, the top of the range.
func TestAppendOSC4TopBudgetIndex(t *testing.T) {
	got := AppendOSC4(nil, testPalette(240), 16)
	if want := ";16;rgb:"; !bytes.Contains(got, []byte(want)) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
	if want := ";255;rgb:"; !bytes.Contains(got, []byte(want)) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
	if bytes.Contains(got, []byte(";256;")) {
		t.Error("got an index above 255")
	}
}

// TestAppendOSC4DoesNotAllocate proves the append form allocates nothing when
// the caller's buffer has the capacity, which is what the frame path relies on.
func TestAppendOSC4DoesNotAllocate(t *testing.T) {
	entries := testPalette(240)
	buffer := make([]byte, 0, len(AppendOSC4(nil, entries, 16)))
	if got := testing.AllocsPerRun(100, func() { _ = AppendOSC4(buffer[:0], entries, 16) }); got != 0 {
		t.Errorf("got %v allocations, want 0", got)
	}
}

// TestOSC104Bytes pins the bare reset: no parameters, so it restores every
// palette entry, and ST terminates it.
func TestOSC104Bytes(t *testing.T) {
	if want := "\x1b]104\x1b\\"; OSC104 != want {
		t.Errorf("got %q, want %q", OSC104, want)
	}
}
