package sixel

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
)

const (
	rareColourJNDReference = 2.3
	rareColourContrastMin  = 3.0
)

var rareColourBackground = color.RGBA{R: 24, G: 28, B: 36, A: 0xff}

type rareColourFeature struct {
	name   string
	phaseX int
	phaseY int
	points []image.Point
	colour color.RGBA
}

// TestSamplePalette64KRareColours pins the accepted quality trade-off for the
// smaller production sample. A feature merging with its background, losing
// structure, dimensions or opacity, failing either decoder, or changing between
// identical runs rejects the policy. WCAG's 3:1 non-text graphical-object
// threshold defines visibility. CIEDE2000 uses 2.3 as the common just-noticeable
// reference; threshold crossings and colour-category changes are recorded, but
// do not reject features that remain visible and structurally intact.
func TestSamplePalette64KRareColours(t *testing.T) {
	for _, size := range paletteBenchSizes {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			stride := paletteSampleStep(size.w, size.h, 1<<16)
			src, features := rareColourFixture(t, size, stride)
			currentPayload := encodePaletteBudget(t, src, 1<<18)
			candidatePayload := encodePaletteBudget(t, src, 1<<16)
			var production bytes.Buffer
			if err := newOpaqueEncoder(&production).encode(src); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(production.Bytes(), candidatePayload) {
				t.Fatal("production output differs from the accepted 64K policy")
			}
			candidateAgain := encodePaletteBudget(t, src, 1<<16)
			if !bytes.Equal(candidatePayload, candidateAgain) {
				t.Fatal("64K payload is not deterministic")
			}

			current := validatePalettePayload(t, currentPayload, src.Bounds())
			candidate := validatePalettePayload(t, candidatePayload, src.Bounds())
			assertOpaque(t, src)
			assertOpaque(t, current)
			assertOpaque(t, candidate)

			currentBG := current.RGBAAt(4, 4)
			candidateBG := candidate.RGBAAt(4, 4)
			var maxIncrease, maxChange float64
			var jndIncreases, jndChanges int
			var categoryChanges []string
			var maxIncreaseFeature, maxChangeFeature string
			for _, feature := range features {
				sourceColour := averageFeatureColour(src, feature.points)
				currentColour := averageFeatureColour(current, feature.points)
				candidateColour := averageFeatureColour(candidate, feature.points)
				contrast := metrics.WCAGContrast(
					candidateColour.R, candidateColour.G, candidateColour.B,
					candidateBG.R, candidateBG.G, candidateBG.B,
				)
				if contrast < rareColourContrastMin {
					t.Errorf("%s contrast %.3f < %.1f", featureLabel(feature), contrast, rareColourContrastMin)
				}
				if rareColourCategory(sourceColour) != rareColourCategory(candidateColour) {
					categoryChanges = append(categoryChanges, fmt.Sprintf("%s category %s changed to %s",
						featureLabel(feature), rareColourCategory(sourceColour), rareColourCategory(candidateColour)))
				}
				currentError := metrics.DeltaE2000(
					sourceColour.R, sourceColour.G, sourceColour.B,
					currentColour.R, currentColour.G, currentColour.B,
				)
				candidateError := metrics.DeltaE2000(
					sourceColour.R, sourceColour.G, sourceColour.B,
					candidateColour.R, candidateColour.G, candidateColour.B,
				)
				increase := candidateError - currentError
				if increase > maxIncrease {
					maxIncrease, maxIncreaseFeature = increase, featureLabel(feature)
				}
				if increase >= rareColourJNDReference {
					jndIncreases++
				}
				change := metrics.DeltaE2000(
					currentColour.R, currentColour.G, currentColour.B,
					candidateColour.R, candidateColour.G, candidateColour.B,
				)
				if change > maxChange {
					maxChange, maxChangeFeature = change, featureLabel(feature)
				}
				if change >= rareColourJNDReference {
					jndChanges++
				}
			}
			firstCategoryChange := "none"
			if len(categoryChanges) > 0 {
				firstCategoryChange = categoryChanges[0]
			}
			t.Logf("stride=%d features=%d category-changes=%d first-category-change=%q max-error-increase=%.3f (%s) max-output-change=%.3f (%s) jnd-increases=%d jnd-changes=%d payload-current=%d payload-64k=%d background-change=%.3f",
				stride, len(features), len(categoryChanges), firstCategoryChange, maxIncrease, maxIncreaseFeature, maxChange, maxChangeFeature,
				jndIncreases, jndChanges, len(currentPayload), len(candidatePayload),
				metrics.DeltaE2000(currentBG.R, currentBG.G, currentBG.B, candidateBG.R, candidateBG.G, candidateBG.B))
		})
	}
}

