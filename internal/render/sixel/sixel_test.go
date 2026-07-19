package sixel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	gosixel "github.com/mattn/go-sixel"
	xdraw "golang.org/x/image/draw"

	"github.com/wimpysworld/nyala/internal/render"
)

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// knownFrame builds a small two-colour RGBA frame for the payload tests.
func knownFrame(t *testing.T) render.Frame {
	t.Helper()
	const w, h = 8, 12
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	blue := color.RGBA{R: 0x00, G: 0x00, B: 0xff, A: 0xff}
	for y := range h {
		for x := range w {
			if y < h/2 {
				img.SetRGBA(x, y, red)
			} else {
				img.SetRGBA(x, y, blue)
			}
		}
	}
	return render.Frame{Img: img}
}

func TestName(t *testing.T) {
	var r Renderer
	if got := r.Name(); got != "sixel" {
		t.Fatalf("Name() = %q, want %q", got, "sixel")
	}
}

func TestRenderEmitsFramedDCSPayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(knownFrame(t), &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "\x1bP0;0;8q") {
		t.Errorf("payload does not start with parameterised Sixel DCS introducer: %q", out[:min(len(out), 12)])
	}
	if !strings.HasSuffix(out, "\x1b\\") {
		t.Errorf("payload does not end with String Terminator ESC backslash")
	}

	body := strings.TrimSuffix(strings.TrimPrefix(out, "\x1bP0;0;8q"), "\x1b\\")
	if len(body) == 0 {
		t.Fatalf("payload body is empty, want non-empty Sixel data")
	}
	// The encoder body opens with the raster attribute introducer.
	if body[0] != '"' {
		t.Errorf("body does not start with raster attribute %q, got %q", '"', body[0])
	}
	// A palette colour introducer must appear somewhere in the body.
	if !strings.ContainsRune(body, '#') {
		t.Errorf("body has no colour introducer %q", '#')
	}
}

func TestRenderZeroAreaImageEmitsEmptyPayload(t *testing.T) {
	t.Parallel()
	for _, bounds := range []image.Rectangle{
		image.Rect(0, 0, 0, 4),
		image.Rect(0, 0, 4, 0),
	} {
		var r Renderer
		var buf bytes.Buffer
		if err := r.Render(render.Frame{Img: image.NewRGBA(bounds)}, &buf); err != nil {
			t.Fatalf("Render(%v) = %v, want nil", bounds, err)
		}
		if got := buf.String(); got != emptyPayload {
			t.Fatalf("Render(%v) = %q, want %q", bounds, got, emptyPayload)
		}
	}
}

func TestAppendRunRepeatBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		count int
		want  string
	}{
		{count: 0, want: ""},
		{count: 1, want: "@"},
		{count: 2, want: "@@"},
		{count: 3, want: "@@@"},
		{count: 4, want: "!4@"},
		{count: 254, want: "!254@"},
		{count: 255, want: "!255@"},
		{count: 256, want: "!256@"},
		{count: repeatLimit - 1, want: "!8191@"},
		{count: repeatLimit, want: "!8192@"},
		{count: repeatLimit + 1, want: "!8192@@"},
		{count: repeatLimit * 2, want: "!8192@!8192@"},
		{count: repeatLimit*2 + 3, want: "!8192@!8192@@@@"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.count), func(t *testing.T) {
			got := appendRun(nil, 1, tt.count)
			if string(got) != tt.want {
				t.Fatalf("appendRun count %d = %q, want %q", tt.count, got, tt.want)
			}
		})
	}
}

