package blockinterop

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// PaletteEntry is one terminal register a palette control sequence redefined,
// with the colour that register holds after the sequence.
type PaletteEntry struct {
	// Index is the terminal register number, 0..255.
	Index int
	// Colour is the value the register holds after the sequence.
	Colour color.RGBA
}

// PaletteEvent is one palette control sequence found in a Block stream. Offset
// and Length locate the sequence in the payload the tracker read, so a test can
// move the sequence and prove that the consistency check rejects the result.
// Reset marks an OSC 104 restore; every other event is an OSC 4 write.
type PaletteEvent struct {
	// Offset is the byte offset of the introducer in the payload.
	Offset int
	// Length is the byte length of the whole sequence, terminator included.
	Length int
	// Reset reports whether the sequence is an OSC 104 restore.
	Reset bool
	// Entries are the registers the sequence redefined, in stream order. A bare
	// OSC 104 names none, because it restores every register at once.
	Entries []PaletteEntry
}

// Bare reports whether the event is a bare OSC 104 restore, which names no
// register and therefore returns the whole palette to the user's theme.
func (e PaletteEvent) Bare() bool { return e.Reset && len(e.Entries) == 0 }

// PaletteTracker models the terminal's 256 palette registers across a Block
// stream. A terminal resolves an indexed SGR colour through the registers it
// holds when the cell is drawn, so a decoder that assumes fixed register
// meanings cannot tell a palette paired with its own frame from a palette
// paired with the previous one. The tracker applies every OSC 4 write and
// OSC 104 restore in stream order and rewrites each indexed SGR colour into the
// truecolour form the register named at that point, which is what the terminal
// puts on screen. The rewritten stream then decodes through Decode unchanged.
//
// One tracker carries state across payloads, so a caller can feed frames in one
// at a time and keep the terminal model a real terminal would hold.
type PaletteTracker struct {
	entries [256]color.RGBA
}

// NewPaletteTracker returns a tracker seeded with the canonical xterm-256
// table, the reference model for a terminal that received no palette write.
func NewPaletteTracker() *PaletteTracker {
	tracker := &PaletteTracker{}
	tracker.restore()
	return tracker
}

// Entry returns the colour terminal register index holds now. It panics on an
// index outside 0..255, which no valid stream produces.
func (t *PaletteTracker) Entry(index int) color.RGBA { return t.entries[index] }

// Rewrite consumes payload, applies every palette control sequence it carries,
// and returns the payload with the palette sequences removed and every indexed
// SGR colour replaced by the truecolour value its register named at that point
// in the stream. It also returns the palette events in stream order, one per
// sequence, so a caller can assert the chunked run of OSC 4 sequences a frame
// wrote, or that the frame wrote none at all.
// A malformed palette sequence or SGR colour is an error, never a silent skip.
func (t *PaletteTracker) Rewrite(payload []byte) ([]byte, []PaletteEvent, error) {
	out := make([]byte, 0, len(payload))
	var events []PaletteEvent
	for i := 0; i < len(payload); {
		if payload[i] != 0x1b || i+1 >= len(payload) {
			out = append(out, payload[i])
			i++
			continue
		}
		switch payload[i+1] {
		case ']':
			body, end, err := oscBody(payload, i)
			if err != nil {
				return nil, nil, err
			}
			event, palette, err := t.apply(body)
			if err != nil {
				return nil, nil, fmt.Errorf("blockinterop: palette sequence at offset %d: %w", i, err)
			}
			if !palette {
				out = append(out, payload[i:end]...)
				i = end
				continue
			}
			event.Offset, event.Length = i, end-i
			events = append(events, event)
			i = end
		case '[':
			end, final := csiEnd(payload, i)
			if end < 0 {
				return nil, nil, fmt.Errorf("blockinterop: unterminated CSI sequence at offset %d", i)
			}
			if final != 'm' {
				out = append(out, payload[i:end]...)
				i = end
				continue
			}
			body, err := t.rewriteSGR(string(payload[i+2 : end-1]))
			if err != nil {
				return nil, nil, fmt.Errorf("blockinterop: SGR sequence at offset %d: %w", i, err)
			}
			out = append(out, "\x1b["...)
			out = append(out, body...)
			out = append(out, 'm')
			i = end
		default:
			out = append(out, payload[i], payload[i+1])
			i += 2
		}
	}
	return out, events, nil
}