func TestSamplePaletteExtremeAspectRatio(t *testing.T) {
	t.Parallel()
	const height = 65537
	src := image.NewRGBA(image.Rect(0, 0, 1, height))
	for y := range height {
		src.SetRGBA(0, y, color.RGBA{R: uint8(y), G: uint8(y >> 8), B: uint8(y >> 4), A: 0xff})
	}

	palette := samplePalette(src, paletteColours)
	if len(palette) == 0 || len(palette) > paletteColours {
		t.Fatalf("samplePalette() returned %d colours, want 1..%d", len(palette), paletteColours)
	}
}

func TestSamplePalette64KPreservesExactColourPath(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 24, 24))
	for y := range 24 {
		for x := range 24 {
			c := rareColourBackground
			if x >= 8 && x < 16 {
				c = color.RGBA{R: 0xff, G: 0x50, B: 0x60, A: 0xff}
			}
			if y >= 16 {
				c = color.RGBA{R: 0x60, G: 0xd0, B: 0xff, A: 0xff}
			}
			src.SetRGBA(x, y, c)
		}
	}
	if _, exact := palettedFromRGBA(src, paletteColours); !exact {
		t.Fatal("exact-colour fixture did not take the exact path")
	}
	reusable, exact := palettedFromRGBA(src, 2)
	if exact {
		t.Fatal("exact-colour fixture ignored a smaller palette limit")
	}
	if reusable == nil || reusable.Bounds() != src.Bounds() || len(reusable.Pix) != src.Bounds().Dx()*src.Bounds().Dy() {
		t.Fatal("palette overflow did not retain reusable indexed storage")
	}
	current := encodePaletteBudget(t, src, 1<<18)
	candidate := encodePaletteBudget(t, src, 1<<16)
	if !bytes.Equal(candidate, current) {
		t.Fatal("64K budget changed exact-colour output")
	}
	decoded := validatePalettePayload(t, candidate, src.Bounds())
	assertOpaque(t, decoded)
}

func TestOpaquePaletteFallbackReuseMatchesFreshMapping(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for y := range 16 {
		for x := range 32 {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 16), B: uint8((x + y) * 5), A: 0xff}) //nolint:gosec // Deliberate wrapping test pattern.
		}
	}
	if _, exact := palettedFromRGBA(src, paletteColours); exact {
		t.Fatal("fallback fixture did not exceed the exact palette limit")
	}

	palette := samplePalette(src, paletteColours)
	fresh := image.NewPaletted(src.Bounds(), palette)
	mapPaletted(fresh, src, newPaletteLUT(palette))
	var want, got bytes.Buffer
	if err := newOpaqueEncoder(&want).encode(fresh); err != nil {
		t.Fatal(err)
	}
	if err := newOpaqueEncoder(&got).encode(src); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatal("reused indexed storage changed the fallback payload")
	}
}

