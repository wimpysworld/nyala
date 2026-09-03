package palette

import (
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
)

// fullColours is the whole terminal palette, the count a base-zero engine
// quantises to when nothing reserves registers for the terminal theme.
const fullColours = 256

// budgetFrame builds a 256x256 frame whose every pixel differs, so median cut
// always has more distinct colours than any register budget asks for. The frame
// sits exactly on the 64K sample budget, so the engine quantises it whole.
func budgetFrame() *image.RGBA {
	const side = 256
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x),     //nolint:gosec // Bounded by the fixture width.
				G: uint8(y),     //nolint:gosec // Bounded by the fixture height.
				B: uint8(x ^ y), //nolint:gosec // Bounded by the fixture extent.
				A: 0xff,
			})
		}
	}
	return img
}

// requantise drives one frame straight through a re-quantisation, skipping the
// drift test, because these tests judge the palette the engine builds rather
// than when it decides to build one. Every case asks for the whole terminal
// palette, so the budget mode's own cap is the only thing that can narrow it.
func requantise(t *testing.T, e *Engine, img *image.RGBA) {
	t.Helper()
	e.Requantise(img.Bounds(), e.Sample(img), fullColours)
}

// lutIndex returns the register the LUT gives a colour, through the same RGB555
// key a Block consumer builds.
func lutIndex(lut []uint8, c color.RGBA) uint8 {
	return lut[int(c.R>>3)<<10|int(c.G>>3)<<5|int(c.B>>3)]
}

func TestBudgetModeCapsPaletteAndRebasesLUT(t *testing.T) {
	t.Parallel()
	var engine Engine
	engine.SetBudget(true)
	if got := engine.Base(); got != BudgetBase {
		t.Fatalf("Base() = %d, want %d", got, BudgetBase)
	}
	requantise(t, &engine, budgetFrame())
	if len(engine.Palette) > BudgetColours {
		t.Fatalf("palette entries = %d, want at most %d", len(engine.Palette), BudgetColours)
	}
	top := BudgetBase + len(engine.Palette) - 1
	for key, register := range engine.LUT {
		if int(register) < BudgetBase || int(register) > top {
			t.Fatalf("LUT[%d] = %d, want a register in %d..%d", key, register, BudgetBase, top)
		}
	}
	if top > 255 {
		t.Fatalf("top register = %d, want at most 255", top)
	}
}

func TestBudgetModeLUTIndexFeedsOSC4Directly(t *testing.T) {
	t.Parallel()
	var engine Engine
	engine.SetBudget(true)
	frame := budgetFrame()
	requantise(t, &engine, frame)

	// A consumer reads one register out of the LUT and writes it to the SGR
	// stream unchanged. The same register must name the entry AppendOSC4 sets,
	// with no base arithmetic anywhere but inside the engine.
	sequence := string(AppendOSC4(nil, engine.Palette, engine.Base()))
	for _, sample := range []color.RGBA{
		frame.RGBAAt(0, 0),
		frame.RGBAAt(255, 255),
		frame.RGBAAt(64, 192),
	} {
		register := lutIndex(engine.LUT, sample)
		if int(register) < BudgetBase {
			t.Fatalf("register %d for %v is inside the reserved theme range", register, sample)
		}
		entry := engine.Palette[int(register)-engine.Base()]
		r, g, b, _ := entry.RGBA()
		want := fmt.Sprintf(";%d;rgb:%02x/%02x/%02x", register, r>>8, g>>8, b>>8)
		if !strings.Contains(sequence, want) {
			t.Fatalf("OSC 4 sequence does not set register %d to its LUT colour; want %q", register, want)
		}
	}
}

func TestBudgetModeOffKeepsFullPaletteAtBaseZero(t *testing.T) {
	t.Parallel()
	var engine Engine
	if got := engine.Base(); got != 0 {
		t.Fatalf("zero-value Base() = %d, want 0", got)
	}
	requantise(t, &engine, budgetFrame())
	if len(engine.Palette) != fullColours {
		t.Fatalf("palette entries = %d, want %d", len(engine.Palette), fullColours)
	}
	// A base-zero LUT names palette positions, so the reserved theme range
	// carries picture colours like any other register.
	if !strings.Contains(string(AppendOSC4(nil, engine.Palette, engine.Base())), "\x1b]4;0;rgb:") {
		t.Fatal("OSC 4 sequence does not start at register 0")
	}
	var lowest uint8 = 255
	for _, register := range engine.LUT {
		lowest = min(lowest, register)
	}
	if lowest >= BudgetBase {
		t.Fatalf("lowest LUT register = %d, want a register below %d", lowest, BudgetBase)
	}
}

func TestBudgetModeSwitchRebasesRetainedState(t *testing.T) {
	t.Parallel()
	var engine Engine
	frame := budgetFrame()
	requantise(t, &engine, frame)
	engine.SetBudget(true)
	if engine.Reuse(frame.Bounds(), engine.Sample(frame)) {
		t.Fatal("a mode change reused a palette built for the previous register base")
	}
	requantise(t, &engine, frame)
	if len(engine.Palette) > BudgetColours {
		t.Fatalf("palette entries after the switch = %d, want at most %d", len(engine.Palette), BudgetColours)
	}
	for key, register := range engine.LUT {
		if int(register) < BudgetBase {
			t.Fatalf("LUT[%d] = %d, want a register at or above %d", key, register, BudgetBase)
		}
	}
}