func TestLongRunsPreserveBandsAndPixels(t *testing.T) {
	t.Parallel()
	const width, height = repeatLimit + 9, 7
	palette := color.Palette{color.RGBA{R: 0xff, A: 0xff}}
	src := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	var payload bytes.Buffer
	if err := newOpaqueEncoder(&payload).encode(src); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if want := "#0!8192~!9~-#0!8192@!9@"; !bytes.Contains(payload.Bytes(), []byte(want)) {
		t.Fatalf("payload does not preserve the expected run, band, and colour delimiters: want fragment %q", want)
	}
	for _, count := range sixelRepeatCounts(payload.Bytes()) {
		if count > repeatLimit {
			t.Fatalf("emitted repeat %d exceeds limit %d", count, repeatLimit)
		}
	}
	var decodedImage image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(payload.Bytes())).Decode(&decodedImage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	decoded := image.NewRGBA(src.Bounds())
	draw.Draw(decoded, decoded.Bounds(), decodedImage, decodedImage.Bounds().Min, draw.Src)
	var baselinePayload bytes.Buffer
	baseline := gosixel.NewEncoder(&baselinePayload)
	baseline.Colors = 256
	if err := baseline.Encode(src); err != nil {
		t.Fatalf("encode 255 baseline: %v", err)
	}
	var baselineImage image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(baselinePayload.Bytes())).Decode(&baselineImage); err != nil {
		t.Fatalf("decode 255 baseline: %v", err)
	}
	baselineRGBA := image.NewRGBA(src.Bounds())
	draw.Draw(baselineRGBA, baselineRGBA.Bounds(), baselineImage, baselineImage.Bounds().Min, draw.Src)
	if !bytes.Equal(decoded.Pix, baselineRGBA.Pix) {
		t.Fatal("decoded pixels differ from the 255-split baseline")
	}
	want := image.NewRGBA(src.Bounds())
	draw.Draw(want, want.Bounds(), src, src.Bounds().Min, draw.Src)
	if !bytes.Equal(decoded.Pix, want.Pix) {
		t.Fatal("decoded pixels differ from the source")
	}
}

func TestLongRunWarmEncodeDoesNotAllocate(t *testing.T) {
	palette := color.Palette{color.Black}
	src := image.NewPaletted(image.Rect(0, 0, repeatLimit+9, 7), palette)
	enc := newOpaqueEncoder(io.Discard)
	if err := enc.encode(src); err != nil {
		t.Fatal(err)
	}
	var encodeErr error
	allocs := testing.AllocsPerRun(10, func() {
		if encodeErr == nil {
			encodeErr = enc.encode(src)
		}
	})
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if allocs != 0 {
		t.Fatalf("warm long-run encode allocs = %.0f, want 0", allocs)
	}
}

func TestRightmostPlaneBoundMatchesFullScan(t *testing.T) {
	t.Parallel()
	palette := make(color.Palette, 256)
	for i := range palette {
		palette[i] = color.RGBA{A: 0xff}
	}
	palette[0] = color.RGBA{R: 0xff, A: 0xff}
	palette[1] = color.RGBA{G: 0xff, A: 0xff}
	palette[255] = color.RGBA{B: 0xff, A: 0xff}
	tests := map[string]*image.Paletted{
		"right-edge": rightBoundFixture(image.Rect(0, 0, 37, 6), palette, func(x, _ int) uint8 {
			if x == 36 {
				return 1
			}
			return 0
		}),
		"sparse-zero-tail": rightBoundFixture(image.Rect(0, 0, 37, 6), palette, func(x, _ int) uint8 {
			if x == 3 {
				return 1
			}
			return 0
		}),
		"full-width-planes": rightBoundFixture(image.Rect(0, 0, 37, 6), palette, alternatingIndex),
		"odd-height":        rightBoundFixture(image.Rect(0, 0, 37, 7), palette, alternatingIndex),
		"indices-0-255": rightBoundFixture(image.Rect(0, 0, 37, 6), palette, func(x, _ int) uint8 {
			if x == 5 {
				return 255
			}
			return 0
		}),
		"non-zero-bounds": rightBoundFixture(image.Rect(3, 5, 40, 12), palette, alternatingIndex),
	}
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			assertRightBoundMatchesFullScan(t, newOpaqueEncoder(io.Discard), src)
		})
	}
}

func TestRightmostPlaneBoundResetsOnEncoderReuse(t *testing.T) {
	palette := color.Palette{
		color.RGBA{R: 0xff, A: 0xff},
		color.RGBA{B: 0xff, A: 0xff},
	}
	full := rightBoundFixture(image.Rect(0, 0, 61, 6), palette, alternatingIndex)
	sparse := rightBoundFixture(image.Rect(0, 0, 23, 7), palette, func(x, _ int) uint8 {
		if x == 2 {
			return 1
		}
		return 0
	})
	var out bytes.Buffer
	enc := newOpaqueEncoder(&out)
	assertRightBoundMatchesFullScan(t, enc, full)
	out.Reset()
	assertRightBoundMatchesFullScan(t, enc, sparse)
}

