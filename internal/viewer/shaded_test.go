package viewer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// TestResolveSchemeShadedExplicitOnly pins the Shaded selection policy: only
// the explicit --blocks=shaded value resolves to the scheme, and auto never
// does, even on a terminal the sextant allow-list admits.
func TestResolveSchemeShadedExplicitOnly(t *testing.T) {
	if got := resolveScheme("shaded", nil); got != block.SchemeShaded {
		t.Fatalf("resolveScheme(shaded) = %d, want SchemeShaded", got)
	}
	kittyEnv := []string{"KITTY_WINDOW_ID=1", "TERM=xterm-kitty"}
	if got := resolveScheme("auto", kittyEnv); got == block.SchemeShaded {
		t.Fatal("resolveScheme(auto) selected Shaded")
	}
}

// TestApplyShadedColour pins the startup colour resolution for the explicit
// Shaded scheme: explicit truecolour fails fast with the documented error,
// an automatic truecolour resolution degrades to xterm-256, every compatible
// resolved mode stands, and a non-Shaded scheme is untouched.
func TestApplyShadedColour(t *testing.T) {
	const wantErr = "viewer: shaded Block mode does not support truecolour; use --color=256 or --color=ansi-grey"

	st := NewState(80, 24)
	st.Scheme = block.SchemeShaded
	st.Colour = ColourTrue
	if err := st.applyShadedColour("truecolor"); err == nil || err.Error() != wantErr {
		t.Fatalf("explicit truecolour error = %v, want %q", err, wantErr)
	}

	st.Colour = ColourTrue
	if err := st.applyShadedColour("auto"); err != nil {
		t.Fatalf("auto resolution: %v", err)
	}
	if st.Colour != ColourXTerm256 {
		t.Fatalf("auto truecolour degraded to %d, want ColourXTerm256", st.Colour)
	}

	for _, mode := range []ColourMode{ColourXTerm256, ColourXTermGrey} {
		st.Colour = mode
		if err := st.applyShadedColour("auto"); err != nil || st.Colour != mode {
			t.Fatalf("compatible mode %d changed to %d (err %v)", mode, st.Colour, err)
		}
	}

	quadrant := NewState(80, 24)
	quadrant.Colour = ColourTrue
	if err := quadrant.applyShadedColour("truecolor"); err != nil || quadrant.Colour != ColourTrue {
		t.Fatalf("non-Shaded scheme changed: colour %d err %v", quadrant.Colour, err)
	}
}

// TestCycleSchemeShaded pins the Alt+B policy for Shaded: the cycle offers
// Shaded whenever the current colour mode is 256 or ansi-grey, on every
// session however it started, and skips it while the mode is truecolour. No
// startup state arms the scheme; the toggles read only the current colour
// mode, so leaving Shaded keeps no hidden flag.
func TestCycleSchemeShaded(t *testing.T) {
	st := NewState(80, 24)
	st.sextantOK = true
	st.Scheme = block.SchemeSextant

	// A truecolour-start session skips Shaded and wraps to quadrant.
	if got := st.CycleScheme(false); got != block.SchemeQuadrant {
		t.Fatalf("truecolour cycle = %d, want quadrant", got)
	}

	// Alt+C to 256 puts Shaded in the cycle at once, and Alt+B never touches
	// the colour mode.
	if got := st.CycleColour(false); got != ColourXTerm256 {
		t.Fatalf("colour cycle = %d, want ColourXTerm256", got)
	}
	st.Scheme = block.SchemeSextant
	if got := st.CycleScheme(false); got != block.SchemeShaded {
		t.Fatalf("256 cycle = %d, want SchemeShaded", got)
	}
	if got := st.CycleScheme(false); got != block.SchemeQuadrant {
		t.Fatalf("wrap after Shaded = %d, want quadrant", got)
	}
	if st.Colour != ColourXTerm256 {
		t.Fatalf("scheme cycle changed colour to %d, want ColourXTerm256", st.Colour)
	}

	// Raising the colour mode back to truecolour removes Shaded from the
	// cycle until the mode drops again, even after a Shaded stint.
	st.Colour = ColourTrue
	st.Scheme = block.SchemeSextant
	if got := st.CycleScheme(false); got == block.SchemeShaded {
		t.Fatal("truecolour cycle landed on Shaded after a Shaded stint")
	}
	st.Colour = ColourXTermGrey
	st.Scheme = block.SchemeSextant
	if got := st.CycleScheme(false); got != block.SchemeShaded {
		t.Fatalf("ansi-grey cycle = %d, want SchemeShaded", got)
	}
}

