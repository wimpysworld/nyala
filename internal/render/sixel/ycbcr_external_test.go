package sixel_test

import (
	"bytes"
	"image"
	"image/draw"
	"regexp"
	"testing"

	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/sixel"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

func TestRenderYCbCrMatchesMaterialisedRGBA(t *testing.T) {
	t.Parallel()

	ratios := []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444,
		image.YCbCrSubsampleRatio422,
		image.YCbCrSubsampleRatio420,
		image.YCbCrSubsampleRatio440,
		image.YCbCrSubsampleRatio411,
		image.YCbCrSubsampleRatio410,
	}
	origins := []image.Point{
		image.Pt(0, 0),
		image.Pt(3, 5),
	}

	for _, ratio := range ratios {
		for _, origin := range origins {
			bounds := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(19, 13))}
			name := ratio.String() + "/" + origin.String()
			t.Run(name, func(t *testing.T) {
				src := patternedYCbCr(bounds, ratio)
				materialised := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
				draw.Draw(materialised, materialised.Bounds(), src, bounds.Min, draw.Src)

				nativePayload, native := renderAndDecode(t, src)
				_, rgba := renderAndDecode(t, materialised)
				if got, want := native.Bounds(), materialised.Bounds(); got != want {
					t.Fatalf("decoded bounds = %v, want %v", got, want)
				}
				psnr := metrics.PSNR(rgba, native)
				ssim := metrics.SSIM(rgba, native)
				if psnr < 30 || ssim < 0.95 {
					t.Fatalf("native YCbCr quality versus RGBA path: PSNR=%f SSIM=%f", psnr, ssim)
				}
				for i := 3; i < len(native.Pix); i += 4 {
					if native.Pix[i] != 0xff {
						t.Fatalf("alpha byte %d = %d, want 255", i, native.Pix[i])
					}
				}
				definitions := regexp.MustCompile(`#\d+;2;`).FindAll(nativePayload, -1)
				if len(definitions) == 0 || len(definitions) > 256 {
					t.Fatalf("palette definitions = %d, want 1..256", len(definitions))
				}
			})
		}
	}
}

func patternedYCbCr(bounds image.Rectangle, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	img := image.NewYCbCr(bounds, ratio)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			img.Y[img.YOffset(x, y)] = uint8(16 + positiveMod(x*17+y*29, 220)) //nolint:gosec // Modulo bounds the value below 256.
			chroma := img.COffset(x, y)
			img.Cb[chroma] = uint8(16 + positiveMod(x*11+y*7, 224)) //nolint:gosec // Modulo bounds the value below 256.
			img.Cr[chroma] = uint8(16 + positiveMod(x*5+y*13, 224)) //nolint:gosec // Modulo bounds the value below 256.
		}
	}
	return img
}

func positiveMod(value, modulus int) int {
	value %= modulus
	if value < 0 {
		value += modulus
	}
	return value
}

func renderAndDecode(t *testing.T, bitmap image.Image) ([]byte, *image.RGBA) {
	t.Helper()
	var renderer sixel.Renderer
	var payload bytes.Buffer
	if err := renderer.Render(render.Frame{Img: bitmap}, &payload); err != nil {
		t.Fatalf("Render(): %v", err)
	}
	decoded, err := renderdecode.DecodeSixel(payload.Bytes())
	if err != nil {
		t.Fatalf("DecodeSixel(): %v", err)
	}
	return payload.Bytes(), decoded
}