// restore returns every register to the canonical xterm-256 value, the effect a
// bare OSC 104 has on a terminal whose theme is the reference palette.
func (t *PaletteTracker) restore() {
	for i := range t.entries {
		t.entries[i] = block.XTerm256RGBA(uint8(i)) //nolint:gosec // the array bounds the index to 255.
	}
}

// apply applies one OSC body to the registers and reports whether the body was
// a palette sequence at all. A body that is not OSC 4 or OSC 104 passes through
// untouched, so a title or a progress write reaches the emulator as it was.
func (t *PaletteTracker) apply(body string) (PaletteEvent, bool, error) {
	switch {
	case strings.HasPrefix(body, "4;"):
		fields := strings.Split(body[len("4;"):], ";")
		if len(fields)%2 != 0 {
			return PaletteEvent{}, true, fmt.Errorf("OSC 4 carries %d fields, want index and spec pairs", len(fields))
		}
		event := PaletteEvent{Entries: make([]PaletteEntry, 0, len(fields)/2)}
		for i := 0; i < len(fields); i += 2 {
			index, err := paletteIndex(fields[i])
			if err != nil {
				return PaletteEvent{}, true, err
			}
			colour, err := parseXColour(fields[i+1])
			if err != nil {
				return PaletteEvent{}, true, err
			}
			t.entries[index] = colour
			event.Entries = append(event.Entries, PaletteEntry{Index: index, Colour: colour})
		}
		return event, true, nil
	case body == "104":
		t.restore()
		return PaletteEvent{Reset: true}, true, nil
	case strings.HasPrefix(body, "104;"):
		event := PaletteEvent{Reset: true}
		for _, field := range strings.Split(body[len("104;"):], ";") {
			index, err := paletteIndex(field)
			if err != nil {
				return PaletteEvent{}, true, err
			}
			t.entries[index] = block.XTerm256RGBA(uint8(index)) //nolint:gosec // paletteIndex bounds the value to 255.
			event.Entries = append(event.Entries, PaletteEntry{Index: index, Colour: t.entries[index]})
		}
		return event, true, nil
	}
	return PaletteEvent{}, false, nil
}

// rewriteSGR returns the SGR parameter body with every "38;5;n" and "48;5;n"
// replaced by the truecolour parameters the register names now. Every other
// parameter passes through in place, so the emulator sees the same attributes.
func (t *PaletteTracker) rewriteSGR(body string) (string, error) {
	params := strings.Split(body, ";")
	out := make([]string, 0, len(params))
	for i := 0; i < len(params); i++ {
		if params[i] != "38" && params[i] != "48" {
			out = append(out, params[i])
			continue
		}
		// Reject a truncated indexed colour rather than copy it through. A
		// decoder that passes "38;5" on hands the emulator a malformed attribute,
		// and the consistency proof this package exists for would then rest on
		// input no terminal can read.
		if i+1 >= len(params) {
			return "", fmt.Errorf("SGR parameter %q ends without a colour selector", params[i])
		}
		if params[i+1] != "5" {
			out = append(out, params[i])
			continue
		}
		if i+2 >= len(params) {
			return "", fmt.Errorf("SGR parameters %q;5 end without a register", params[i])
		}
		index, err := paletteIndex(params[i+2])
		if err != nil {
			return "", err
		}
		colour := t.entries[index]
		out = append(out, params[i], "2", strconv.Itoa(int(colour.R)), strconv.Itoa(int(colour.G)), strconv.Itoa(int(colour.B)))
		i += 2
	}
	return strings.Join(out, ";"), nil
}