// TestCycleColourShadedSkipsTruecolour pins the Alt+C rule under Shaded: the
// ladder cycles 256 -> grey and wraps past truecolour in both directions,
// even when the capability cap allows truecolour.
func TestCycleColourShadedSkipsTruecolour(t *testing.T) {
	st := NewState(80, 24)
	st.Scheme = block.SchemeShaded
	st.colourCap = ColourTrue
	st.Colour = ColourXTerm256
	want := []ColourMode{ColourXTermGrey, ColourXTerm256}
	for i, w := range want {
		if got := st.CycleColour(false); got != w {
			t.Fatalf("cycle %d = %d, want %d", i, got, w)
		}
	}
	if got := st.CycleColour(true); got != ColourXTermGrey {
		t.Fatalf("reverse wrap = %d, want ansi-grey, not truecolour", got)
	}
}

// TestShadedStreamSGRRanges proves no truecolour SGR can appear from a Shaded
// writer path: every compatible colour mode emits only its own palette
// escapes.
func TestShadedStreamSGRRanges(t *testing.T) {
	frame := render.Frame{Img: monochromeRampImage(16, 16)}
	for _, mode := range []ColourMode{ColourXTerm256, ColourXTermGrey} {
		st := NewState(8, 4)
		st.Scheme = block.SchemeShaded
		st.Colour = mode
		st.SetDiff(&block.GridDiff{})
		var out bytes.Buffer
		if err := st.RenderStreamFrame(frame, &out); err != nil {
			t.Fatalf("mode %d draw: %v", mode, err)
		}
		payload := out.String()
		for _, forbidden := range []string{"\x1b[38;2;", "\x1b[48;2;"} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("mode %d emitted truecolour SGR %q in %q", mode, forbidden, payload)
			}
		}
		for _, params := range decodedSGRs(t, payload) {
			if len(params) != 3 || params[1] != 5 || (params[0] != 38 && params[0] != 48) {
				t.Fatalf("mode %d emitted non-indexed SGR %v", mode, params)
			}
			if params[2] < 16 || params[2] > 255 {
				t.Fatalf("mode %d emitted index %d outside 16..255", mode, params[2])
			}
		}
	}
}

// TestShadedComposeQuantisesToPalette proves the palette-before-score rule
// reaches the production compose: every bitmap cell of a Shaded xterm-256
// compose already holds a canonical palette colour, so the writer's index
// lookup can never shift the displayed colour.
func TestShadedComposeQuantisesToPalette(t *testing.T) {
	st := NewState(8, 4)
	st.Scheme = block.SchemeShaded
	st.Colour = ColourXTerm256
	grid := st.ComposeBlockFrame(render.Frame{Img: monochromeRampImage(16, 16)})
	if grid == nil {
		t.Fatal("ComposeBlockFrame returned nil")
	}
	for i, cell := range grid.Cells {
		fgCanonical := block.XTerm256RGBA(block.XTerm256(cell.FG))
		bgCanonical := block.XTerm256RGBA(block.XTerm256(cell.BG))
		if cell.FG != fgCanonical || cell.BG != bgCanonical {
			t.Fatalf("cell %d colours fg %v bg %v are not canonical palette entries", i, cell.FG, cell.BG)
		}
	}
}