func encodePaletteBudget(t *testing.T, src *image.RGBA, budget int) []byte {
	t.Helper()
	paletted, exact := palettedFromRGBA(src, paletteColours)
	if !exact {
		palette := samplePaletteBudget(src, budget)
		paletted.Palette = palette
		mapPaletted(paletted, src, newPaletteLUT(palette))
	}
	var out bytes.Buffer
	if err := newOpaqueEncoder(&out).encode(paletted); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func rareColourFixture(t *testing.T, size paletteBenchSize, stride int) (*image.RGBA, []rareColourFeature) {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, size.w, size.h))
	for y := range size.h {
		for x := range size.w {
			src.SetRGBA(x, y, rareColourBackground)
		}
	}
	// Force the adaptive path with a one-pixel web-style colour strip outside
	// every measured feature and its local background.
	for x := range 512 {
		src.SetRGBA(x, size.h-1, color.RGBA{
			R: uint8(x),      //nolint:gosec // Deliberate wrapping test pattern.
			G: uint8(x >> 1), //nolint:gosec // Deliberate wrapping test pattern.
			B: uint8(x * 37), //nolint:gosec // Deliberate wrapping test pattern.
			A: 0xff,
		})
	}
	cellW, cellH := size.w/stride, size.h/stride
	features := make([]rareColourFeature, 0, stride*stride*8)
	for phaseY := range stride {
		for phaseX := range stride {
			index := phaseY*stride + phaseX
			origin := image.Pt(phaseX*cellW, phaseY*cellH)
			thin := rareColour(index, 0)
			twoPixel := rareColour(index, 120)
			rare := rareColour(index, 240)
			antialias := blendRareColour(thin, rareColourBackground, 0.8)
			features = append(features,
				drawRareRect(src, "line-1px-vertical", phaseX, phaseY, phasePoint(origin.Add(image.Pt(32, 24)), phaseX, phaseY, stride), 1, 18, thin),
				drawRareRect(src, "line-1px-horizontal", phaseX, phaseY, phasePoint(origin.Add(image.Pt(64, 24)), phaseX, phaseY, stride), 18, 1, thin),
				drawRareRect(src, "line-2px-vertical", phaseX, phaseY, phasePoint(origin.Add(image.Pt(96, 24)), phaseX, phaseY, stride), 2, 18, twoPixel),
				drawRareRect(src, "line-2px-horizontal", phaseX, phaseY, phasePoint(origin.Add(image.Pt(128, 24)), phaseX, phaseY, stride), 18, 2, twoPixel),
				drawRareRect(src, "badge", phaseX, phaseY, phasePoint(origin.Add(image.Pt(160, 24)), phaseX, phaseY, stride), 7, 5, rare),
				drawRareRect(src, "block", phaseX, phaseY, phasePoint(origin.Add(image.Pt(192, 24)), phaseX, phaseY, stride), 3, 3, rare),
				drawRareCross(src, "icon", phaseX, phaseY, phasePoint(origin.Add(image.Pt(224, 24)), phaseX, phaseY, stride), rare),
				drawRareRing(src, "antialias-shade", phaseX, phaseY, phasePoint(origin.Add(image.Pt(256, 24)), phaseX, phaseY, stride), antialias),
			)
		}
	}
	assertOpaque(t, src)
	if _, exact := palettedFromRGBA(src, paletteColours); exact {
		t.Fatal("rare-colour fixture did not exercise palette sampling")
	}
	return src, features
}

func paletteSampleStep(width, height, budget int) int {
	step := 2
	for (width/step)*(height/step) > budget {
		step++
	}
	return step
}

func phasePoint(base image.Point, phaseX, phaseY, stride int) image.Point {
	base.X += (phaseX - base.X%stride + stride) % stride
	base.Y += (phaseY - base.Y%stride + stride) % stride
	return base
}

func drawRareRect(img *image.RGBA, name string, phaseX, phaseY int, origin image.Point, width, height int, c color.RGBA) rareColourFeature {
	points := make([]image.Point, 0, width*height)
	for y := range height {
		for x := range width {
			p := origin.Add(image.Pt(x, y))
			img.SetRGBA(p.X, p.Y, c)
			points = append(points, p)
		}
	}
	return rareColourFeature{name: name, phaseX: phaseX, phaseY: phaseY, points: points, colour: c}
}

func drawRareCross(img *image.RGBA, name string, phaseX, phaseY int, origin image.Point, c color.RGBA) rareColourFeature {
	points := make([]image.Point, 0, 17)
	for n := range 9 {
		for _, p := range []image.Point{origin.Add(image.Pt(4, n)), origin.Add(image.Pt(n, 4))} {
			img.SetRGBA(p.X, p.Y, c)
			points = append(points, p)
		}
	}
	return rareColourFeature{name: name, phaseX: phaseX, phaseY: phaseY, points: points, colour: c}
}