func alternatingIndex(x, y int) uint8 {
	if (x+y)%2 == 0 {
		return 0
	}
	return 1
}

func rightBoundFixture(bounds image.Rectangle, palette color.Palette, index func(x, y int) uint8) *image.Paletted {
	img := image.NewPaletted(bounds, palette)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			img.SetColorIndex(x, y, index(x-bounds.Min.X, y-bounds.Min.Y))
		}
	}
	return img
}

func assertRightBoundMatchesFullScan(t *testing.T, enc *opaqueEncoder, src *image.Paletted) {
	t.Helper()
	var got bytes.Buffer
	enc.w = &got
	if err := enc.encode(src); err != nil {
		t.Fatalf("bounded encode: %v", err)
	}
	var want bytes.Buffer
	fullScan := gosixel.NewEncoder(&want)
	fullScan.Colors = 256
	if err := fullScan.Encode(src); err != nil {
		t.Fatalf("full-scan encode: %v", err)
	}
	if !bytes.Equal(got.Bytes(), omitRedundantBandCR(want.Bytes())) {
		t.Fatalf("bounded output differs from full scan: got %d bytes, want %d", got.Len(), want.Len())
	}
	var decoded image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(got.Bytes())).Decode(&decoded); err != nil {
		t.Fatalf("decode bounded output: %v", err)
	}
	gotRGBA := image.NewRGBA(image.Rect(0, 0, src.Bounds().Dx(), src.Bounds().Dy()))
	draw.Draw(gotRGBA, gotRGBA.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	wantRGBA := image.NewRGBA(gotRGBA.Bounds())
	draw.Draw(wantRGBA, wantRGBA.Bounds(), src, src.Bounds().Min, draw.Src)
	if !bytes.Equal(gotRGBA.Pix, wantRGBA.Pix) {
		t.Fatal("bounded output decodes to different pixels")
	}
}

func omitRedundantBandCR(payload []byte) []byte {
	return bytes.ReplaceAll(payload, []byte("-$#"), []byte("-#"))
}

func sixelRepeatCounts(payload []byte) []int {
	var counts []int
	for i := 0; i < len(payload); i++ {
		if payload[i] != '!' {
			continue
		}
		count := 0
		for i++; i < len(payload) && payload[i] >= '0' && payload[i] <= '9'; i++ {
			count = count*10 + int(payload[i]-'0')
		}
		counts = append(counts, count)
	}
	return counts
}

func TestRenderNilImageEmitsEmptyFramedPayload(t *testing.T) {
	var r Renderer
	var buf bytes.Buffer
	if err := r.Render(render.Frame{}, &buf); err != nil {
		t.Fatalf("Render() = %v, want nil", err)
	}
	if got := buf.String(); got != "\x1bP0;0;8q\x1b\\" {
		t.Fatalf("nil-image payload = %q, want framed-but-empty %q", got, "\x1bP0;0;8q\x1b\\")
	}
}

var errTestWriter = errors.New("test writer failure")

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errTestWriter }

func TestRenderReusesEncoderAndRecoversFromWriterError(t *testing.T) {
	var r Renderer
	if err := r.Render(knownFrame(t), errorWriter{}); !errors.Is(err, errTestWriter) {
		t.Fatalf("Render() error = %v, want %v", err, errTestWriter)
	}
	if r.dst.dst != nil {
		t.Fatal("renderer retained the failed writer")
	}
	enc := r.enc
	if enc == nil {
		t.Fatal("renderer did not initialise its persistent encoder")
	}

	var first, second bytes.Buffer
	if err := r.Render(knownFrame(t), &first); err != nil {
		t.Fatalf("Render() after writer error: %v", err)
	}
	if err := r.Render(knownFrame(t), &second); err != nil {
		t.Fatalf("second Render(): %v", err)
	}
	if r.enc != enc {
		t.Fatal("renderer replaced its persistent encoder")
	}
	if r.dst.dst != nil {
		t.Fatal("renderer retained the successful writer")
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("repeated render output differs")
	}
}

