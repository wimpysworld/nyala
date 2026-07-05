// Package font embeds the Route A rasteriser fonts and checks glyph coverage.
//
// Route A renders the block backend's owned cell grid back to an image so the
// metrics can score it. That render needs a deterministic, embedded font.
// FiraCode Nerd Font Mono is the primary face; GNU Unifont is the fallback for
// code points the primary lacks. Both faces ship in this package via go:embed
// so the shipped binary carries them and no system font is read at runtime.
//
// GNU Unifont's official build ships as OpenType (.otf), not TrueType. Both
// parse through golang.org/x/image/font/sfnt, so the fallback embeds the .otf.
package font

import (
	_ "embed"
	"fmt"

	"golang.org/x/image/font/sfnt"
)

//go:embed FiraCodeNerdFontMono-Regular.ttf
var firaCodeTTF []byte

//go:embed Unifont.otf
var unifontOTF []byte

// FiraCodeTTF returns the embedded FiraCode Nerd Font Mono TTF bytes, the
// primary face used by the Route A rasteriser.
func FiraCodeTTF() []byte {
	return firaCodeTTF
}

// Atlas holds the parsed primary and fallback fonts plus a reusable glyph
// lookup buffer. The primary is FiraCode Nerd Font Mono; the fallback is GNU
// Unifont.
type Atlas struct {
	// Primary is FiraCode Nerd Font Mono, the face used for most glyphs.
	Primary *sfnt.Font
	// Fallback is GNU Unifont, used for code points the primary lacks.
	Fallback *sfnt.Font

	buf sfnt.Buffer
}

// Load parses the embedded fonts and returns the atlas. It fails if either
// embedded font does not parse.
func Load() (*Atlas, error) {
	primary, err := sfnt.Parse(firaCodeTTF)
	if err != nil {
		return nil, fmt.Errorf("parse FiraCode Nerd Font Mono: %w", err)
	}
	fallback, err := sfnt.Parse(unifontOTF)
	if err != nil {
		return nil, fmt.Errorf("parse GNU Unifont: %w", err)
	}
	return &Atlas{Primary: primary, Fallback: fallback}, nil
}

// Resolve returns the font that has a glyph for r and that font's glyph index.
// It tries the primary first, then the fallback. The bool is false when neither
// font covers r, so the caller knows the rune would render as tofu.
func (a *Atlas) Resolve(r rune) (*sfnt.Font, sfnt.GlyphIndex, bool) {
	if gi, err := a.Primary.GlyphIndex(&a.buf, r); err == nil && gi != 0 {
		return a.Primary, gi, true
	}
	if gi, err := a.Fallback.GlyphIndex(&a.buf, r); err == nil && gi != 0 {
		return a.Fallback, gi, true
	}
	return nil, 0, false
}