func drawRareRing(img *image.RGBA, name string, phaseX, phaseY int, origin image.Point, c color.RGBA) rareColourFeature {
	points := make([]image.Point, 0, 32)
	for n := range 9 {
		for _, p := range []image.Point{
			origin.Add(image.Pt(n, 0)), origin.Add(image.Pt(n, 8)),
			origin.Add(image.Pt(0, n)), origin.Add(image.Pt(8, n)),
		} {
			img.SetRGBA(p.X, p.Y, c)
			points = append(points, p)
		}
	}
	return rareColourFeature{name: name, phaseX: phaseX, phaseY: phaseY, points: points, colour: c}
}

func rareColour(index, hueOffset int) color.RGBA {
	hue := math.Mod(float64(index)*137.508+float64(hueOffset), 360)
	c := 0.55
	x := c * (1 - math.Abs(math.Mod(hue/60, 2)-1))
	var r, g, b float64
	switch int(hue / 60) {
	case 0:
		r, g = c, x
	case 1:
		r, g = x, c
	case 2:
		g, b = c, x
	case 3:
		g, b = x, c
	case 4:
		r, b = x, c
	default:
		r, b = c, x
	}
	const minimum = 1 - 0.55
	return color.RGBA{
		R: uint8(math.Round((r + minimum) * 255)),
		G: uint8(math.Round((g + minimum) * 255)),
		B: uint8(math.Round((b + minimum) * 255)),
		A: 0xff,
	}
}

func blendRareColour(foreground, background color.RGBA, foregroundWeight float64) color.RGBA {
	blend := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a)*foregroundWeight + float64(b)*(1-foregroundWeight)))
	}
	return color.RGBA{
		R: blend(foreground.R, background.R),
		G: blend(foreground.G, background.G),
		B: blend(foreground.B, background.B),
		A: 0xff,
	}
}

func averageFeatureColour(img *image.RGBA, points []image.Point) color.RGBA {
	var r, g, b uint64
	for _, p := range points {
		c := img.RGBAAt(p.X, p.Y)
		r += uint64(c.R)
		g += uint64(c.G)
		b += uint64(c.B)
	}
	n := uint64(len(points))
	return color.RGBA{
		R: uint8(r / n), //nolint:gosec // An average of 8-bit channels stays in 0..255.
		G: uint8(g / n), //nolint:gosec // An average of 8-bit channels stays in 0..255.
		B: uint8(b / n), //nolint:gosec // An average of 8-bit channels stays in 0..255.
		A: 0xff,
	}
}

func featureLabel(feature rareColourFeature) string {
	return fmt.Sprintf("%s phase=(%d,%d)", feature.name, feature.phaseX, feature.phaseY)
}

func rareColourCategory(c color.RGBA) string {
	anchors := []struct {
		name   string
		colour color.RGBA
	}{
		{name: "red", colour: color.RGBA{R: 0xff, G: 0x50, B: 0x50, A: 0xff}},
		{name: "yellow", colour: color.RGBA{R: 0xff, G: 0xe0, B: 0x50, A: 0xff}},
		{name: "green", colour: color.RGBA{R: 0x60, G: 0xe0, B: 0x70, A: 0xff}},
		{name: "cyan", colour: color.RGBA{R: 0x50, G: 0xe0, B: 0xe0, A: 0xff}},
		{name: "blue", colour: color.RGBA{R: 0x60, G: 0x70, B: 0xff, A: 0xff}},
		{name: "magenta", colour: color.RGBA{R: 0xe0, G: 0x60, B: 0xe0, A: 0xff}},
	}
	bestName, bestDistance := "", math.MaxFloat64
	for _, anchor := range anchors {
		distance := metrics.DeltaE2000(c.R, c.G, c.B, anchor.colour.R, anchor.colour.G, anchor.colour.B)
		if distance < bestDistance {
			bestName, bestDistance = anchor.name, distance
		}
	}
	return bestName
}