func TestRenderReusesFrameScratchAndReplacesItOnResize(t *testing.T) {
	frame := render.Frame{Img: scratchYCbCr(image.Rect(3, 5, 35, 23))}
	var r Renderer
	var first, second bytes.Buffer
	if err := r.Render(frame, &first); err != nil {
		t.Fatal(err)
	}
	rgba, paletted := r.enc.rgba, r.enc.paletted
	if rgba == nil || paletted == nil || len(r.enc.lut) != 1<<15 {
		t.Fatal("renderer did not retain complete frame scratch")
	}
	if cap(rgba.Pix) != 32*18*4 || cap(paletted.Pix) != 32*18 || cap(r.enc.lut) != 1<<15 {
		t.Fatalf("retained capacities = RGBA %d, Paletted %d, LUT %d", cap(rgba.Pix), cap(paletted.Pix), cap(r.enc.lut))
	}
	if err := r.Render(frame, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Bytes(), first.Bytes()) {
		t.Fatal("reused frame scratch changed repeated payload")
	}
	if r.enc.rgba != rgba || r.enc.paletted != paletted {
		t.Fatal("same-size frame replaced retained image scratch")
	}

	resized := render.Frame{Img: scratchYCbCr(image.Rect(0, 0, 19, 11))}
	var want, got bytes.Buffer
	var fresh Renderer
	if err := fresh.Render(resized, &want); err != nil {
		t.Fatal(err)
	}
	if err := r.Render(resized, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatal("resized frame payload differs from fresh encoder")
	}
	if r.enc.rgba == rgba || r.enc.paletted == paletted {
		t.Fatal("resize retained stale image scratch")
	}
	if cap(r.enc.rgba.Pix) != 19*11*4 || cap(r.enc.paletted.Pix) != 19*11 {
		t.Fatalf("resized capacities = RGBA %d, Paletted %d", cap(r.enc.rgba.Pix), cap(r.enc.paletted.Pix))
	}
}

func scratchYCbCr(bounds image.Rectangle) *image.YCbCr {
	img := image.NewYCbCr(bounds, image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = uint8(i*29 + 17) //nolint:gosec // Deliberate wrapping test pattern.
	}
	for i := range img.Cb {
		img.Cb[i] = uint8(i*13 + 41) //nolint:gosec // Deliberate wrapping test pattern.
		img.Cr[i] = uint8(i*7 + 83)  //nolint:gosec // Deliberate wrapping test pattern.
	}
	return img
}

func TestRenderConcurrentCallsAreDeterministic(t *testing.T) {
	var r Renderer
	var want bytes.Buffer
	if err := r.Render(knownFrame(t), &want); err != nil {
		t.Fatalf("reference Render(): %v", err)
	}

	const workers = 8
	outputs := make([][]byte, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if err := r.Render(knownFrame(t), &out); err != nil {
				errs <- err
				return
			}
			outputs[worker] = append([]byte(nil), out.Bytes()...)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for worker, output := range outputs {
		if !bytes.Equal(output, want.Bytes()) {
			t.Fatalf("worker %d output differs", worker)
		}
	}
}

func TestOpaqueEncoderMatchesGoSixel(t *testing.T) {
	t.Parallel()
	bounds := image.Rect(3, 5, 83, 53)
	rgba := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			rgba.SetRGBA(x, y, color.RGBA{
				R: uint8(x*17 + y*3), //nolint:gosec // Overflow creates the test pattern.
				G: uint8(x*5 + y*11), //nolint:gosec // Overflow creates the test pattern.
				B: uint8(x*7 + y*13), //nolint:gosec // Overflow creates the test pattern.
				A: 0xff,
			})
		}
	}
	nrgba := image.NewNRGBA(bounds)
	draw.Draw(nrgba, bounds, rgba, bounds.Min, draw.Src)
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, rgba, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	ycbcr, err := jpeg.Decode(&jpegBytes)
	if err != nil {
		t.Fatal(err)
	}

	for name, img := range map[string]image.Image{
		"rgba":  rgba,
		"nrgba": nrgba,
		"ycbcr": ycbcr,
	} {
		t.Run(name, func(t *testing.T) {
			assertOpaque(t, img)
			var want bytes.Buffer
			upstream := gosixel.NewEncoder(&want)
			upstream.Colors = 256
			if err := upstream.Encode(img); err != nil {
				t.Fatalf("upstream Encode(): %v", err)
			}
			var got bytes.Buffer
			if err := newOpaqueEncoder(&got).encode(img); err != nil {
				t.Fatalf("opaque encode(): %v", err)
			}
			if !bytes.Equal(got.Bytes(), omitRedundantBandCR(want.Bytes())) {
				t.Fatalf("opaque payload differs from go-sixel v0.0.12: got %d bytes, want %d", got.Len(), want.Len())
			}
		})
	}
}

