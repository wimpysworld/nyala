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

// Covers reports whether the atlas can render r in either font.
func (a *Atlas) Covers(r rune) bool {
	_, _, ok := a.Resolve(r)
	return ok
}

// QuadrantSupport reports which glyph tier the atlas reaches for the quadrant
// block set, deciding the KMSCON fallback with no runtime terminal probe. The
// fallback exists because KMSCON font coverage is environment-dependent: a given
// console may lack the quadrant glyphs, so the renderer picks the richest tier
// the embedded fonts can actually draw rather than assuming the terminal has them.
type QuadrantSupport int

const (
	// QuadrantPrimary means the primary font covers the quadrant block set
	// U+2596..U+259F, so the quadrant scheme renders from the primary face.
	QuadrantPrimary QuadrantSupport = iota
	// QuadrantUnifont means the primary lacks the quadrant set but the GNU
	// Unifont fallback covers it, so the quadrant scheme renders from the
	// fallback face.
	QuadrantUnifont
	// QuadrantHalfBlock means neither font covers the full quadrant set, so the
	// renderer falls to the half-block floor U+2584.
	QuadrantHalfBlock
)

// quadrantRange is the contiguous quadrant block set U+2596..U+259F the quadrant
// scheme emits. The fallback tier is decided by whether a font covers every code
// point in this range.
const (
	quadrantLow  = 0x2596
	quadrantHigh = 0x259F
)

// QuadrantSupport reports the highest tier the atlas reaches for the quadrant
// block set U+2596..U+259F. It returns QuadrantPrimary when the primary font
// covers the whole set, QuadrantUnifont when only the GNU Unifont fallback does,
// and QuadrantHalfBlock when neither covers it so the half-block floor U+2584
// applies. Coverage is checked through GlyphIndex on each font in turn.
func (a *Atlas) QuadrantSupport() QuadrantSupport {
	if a.fontCoversQuadrants(a.Primary) {
		return QuadrantPrimary
	}
	if a.fontCoversQuadrants(a.Fallback) {
		return QuadrantUnifont
	}
	return QuadrantHalfBlock
}

// fontCoversQuadrants reports whether font f has a glyph for every code point in
// the quadrant block set U+2596..U+259F. A nil font or any missing code point
// returns false.
func (a *Atlas) fontCoversQuadrants(f *sfnt.Font) bool {
	if f == nil {
		return false
	}
	for r := rune(quadrantLow); r <= quadrantHigh; r++ {
		gi, err := f.GlyphIndex(&a.buf, r)
		if err != nil || gi == 0 {
			return false
		}
	}
	return true
}

// CoverageGap is one rune the atlas cannot render. It would show as tofu.
type CoverageGap struct {
	// Rune is the uncovered code point.
	Rune rune
}

// Coverage checks every rune in glyphs against both fonts and returns the gaps:
// the runes neither FiraCode Nerd Font Mono nor GNU Unifont can render. An empty
// result means full coverage of the emitted glyph table.
func (a *Atlas) Coverage(glyphs []rune) []CoverageGap {
	var gaps []CoverageGap
	for _, r := range glyphs {
		if !a.Covers(r) {
			gaps = append(gaps, CoverageGap{Rune: r})
		}
	}
	return gaps
}
