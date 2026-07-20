package engine

import (
	"fmt"
	"image"
	"image/draw"

	"github.com/wimpysworld/nyala/internal/render"
)

type backend = render.Backend

const (
	backendBlock = render.BackendBlock
	backendSixel = render.BackendSixel
	backendKitty = render.BackendKitty
)

// decodeScreencastNative decodes one base64 screencast payload without forcing
// the decoder result into RGBA.
func decodeScreencastNative(data string) (image.Image, error) {
	raw, release, err := decodeBase64Pooled(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	defer release()
	img, err := decodeFrameImage(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	return img, nil
}

// decodeImageBytes decodes raw image bytes to RGBA channel order.
func decodeImageBytes(raw []byte) (*image.RGBA, error) {
	img, err := decodeFrameImage(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	return imageToRGBA(img), nil
}

// imageToRGBA returns an RGBA image without copying an existing RGBA source.
func imageToRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	return rgba
}

// initialScreencastFormat reads the active backend under the engine lock.
func (e *ChromiumEngine) initialScreencastFormat() screencastFmt {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initialScreencastFormatLocked()
}

// buildStylesheetScript builds the production stylesheet with text paint hidden.
func buildStylesheetScript(fontBytes []byte, cellW, cellH float64) string {
	return buildStylesheetScriptWithPaint(fontBytes, cellW, cellH, false)
}