func TestSixelWorkerCount(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldProcs) })
	if got := sixelWorkerCount(); got != 1 {
		t.Fatalf("sixelWorkerCount() = %d, want 1", got)
	}
	runtime.GOMAXPROCS(8)
	if got := sixelWorkerCount(); got != 4 {
		t.Fatalf("sixelWorkerCount() = %d, want capped count 4", got)
	}
}

func TestParallelMappingActivationBoundaries(t *testing.T) {
	tests := []struct {
		name            string
		pixels, workers int
		want            bool
	}{
		{name: "one-worker", pixels: parallelMapPixelThreshold, workers: 1},
		{name: "below-threshold", pixels: parallelMapPixelThreshold - 1, workers: 4},
		{name: "at-threshold", pixels: parallelMapPixelThreshold, workers: 4, want: true},
		{name: "above-threshold", pixels: parallelMapPixelThreshold + 1, workers: 4, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parallelMapping(test.pixels, test.workers); got != test.want {
				t.Fatalf("parallelMapping(%d, %d) = %t, want %t", test.pixels, test.workers, got, test.want)
			}
		})
	}
}

func TestParallelMappingMatchesSerial(t *testing.T) {
	palette := mappingTestPalette()
	lut := serialPaletteLUT(palette)
	bounds := image.Rect(11, 7, 1012, 507)
	storage := image.NewRGBA(bounds.Inset(-3))
	src := storage.SubImage(bounds).(*image.RGBA)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			src.SetRGBA(x, y, color.RGBA{
				R: uint8(x*17 + y*3), //nolint:gosec // Wrapping builds an RGB555 mapping fixture.
				G: uint8(x*5 + y*11), //nolint:gosec // Wrapping builds an RGB555 mapping fixture.
				B: uint8(x*7 + y*13), //nolint:gosec // Wrapping builds an RGB555 mapping fixture.
				A: 0xff,
			})
		}
	}
	want := image.NewPaletted(bounds, palette)
	mapPalettedRows(want, src, lut, bounds.Min.Y, bounds.Max.Y)

	oldProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldProcs) })
	for _, workers := range []int{1, 4} {
		runtime.GOMAXPROCS(workers)
		storage := image.NewPaletted(bounds.Inset(-5), palette)
		got := storage.SubImage(bounds).(*image.Paletted)
		mapPaletted(got, src, lut)
		if !palettedIndexesEqual(got, want) {
			t.Fatalf("workers=%d mapped indexes differ", workers)
		}
		mapPaletted(got, src, lut)
		if !palettedIndexesEqual(got, want) {
			t.Fatalf("workers=%d repeated mapped indexes differ", workers)
		}
	}
}

