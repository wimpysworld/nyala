//go:build sixelinterop

package sixel_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// decoderChannelTolerance covers only the different integer rounding used when
// go-sixel and libsixel expand Sixel's 0..100 colour registers back to 0..255.
const decoderChannelTolerance = 2

func TestLibsixelRoundTrip(t *testing.T) {
	tool, err := exec.LookPath("sixel2png")
	if err != nil {
		t.Fatal("sixel2png not found; run this gate through `just sixel-interop` in the Nix dev shell")
	}
	version, err := exec.Command(tool, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("sixel2png --version: %v: %s", err, version)
	}
	t.Logf("decoder: %s", bytes.SplitN(version, []byte{'\n'}, 2)[0])

	committed := loadPNG(t, filepath.Join("..", "..", "..", "testdata", "testpage-02-basic.png"))
	cases := []struct {
		name     string
		img      image.Image
		exact    bool
		adaptive bool
	}{
		{name: "flat-odd-height", img: flatFixture(79, 13), exact: true},
		{name: "sparse-planes-index-255", img: sparsePaletteFixture(), exact: true},
		{name: "rle-3", img: runFixture(3), exact: true},
		{name: "rle-4", img: runFixture(4), exact: true},
		{name: "rle-8192", img: runFixture(8192), exact: true},
		{name: "rle-8193", img: runFixture(8193), exact: true},
		{name: "photo-parallel-map-threshold", img: photoFixture(800, 625), adaptive: true},
		{name: "committed-page-production-parallel", img: committed, adaptive: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := renderPayload(t, tc.img)
			assertCompletePayload(t, payload, tc.img.Bounds().Dx(), tc.img.Bounds().Dy())
			goDecoded, err := capture.DecodeSixel(payload)
			if err != nil {
				t.Fatalf("go-sixel decode: %v", err)
			}
			libDecoded := decodeLibsixel(t, tool, payload)
			wantBounds := image.Rect(0, 0, tc.img.Bounds().Dx(), tc.img.Bounds().Dy())
			if goDecoded.Bounds() != wantBounds {
				t.Fatalf("go-sixel bounds = %v, want %v", goDecoded.Bounds(), wantBounds)
			}
			if libDecoded.Bounds() != wantBounds {
				t.Fatalf("libsixel bounds = %v, want %v", libDecoded.Bounds(), wantBounds)
			}
			tolerance := decoderChannelTolerance
			if tc.exact {
				tolerance = 0
			}
			assertImagesNear(t, libDecoded, goDecoded, tolerance)
			if tc.exact {
				assertImagesNear(t, goDecoded, opaqueRGBA(tc.img), 0)
			}
			if tc.adaptive {
				source := opaqueRGBA(tc.img)
				goPSNR, libPSNR := metrics.PSNR(goDecoded, source), metrics.PSNR(libDecoded, source)
				goSSIM, libSSIM := metrics.SSIM(goDecoded, source), metrics.SSIM(libDecoded, source)
				t.Logf("source-relative: go-sixel PSNR=%.2f dB SSIM=%.5f; libsixel PSNR=%.2f dB SSIM=%.5f", goPSNR, goSSIM, libPSNR, libSSIM)
				if goPSNR < 24 || libPSNR < 24 || goSSIM < 0.90 || libSSIM < 0.90 {
					t.Fatalf("adaptive quality below floor: go PSNR=%.2f SSIM=%.5f, libsixel PSNR=%.2f SSIM=%.5f", goPSNR, goSSIM, libPSNR, libSSIM)
				}
			}
		})
	}
}

func renderPayload(t *testing.T, img image.Image) []byte {
	t.Helper()
	old := runtime.GOMAXPROCS(4)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
	var out bytes.Buffer
	var renderer sixel.Renderer
	if err := renderer.Render(render.Frame{Img: img}, &out); err != nil {
		t.Fatalf("render Sixel: %v", err)
	}
	return out.Bytes()
}

func decodeLibsixel(t *testing.T, tool string, payload []byte) *image.RGBA {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "input.sixel")
	output := filepath.Join(dir, "output.png")
	if err := os.WriteFile(input, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(tool, "-i", input, "-o", output)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sixel2png: %v: %s", err, combined)
	}
	f, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoded, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode libsixel PNG: %v", err)
	}
	return opaqueRGBA(decoded)
}

func assertCompletePayload(t *testing.T, payload []byte, width, height int) {
	t.Helper()
	if !bytes.HasPrefix(payload, []byte("\x1bP0;0;8q")) || !bytes.HasSuffix(payload, []byte("\x1b\\")) {
		t.Fatal("production output is not a complete Sixel DCS")
	}
	wantRaster := []byte(fmt.Sprintf("\"1;1;%d;%d", width, height))
	if !bytes.Contains(payload, wantRaster) {
		t.Fatalf("payload lacks raster dimensions %q", wantRaster)
	}
	if !bytes.Contains(payload, []byte{'#'}) {
		t.Fatal("payload lacks a palette definition")
	}
	for i := 0; i < len(payload); i++ {
		if payload[i] != '!' {
			continue
		}
		count := 0
		for i++; i < len(payload) && payload[i] >= '0' && payload[i] <= '9'; i++ {
			count = count*10 + int(payload[i]-'0')
		}
		if count < 4 || count > 8192 {
			t.Fatalf("invalid Sixel RLE count %d", count)
		}
	}
}

func assertImagesNear(t *testing.T, got, want *image.RGBA, tolerance int) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds differ: got %v, want %v", got.Bounds(), want.Bounds())
	}
	for y := got.Bounds().Min.Y; y < got.Bounds().Max.Y; y++ {
		for x := got.Bounds().Min.X; x < got.Bounds().Max.X; x++ {
			g, w := got.RGBAAt(x, y), want.RGBAAt(x, y)
			if channelDelta(g.R, w.R) > tolerance || channelDelta(g.G, w.G) > tolerance || channelDelta(g.B, w.B) > tolerance || g.A != w.A {
				t.Fatalf("pixel (%d,%d) = %#v, want %#v within %d per RGB channel", x, y, g, w, tolerance)
			}
		}
	}
}

func channelDelta(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func opaqueRGBA(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	return dst
}

func loadPNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func flatFixture(width, height int) *image.Paletted {
	palette := color.Palette{color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}, color.RGBA{R: 0, G: 0, B: 0xff, A: 0xff}}
	img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for y := range height {
		for x := range width {
			if x > width/3 && x < width*2/3 && y > 2 && y < height-2 {
				img.SetColorIndex(x, y, 1)
			}
		}
	}
	return img
}

func sparsePaletteFixture() *image.Paletted {
	palette := make(color.Palette, 256)
	for i := range palette {
		palette[i] = color.RGBA{A: 0xff}
	}
	palette[0] = color.RGBA{R: 0xff, A: 0xff}
	palette[255] = color.RGBA{G: 0xff, A: 0xff}
	img := image.NewPaletted(image.Rect(0, 0, 67, 7), palette)
	img.SetColorIndex(2, 0, 255)
	img.SetColorIndex(66, 6, 255)
	return img
}

func runFixture(width int) *image.Paletted {
	palette := color.Palette{color.RGBA{R: 0xff, G: 0xff, A: 0xff}}
	return image.NewPaletted(image.Rect(0, 0, width, 7), palette)
}

func photoFixture(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x*29 + y*3),
				G: uint8(x*7 + y*31),
				B: uint8(x*13 + y*17),
				A: 0xff,
			})
		}
	}
	return img
}
