//go:build ocr

// The tagged OCR hook for the sweep. It builds only under the "ocr" build tag,
// so the default build and the shipped nyala binary stay cgo-free. It renders the
// synthetic "Text Size NNpx" ladder, round-trips it through the named backend's
// encode-and-decode path (the same path the sweep scores), runs the gosseract
// CER ladder over the round-tripped image, and returns the corpus CER and WER
// plus the minimum legible size. It mirrors the calibration OCR ladder geometry
// so the two numbers are comparable. When no Tesseract engine is present it
// returns null scores, so the tagged sweep stays green without Tesseract.
//
// Channel order is RGBA throughout.
package harness

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/ocr"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	rfont "github.com/wimpysworld/nyala/internal/render/font"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// errNoGlyph reports the embedded font atlas lacks the probe glyph, so the OCR
// ladder cannot build a face.
var errNoGlyph = errors.New("harness: font atlas has no glyph for probe")

// sweepCERBound is the CER bound the sweep ladder marks legibility at. It matches
// the calibration ladder bound (calibCERBound), so the sweep's minimum legible
// size is comparable to the calibration record.
const sweepCERBound = 0.15

// sweepLadderRowH is the per-rung row height of the rendered ladder, in source
// pixels. It matches the calibration ladder so the spec geometry lines up.
const sweepLadderRowH = 48

// ocrScores renders the synthetic ladder, round-trips it through the named mode's
// capture path, OCRs each rung, and returns the corpus CER and WER plus the
// minimum legible size. The captured render of the page is not used: the page
// carries no known truth, so legibility is measured on the synthetic ladder run
// through the same encode-and-decode path the sweep scores. A missing Tesseract
// engine or a build error returns null scores. Channel order is RGBA.
func ocrScores(rast *capture.Rasteriser, mode string, _ *image.RGBA) (cer, wer *float64, minLegiblePx *int) {
	eng, err := ocr.NewEngine()
	if err != nil {
		return nil, nil, nil
	}
	defer eng.Close()

	sizes := ocr.LadderSizes
	ladder, err := renderSweepLadder(sizes)
	if err != nil {
		return nil, nil, nil
	}
	roundTripped, err := roundTripLadder(rast, mode, ladder)
	if err != nil {
		return nil, nil, nil
	}

	spec := ocr.Spec{Sizes: sizes, RowHeight: sweepLadderRowH}
	results, err := ocr.Ladder(roundTripped, spec, eng, sweepCERBound)
	if err != nil {
		return nil, nil, nil
	}
	if len(results) == 0 {
		return nil, nil, nil
	}

	var cerSum, werSum float64
	for _, r := range results {
		cerSum += r.Result.CER
		werSum += r.Result.WER
	}
	n := float64(len(results))
	cerMean := cerSum / n
	werMean := werSum / n
	cer = &cerMean
	wer = &werMean
	if m, ok := ocr.MinLegibleSize(results); ok {
		minLegiblePx = &m
	}
	return cer, wer, minLegiblePx
}

// roundTripLadder encodes the ladder image through the named backend and decodes
// it back, so the OCR pass reads the true round-tripped render the sweep scores.
// Block rasterises its owned cell grid through Route A; sixel and kitty encode
// the image to their wire payload and decode it back.
func roundTripLadder(rast *capture.Rasteriser, mode string, ladder *image.RGBA) (*image.RGBA, error) {
	switch mode {
	case "block":
		return roundTripBlockLadder(rast, ladder), nil
	case "sixel":
		return roundTripSixelLadder(ladder)
	case "kitty":
		return roundTripKittyLadder(ladder)
	default:
		return ladder, nil
	}
}

// roundTripBlockLadder composes the ladder onto a block grid sized to the ladder
// and rasterises it through Route A. Each cell maps to a 2x4 block, so the grid
// is half the ladder width and a quarter its height in cells.
func roundTripBlockLadder(rast *capture.Rasteriser, ladder *image.RGBA) *image.RGBA {
	b := ladder.Bounds()
	cols := b.Dx() / 2
	rows := b.Dy() / 4
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	br := block.Renderer{Cols: cols, Rows: rows}
	grid := br.Compose(render.Frame{Img: ladder})
	return rast.Rasterise(grid)
}

// roundTripSixelLadder encodes the ladder as Sixel and decodes it back.
func roundTripSixelLadder(ladder *image.RGBA) (*image.RGBA, error) {
	var r sixel.Renderer
	var buf bytes.Buffer
	if err := r.Render(render.Frame{Img: ladder}, &buf); err != nil {
		return nil, err
	}
	return capture.DecodeSixel(buf.Bytes())
}

// roundTripKittyLadder encodes the ladder as Kitty and decodes it back. Kitty is
// lossless, so the decoded image equals the ladder up to channel layout.
func roundTripKittyLadder(ladder *image.RGBA) (*image.RGBA, error) {
	var r kitty.Renderer
	var buf bytes.Buffer
	if err := r.Render(render.Frame{Img: ladder}, &buf); err != nil {
		return nil, err
	}
	return capture.DecodeKitty(buf.Bytes())
}

// renderSweepLadder stacks one "Text Size NNpx" line per size into a tall
// left-column ladder image, the synthetic image the OCR pass round-trips. It
// mirrors the calibration ladder geometry so the two measurements line up.
func renderSweepLadder(sizes []int) (*image.RGBA, error) {
	maxW := 0
	lines := make([]*image.RGBA, len(sizes))
	for i, sz := range sizes {
		img, err := renderSweepLine(ocr.LadderText(sz), sz)
		if err != nil {
			return nil, err
		}
		lines[i] = img
		if img.Bounds().Dx() > maxW {
			maxW = img.Bounds().Dx()
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, maxW+8, sweepLadderRowH*len(sizes)))
	for i := range out.Pix {
		out.Pix[i] = 255 // opaque white ground
	}
	for i, img := range lines {
		dy := i*sweepLadderRowH + (sweepLadderRowH-img.Bounds().Dy())/2
		if dy < i*sweepLadderRowH {
			dy = i * sweepLadderRowH
		}
		r := image.Rect(4, dy, 4+img.Bounds().Dx(), dy+img.Bounds().Dy())
		draw.Draw(out, r, img, img.Bounds().Min, draw.Over)
	}
	return out, nil
}

// renderSweepLine draws s as dark text on a light ground at the given pixel cap
// height using the embedded FiraCode Nerd Font Mono face, matching the calibration line render.
func renderSweepLine(s string, capPx int) (*image.RGBA, error) {
	a, err := rfont.Load()
	if err != nil {
		return nil, err
	}
	fnt, _, ok := a.Resolve('A')
	if !ok {
		return nil, errNoGlyph
	}
	size := float64(capPx) / 0.7
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{Size: size, DPI: 72})
	if err != nil {
		return nil, err
	}
	defer face.Close()

	adv := font.MeasureString(face, s)
	w := adv.Ceil() + 8
	h := int(size*1.4) + 8
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255 // opaque white ground
	}
	d := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.RGBA{A: 255}),
		Face: face,
		Dot:  fixed.P(4, int(size)),
	}
	d.DrawString(s)
	return img, nil
}