func TestParallelMappingProductionPayloadMatchesSerial(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(4)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldProcs) })
	palette := mappingTestPalette()
	lut := serialPaletteLUT(palette)
	bounds := image.Rect(3, 5, 1283, 725)
	srcStorage := image.NewRGBA(bounds.Inset(-2))
	src := srcStorage.SubImage(bounds).(*image.RGBA)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			src.SetRGBA(x, y, color.RGBA{
				R: uint8(x*29 + y*3),  //nolint:gosec // Wrapping builds an adaptive-photo fixture.
				G: uint8(x*7 + y*31),  //nolint:gosec // Wrapping builds an adaptive-photo fixture.
				B: uint8(x*13 + y*17), //nolint:gosec // Wrapping builds an adaptive-photo fixture.
				A: 0xff,
			})
		}
	}
	serial := image.NewPaletted(bounds, palette)
	mapPalettedRows(serial, src, lut, bounds.Min.Y, bounds.Max.Y)
	candidate := image.NewPaletted(bounds, palette)
	mapPaletted(candidate, src, lut)
	if !bytes.Equal(candidate.Pix, serial.Pix) {
		t.Fatal("production mapped indexes differ")
	}
	serialPayload := bandTestPayload(serial, newOpaqueEncoder(io.Discard).appendBands(nil, serial))
	enc := newOpaqueEncoder(io.Discard)
	candidatePayload := bandTestPayload(candidate, enc.appendBands(nil, candidate))
	serialHash := sha256.Sum256(serialPayload)
	if gotHash := sha256.Sum256(candidatePayload); gotHash != serialHash {
		t.Fatalf("combined payload SHA-256 = %x, want %x", gotHash, serialHash)
	}
	if len(enc.band) != sixelWorkerCount() {
		t.Fatalf("combined band workers = %d, want shared budget %d", len(enc.band), sixelWorkerCount())
	}
	for run := range 3 {
		again := image.NewPaletted(bounds, palette)
		mapPaletted(again, src, lut)
		payload := bandTestPayload(again, newOpaqueEncoder(io.Discard).appendBands(nil, again))
		if !bytes.Equal(payload, serialPayload) {
			t.Fatalf("combined repeated payload %d differs", run)
		}
	}
	var serialDecoded, candidateDecoded image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(serialPayload)).Decode(&serialDecoded); err != nil {
		t.Fatalf("decode serial payload: %v", err)
	}
	if err := gosixel.NewDecoder(bytes.NewReader(candidatePayload)).Decode(&candidateDecoded); err != nil {
		t.Fatalf("decode combined payload: %v", err)
	}
	for y := range bounds.Dy() {
		for x := range bounds.Dx() {
			if serialDecoded.At(x, y) != candidateDecoded.At(x, y) {
				t.Fatalf("decoded pixel (%d,%d) differs", x, y)
			}
		}
	}
}

func mappingTestPalette() color.Palette {
	palette := make(color.Palette, 256)
	for i := range palette {
		palette[i] = color.RGBA{R: uint8(i), G: uint8(i * 37), B: uint8(i * 73), A: 0xff} //nolint:gosec // Overflow creates the mapping fixture palette.
	}
	return palette
}

func palettedIndexesEqual(a, b *image.Paletted) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	bounds := a.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		aOffset := a.PixOffset(bounds.Min.X, y)
		bOffset := b.PixOffset(bounds.Min.X, y)
		if !bytes.Equal(a.Pix[aOffset:aOffset+bounds.Dx()], b.Pix[bOffset:bOffset+bounds.Dx()]) {
			return false
		}
	}
	return true
}

func TestParallelBandActivationBoundaries(t *testing.T) {
	tests := []struct {
		name                           string
		colours, width, bands, workers int
		want                           bool
	}{
		{name: "one-worker", colours: 256, width: 1280, bands: 120, workers: 1},
		{name: "below-band-threshold", colours: 256, width: 4096, bands: 119, workers: 4},
		{name: "below-work-threshold", colours: 255, width: 1274, bands: 120, workers: 4},
		{name: "at-work-threshold", colours: 250, width: 1300, bands: 120, workers: 4, want: true},
		{name: "above-thresholds", colours: 256, width: 1280, bands: 120, workers: 4, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parallelBands(test.colours, test.width, test.bands, test.workers); got != test.want {
				t.Fatalf("parallelBands(%d, %d, %d, %d) = %t, want %t", test.colours, test.width, test.bands, test.workers, got, test.want)
			}
		})
	}
}