// oscBody returns the body of the OSC sequence that starts at start and the
// offset just past its terminator. It accepts both terminators a terminal
// accepts, ST and BEL, so a stream from any writer decodes.
func oscBody(payload []byte, start int) (string, int, error) {
	for i := start + 2; i < len(payload); i++ {
		switch payload[i] {
		case 0x07:
			return string(payload[start+2 : i]), i + 1, nil
		case 0x1b:
			if i+1 < len(payload) && payload[i+1] == '\\' {
				return string(payload[start+2 : i]), i + 2, nil
			}
			return "", 0, fmt.Errorf("blockinterop: OSC sequence at offset %d holds a bare ESC", start)
		}
	}
	return "", 0, fmt.Errorf("blockinterop: unterminated OSC sequence at offset %d", start)
}

// csiEnd returns the offset just past the CSI sequence that starts at start and
// its final byte. It returns -1 when the sequence runs off the end of payload.
func csiEnd(payload []byte, start int) (int, byte) {
	for i := start + 2; i < len(payload); i++ {
		c := payload[i]
		if c >= 0x30 && c <= 0x3f {
			continue
		}
		if c >= 0x20 && c <= 0x2f {
			continue
		}
		if c >= 0x40 && c <= 0x7e {
			return i + 1, c
		}
		return -1, 0
	}
	return -1, 0
}

// paletteIndex parses a terminal register number and rejects one outside the
// 0..255 range a 256-colour palette holds.
func paletteIndex(field string) (int, error) {
	index, err := strconv.Atoi(field)
	if err != nil {
		return 0, fmt.Errorf("register %q is not a number", field)
	}
	if index < 0 || index > 255 {
		return 0, fmt.Errorf("register %d is outside 0..255", index)
	}
	return index, nil
}

// parseXColour parses the "rgb:R/G/B" spec an OSC 4 write carries. Each channel
// holds one to four hex digits, because a terminal reports four where the
// emitter writes two, and every width scales onto the same 8-bit value.
func parseXColour(spec string) (color.RGBA, error) {
	const prefix = "rgb:"
	if !strings.HasPrefix(spec, prefix) {
		return color.RGBA{}, fmt.Errorf("colour spec %q is not an rgb: form", spec)
	}
	channels := strings.Split(spec[len(prefix):], "/")
	if len(channels) != 3 {
		return color.RGBA{}, fmt.Errorf("colour spec %q holds %d channels, want 3", spec, len(channels))
	}
	var scaled [3]uint8
	for i, channel := range channels {
		value, err := scaleHex(channel)
		if err != nil {
			return color.RGBA{}, fmt.Errorf("colour spec %q: %w", spec, err)
		}
		scaled[i] = value
	}
	return color.RGBA{R: scaled[0], G: scaled[1], B: scaled[2], A: 0xff}, nil
}

// scaleHex parses one hex channel of one to four digits and scales it onto the
// full 8-bit range, so "ff", "ffff", and "f" all give 255.
func scaleHex(digits string) (uint8, error) {
	if len(digits) == 0 || len(digits) > 4 {
		return 0, fmt.Errorf("channel %q holds %d hex digits, want 1 to 4", digits, len(digits))
	}
	value, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("channel %q is not hexadecimal", digits)
	}
	full := uint64(1)<<(4*len(digits)) - 1
	return uint8((value*255 + full/2) / full), nil //nolint:gosec // the division bounds the value to 255.
}

// DecodeWithPalette decodes a Block stream whose indexed colours change meaning
// mid-stream. It tracks the palette through the stream, resolves every cell
// colour through the registers in force where that cell was written, and
// returns the visible grid together with the palette events in stream order.
// The grid can be compared directly with the composed grid the writer consumed,
// so a palette written for one frame and applied to another fails the compare.
func DecodeWithPalette(payload []byte, cols, rows int) (block.Grid, []PaletteEvent, error) {
	rewritten, events, err := NewPaletteTracker().Rewrite(payload)
	if err != nil {
		return block.Grid{}, nil, err
	}
	grid, err := Decode(rewritten, cols, rows)
	if err != nil {
		return block.Grid{}, nil, err
	}
	return grid, events, nil
}
