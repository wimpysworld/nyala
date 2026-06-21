// Package font embeds the Route A rasteriser fonts and checks glyph coverage.
//
// Route A renders the block backend's owned cell grid back to an image so the
// metrics can score it. That render needs a deterministic, embedded font.
// Cascadia Mono is the primary face; GNU Unifont is the fallback for code
// points Cascadia lacks. Both faces ship in this package via go:embed so the
// shipped binary carries them and no system font is read at runtime.
//
// GNU Unifont's official build ships as OpenType (.otf), not TrueType. Both
// parse through golang.org/x/image/font/sfnt, so the fallback embeds the .otf.
package font

import (
	_ "embed"
	"fmt"

	"golang.org/x/image/font/sfnt"
)

//go:embed CascadiaMono.ttf
var cascadiaTTF []byte

//go:embed Unifont.otf
var unifontOTF []byte

// Atlas holds the parsed primary and fallback fonts plus a reusable glyph
// lookup buffer. The primary is Cascadia Mono; the fallback is GNU Unifont.
type Atlas struct {
	// Primary is Cascadia Mono, the face used for most glyphs.
	Primary *sfnt.Font
	// Fallback is GNU Unifont, used for code points Cascadia lacks.
	Fallback *sfnt.Font

	buf sfnt.Buffer
}

// Load parses the embedded fonts and returns the atlas. It fails if either
// embedded font does not parse.
func Load() (*Atlas, error) {
	primary, err := sfnt.Parse(cascadiaTTF)
	if err != nil {
		return nil, fmt.Errorf("parse Cascadia Mono: %w", err)
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

// Covers reports whether the atlas can render r in either font.
func (a *Atlas) Covers(r rune) bool {
	_, _, ok := a.Resolve(r)
	return ok
}

// CoverageGap is one rune the atlas cannot render. It would show as tofu.
type CoverageGap struct {
	// Rune is the uncovered code point.
	Rune rune
}

// Coverage checks every rune in glyphs against both fonts and returns the gaps:
// the runes neither Cascadia Mono nor GNU Unifont can render. An empty result
// means full coverage of the emitted glyph table.
func (a *Atlas) Coverage(glyphs []rune) []CoverageGap {
	var gaps []CoverageGap
	for _, r := range glyphs {
		if !a.Covers(r) {
			gaps = append(gaps, CoverageGap{Rune: r})
		}
	}
	return gaps
}