func TestParallelBandsMatchSerial(t *testing.T) {
	tests := []struct {
		name   string
		bounds image.Rectangle
	}{
		{name: "complete-bands", bounds: image.Rect(0, 0, 97, 18)},
		{name: "partial-final-band", bounds: image.Rect(0, 0, 97, 19)},
		{name: "non-zero-bounds-and-stride", bounds: image.Rect(11, 7, 108, 26)},
		{name: "repeat-ceiling", bounds: image.Rect(3, 5, repeatLimit+12, 24)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			img := bandTestImage(test.bounds)
			bands := (test.bounds.Dy() + 5) / 6
			serial := appendBandRange(nil, img, &bandScratch{}, 0, bands)
			serialHash := sha256.Sum256(serial)
			for _, workers := range []int{1, 4} {
				enc := newOpaqueEncoder(io.Discard)
				got := enc.appendBandsParallel(nil, img, workers)
				if gotHash := sha256.Sum256(got); gotHash != serialHash {
					t.Fatalf("workers=%d payload SHA-256 = %x, want %x", workers, gotHash, serialHash)
				}
				again := enc.appendBandsParallel(nil, img, workers)
				if !bytes.Equal(again, serial) {
					t.Fatalf("workers=%d repeated payload differs", workers)
				}
			}

			payload := bandTestPayload(img, serial)
			var decoded image.Image
			if err := gosixel.NewDecoder(bytes.NewReader(payload)).Decode(&decoded); err != nil {
				t.Fatalf("decode parallel-equivalent payload: %v", err)
			}
			for y := range test.bounds.Dy() {
				for x := range test.bounds.Dx() {
					want := color.RGBAModel.Convert(img.At(test.bounds.Min.X+x, test.bounds.Min.Y+y)).(color.RGBA)
					got := color.RGBAModel.Convert(decoded.At(x, y)).(color.RGBA)
					if got != want {
						t.Fatalf("decoded pixel (%d,%d) = %#v, want %#v", x, y, got, want)
					}
				}
			}
		})
	}
}

func TestParallelBandsRetainWorkerScratch(t *testing.T) {
	img := bandTestImage(image.Rect(0, 0, 160, 25))
	enc := newOpaqueEncoder(io.Discard)
	want := append([]byte(nil), enc.appendBandsParallel(nil, img, 4)...)
	type addresses struct {
		out, bitset *byte
		seen        *uint16
		right       *int
	}
	before := make([]addresses, len(enc.band))
	for i := range enc.band {
		before[i] = addresses{&enc.band[i].out[0], &enc.band[i].bitset[0], &enc.band[i].seen[0], &enc.band[i].right[0]}
	}
	got := enc.appendBandsParallel(nil, img, 4)
	if !bytes.Equal(got, want) {
		t.Fatal("reused worker payload differs")
	}
	for i := range enc.band {
		after := addresses{&enc.band[i].out[0], &enc.band[i].bitset[0], &enc.band[i].seen[0], &enc.band[i].right[0]}
		if after != before[i] {
			t.Fatalf("worker %d replaced retained scratch", i)
		}
	}
}

func TestParallelBandsProductionPathMatchesSerial(t *testing.T) {
	img := bandBenchmarkImage(1280, 720, 256, 17)
	serial := appendBandRange(nil, img, &bandScratch{}, 0, 120)
	enc := newOpaqueEncoder(io.Discard)
	got := enc.appendBands(nil, img)
	if !bytes.Equal(got, serial) {
		t.Fatalf("production parallel payload SHA-256 = %x, want serial %x", sha256.Sum256(got), sha256.Sum256(serial))
	}
	if len(enc.band) != sixelWorkerCount() {
		t.Fatalf("production worker batches = %d, want %d", len(enc.band), sixelWorkerCount())
	}
}

func TestBandAdvanceOmitsRedundantCarriageReturn(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
	}{
		{name: "720p", width: 1280, height: 720},
		{name: "1080p", width: 1920, height: 1080},
		{name: "1600p", width: 2560, height: 1600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			palette := color.Palette{color.RGBA{A: 0xff}}
			img := image.NewPaletted(image.Rect(0, 0, test.width, test.height), palette)
			candidate := bandTestPayload(img, newOpaqueEncoder(io.Discard).appendBands(nil, img))
			if bytes.Contains(candidate, []byte("-$")) {
				t.Fatal("band advance retains a redundant carriage return")
			}

			legacy := bytes.ReplaceAll(candidate, []byte("-#"), []byte("-$#"))
			bandBoundaries := (test.height+5)/6 - 1
			if saving := len(legacy) - len(candidate); saving != bandBoundaries {
				t.Fatalf("payload saving = %d bytes, want %d", saving, bandBoundaries)
			}
			if got := bytes.Count(candidate, []byte("-#")); got != bandBoundaries {
				t.Fatalf("band advances = %d, want %d", got, bandBoundaries)
			}

			var legacyDecoded, candidateDecoded image.Image
			if err := gosixel.NewDecoder(bytes.NewReader(legacy)).Decode(&legacyDecoded); err != nil {
				t.Fatalf("decode legacy payload: %v", err)
			}
			if err := gosixel.NewDecoder(bytes.NewReader(candidate)).Decode(&candidateDecoded); err != nil {
				t.Fatalf("decode candidate payload: %v", err)
			}
			if legacyDecoded.Bounds() != candidateDecoded.Bounds() {
				t.Fatalf("decoded bounds = %v, want %v", candidateDecoded.Bounds(), legacyDecoded.Bounds())
			}
			for y := range test.height {
				for x := range test.width {
					if legacyDecoded.At(x, y) != candidateDecoded.At(x, y) {
						t.Fatalf("decoded pixel (%d,%d) differs", x, y)
					}
				}
			}
		})
	}
}

