package font

import "testing"

// blockGlyphs is the set of block glyphs the quadrant and half-block
// compositors emit. The Route A font must cover every one or those cells render
// as tofu. Kept in sync with internal/render/block quadrantGlyphs.
var blockGlyphs = []rune{
	0x2584, // lower half block
	0x2596, // quadrant lower left
	0x2597, // quadrant lower right
	0x259D, // quadrant upper right
	0x259E, // quadrant upper right and lower left
	0x2590, // right half block
	0x2598, // quadrant upper left
	0x258C, // left half block
	0x259A, // quadrant upper left and lower right
}

func TestLoadBothFonts(t *testing.T) {
	a, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.Primary == nil {
		t.Error("primary font is nil")
	}
	if a.Fallback == nil {
		t.Error("fallback font is nil")
	}
}

func TestKnownGlyphsResolve(t *testing.T) {
	a, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A digit, a letter, and the lower-half-block glyph the block backend emits.
	for _, r := range []rune{'0', 'A', 0x2584} {
		f, gi, ok := a.Resolve(r)
		if !ok {
			t.Errorf("rune %q (U+%04X) does not resolve in either font", r, r)
			continue
		}
		if f == nil || gi == 0 {
			t.Errorf("rune %q resolved to nil font or glyph index 0", r)
		}
	}
}

func TestResolvePrefersPrimary(t *testing.T) {
	a, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// ASCII letters live in FiraCode Nerd Font Mono, so the primary must win.
	f, _, ok := a.Resolve('A')
	if !ok {
		t.Fatal("'A' did not resolve")
	}
	if f != a.Primary {
		t.Error("'A' resolved to the fallback, expected the primary")
	}
}

func TestBlockGlyphsResolve(t *testing.T) {
	a, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range blockGlyphs {
		if _, _, ok := a.Resolve(r); !ok {
			t.Errorf("block glyph U+%04X would render as tofu", r)
		}
	}
}