func TestParallelBandsReturnWriterError(t *testing.T) {
	img := bandBenchmarkImage(1280, 720, 256, 17)
	if err := newOpaqueEncoder(errorWriter{}).encode(img); !errors.Is(err, errTestWriter) {
		t.Fatalf("parallel encode error = %v, want %v", err, errTestWriter)
	}
}

func bandTestImage(bounds image.Rectangle) *image.Paletted {
	palette := color.Palette{
		color.RGBA{R: 0xff, A: 0xff},
		color.RGBA{G: 0xff, A: 0xff},
		color.RGBA{B: 0xff, A: 0xff},
		color.RGBA{R: 0xff, G: 0xff, A: 0xff},
		color.RGBA{R: 0xff, B: 0xff, A: 0xff},
		color.RGBA{G: 0xff, B: 0xff, A: 0xff},
		color.RGBA{R: 0x33, G: 0x66, B: 0x99, A: 0xff},
		color.RGBA{R: 0x99, G: 0x66, B: 0x33, A: 0xff},
		color.RGBA{A: 0xff}, // Deliberately absent.
	}
	storage := image.NewPaletted(bounds.Inset(-3), palette)
	img := storage.SubImage(bounds).(*image.Paletted)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			idx := uint8((x*7 + y*11 + x*y) % (len(palette) - 1)) //nolint:gosec // The palette bounds the value.
			img.SetColorIndex(x, y, idx)
		}
	}
	return img
}

func bandTestPayload(img *image.Paletted, bands []byte) []byte {
	bounds := img.Bounds()
	payload := append([]byte(nil), "\033P0;0;8q\"1;1;"...)
	payload = fmt.Appendf(payload, "%d;%d", bounds.Dx(), bounds.Dy())
	for n, colour := range img.Palette {
		r, g, b, _ := colour.RGBA()
		payload = appendColourRegister(payload, n, r*100/0xffff, g*100/0xffff, b*100/0xffff)
	}
	payload = append(payload, bands...)
	return append(payload, 0x1b, 0x5c)
}

func BenchmarkOpaqueEncoderComparison(b *testing.B) {
	file, err := os.Open("../../../testdata/linuxmatters.png")
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	src, err := png.Decode(file)
	if err != nil {
		b.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1600))
	xdraw.CatmullRom.Scale(img, img.Bounds(), src, src.Bounds(), draw.Src, nil)
	assertOpaque(b, img)

	var baseline, candidate bytes.Buffer
	upstream := gosixel.NewEncoder(&baseline)
	upstream.Colors = 256
	if err := upstream.Encode(img); err != nil {
		b.Fatal(err)
	}
	if err := newOpaqueEncoder(&candidate).encode(img); err != nil {
		b.Fatal(err)
	}
	baselineBytes := baseline.Len()
	candidateBytes := candidate.Len()

	b.Run("upstream-alpha", func(b *testing.B) {
		encoder := gosixel.NewEncoder(io.Discard)
		encoder.Colors = 256
		b.ReportAllocs()
		b.ReportMetric(float64(baselineBytes), "payload-B")
		for range b.N {
			if err := encoder.Encode(img); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("nyala-opaque", func(b *testing.B) {
		encoder := newOpaqueEncoder(io.Discard)
		b.ReportAllocs()
		b.ReportMetric(float64(candidateBytes), "payload-B")
		for range b.N {
			if err := encoder.encode(img); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func assertOpaque(tb testing.TB, img image.Image) {
	tb.Helper()
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := img.At(x, y).RGBA()
			if alpha != 0xffff {
				tb.Fatalf("pixel (%d,%d) alpha = %d, want 65535", x, y, alpha)
			}
		}
	}
}
