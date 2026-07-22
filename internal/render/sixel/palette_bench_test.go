package sixel

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/delthas/octreequant"
	goquant "github.com/ericpauley/go-quantize/quantize"
	"github.com/gen2brain/jpegn"
	gosixel "github.com/mattn/go-sixel"
	"github.com/soniakeys/quant/median"
	xdraw "golang.org/x/image/draw"

	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

const (
	paletteBenchEnv      = "NYALA_PALETTE_BENCH"
	paletteBenchRunEnv   = "NYALA_PALETTE_BENCH_RUN"
	paletteBenchSamples  = 3
	paletteBenchWarmups  = 4
	paletteColours       = 255
	combinedBenchEnv     = "NYALA_COMBINED_BENCH"
	combinedBenchRunEnv  = "NYALA_COMBINED_BENCH_RUN"
	combinedBenchSamples = 30
)

type paletteBenchFixture struct {
	name string
	path string
}

type paletteBenchSize struct {
	w int
	h int
}

type paletteBenchArm struct {
	name string
	kind string
}

type paletteBenchEncoder struct {
	arm      paletteBenchArm
	out      bytes.Buffer
	upstream *gosixel.Encoder
	nyala    *opaqueEncoder
}

type paletteBenchMeasurement struct {
	meanMS      float64
	p90MS       float64
	decodeMS    float64
	decodeP90MS float64
	allocs      float64
	bytesAlloc  uint64
	payload     []byte
	selectMS    float64
	lutMS       float64
	mapMS       float64
	emissionMS  float64
}

type paletteBenchAggregate struct {
	n            int
	meanMS       float64
	p90MS        float64
	decodeMS     float64
	decodeP90MS  float64
	allocs       float64
	bytesAlloc   uint64
	payloadBytes int
	selectionMS  float64
	mappingMS    float64
}

type combinedBenchArm struct {
	name             string
	parallelMapping  bool
	parallelEmission bool
}

type combinedBenchStages struct {
	palette  time.Duration
	lut      time.Duration
	mapping  time.Duration
	emission time.Duration
}

type combinedBenchStageSamples struct {
	palette  []time.Duration
	lut      []time.Duration
	mapping  []time.Duration
	emission []time.Duration
}

var combinedBenchArms = []combinedBenchArm{
	{name: "serial"},
	{name: "mapping-only", parallelMapping: true},
	{name: "bands-only", parallelEmission: true},
	{name: "combined", parallelMapping: true, parallelEmission: true},
}

var combinedBenchSizes = []paletteBenchSize{
	{w: 1280, h: 720},
	{w: 1920, h: 1080},
	{w: 2560, h: 1600},
}

func TestCombinedBenchArmSelection(t *testing.T) {
	want := map[string]struct {
		mapping  bool
		emission bool
	}{
		"serial":       {},
		"mapping-only": {mapping: true},
		"bands-only":   {emission: true},
		"combined":     {mapping: true, emission: true},
	}
	for run := range 6 {
		seen := make(map[string]bool, len(combinedBenchArms))
		for _, arm := range rotateCombinedArms(combinedBenchArms, run) {
			expect, ok := want[arm.name]
			if !ok {
				t.Fatalf("run %d selected unknown arm %q", run, arm.name)
			}
			if seen[arm.name] {
				t.Fatalf("run %d selected arm %q twice", run, arm.name)
			}
			seen[arm.name] = true
			if arm.parallelMapping != expect.mapping || arm.parallelEmission != expect.emission {
				t.Fatalf("arm %q = mapping %t, emission %t; want mapping %t, emission %t", arm.name, arm.parallelMapping, arm.parallelEmission, expect.mapping, expect.emission)
			}
		}
		if len(seen) != len(want) {
			t.Fatalf("run %d selected %d arms, want %d", run, len(seen), len(want))
		}
	}
}

func TestCombinedBenchArmsMatch(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(4)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldProcs) })
	src := combinedBenchWeb(1280, 720)
	var referencePayload []byte
	var referenceDecoded *image.RGBA
	for _, arm := range combinedBenchArms {
		var out bytes.Buffer
		if _, err := encodeCombinedBench(newOpaqueEncoder(&out), src, arm); err != nil {
			t.Fatalf("%s encode: %v", arm.name, err)
		}
		payload := out.Bytes()
		decoded := validatePalettePayload(t, payload, src.Bounds())
		if referencePayload == nil {
			referencePayload = append([]byte(nil), payload...)
			referenceDecoded = decoded
			continue
		}
		if !bytes.Equal(payload, referencePayload) {
			t.Fatalf("%s payload SHA-256 = %x, want %x", arm.name, sha256.Sum256(payload), sha256.Sum256(referencePayload))
		}
		if !bytes.Equal(decoded.Pix, referenceDecoded.Pix) {
			t.Fatalf("%s decoded pixels differ", arm.name)
		}
	}
}

// TestCombinedMappingBenchmark is the opt-in interaction screen for serial,
// mapping-only, bands-only, and combined execution through the adaptive PNG path.
func TestCombinedMappingBenchmark(t *testing.T) {
	if os.Getenv(combinedBenchEnv) != "1" {
		t.Skip("set " + combinedBenchEnv + "=1")
	}
	oldProcs := runtime.GOMAXPROCS(4)
	oldGC := debug.SetGCPercent(100)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(oldProcs)
		debug.SetGCPercent(oldGC)
	})
	run := envInt(combinedBenchRunEnv)
	fixtures := []string{"flat", "web", "photo"}
	fixtures = rotateCombinedFixtures(fixtures, run)
	arms := rotateCombinedArms(combinedBenchArms, run)
	for _, fixture := range fixtures {
		for _, size := range combinedBenchSizes {
			src := combinedBenchFixture(t, fixture, size)
			pngData := encodeCombinedBenchPNG(t, src)
			var referencePayload []byte
			var referenceDecoded *image.RGBA
			for _, arm := range arms {
				m := measureCombinedBenchArm(t, src, pngData, arm)
				decoded := validatePalettePayload(t, m.payload, src.Bounds())
				if referencePayload == nil {
					referencePayload = append([]byte(nil), m.payload...)
					referenceDecoded = decoded
				} else {
					if !bytes.Equal(m.payload, referencePayload) {
						t.Fatalf("%s %dx%d %s payload SHA-256 = %x, want %x", fixture, size.w, size.h, arm.name, sha256.Sum256(m.payload), sha256.Sum256(referencePayload))
					}
					if !bytes.Equal(decoded.Pix, referenceDecoded.Pix) {
						t.Fatalf("%s %dx%d %s decoded pixels differ", fixture, size.w, size.h, arm.name)
					}
				}
				t.Logf("INTERACTION run=%d fixture=%s size=%dx%d arm=%s full-mean-ms=%.3f full-p90-ms=%.3f png-mean-ms=%.3f png-p90-ms=%.3f palette-p90-ms=%.3f lut-p90-ms=%.3f mapping-p90-ms=%.3f emission-p90-ms=%.3f allocs=%.0f B/op=%d payload-B=%d sha256=%x decoded-equal=true",
					run, fixture, size.w, size.h, arm.name, m.meanMS, m.p90MS, m.decodeMS, m.decodeP90MS,
					m.selectMS, m.lutMS, m.mapMS, m.emissionMS, m.allocs, m.bytesAlloc, len(m.payload), sha256.Sum256(m.payload))
			}
		}
	}
}

func measureCombinedBenchArm(t *testing.T, src *image.RGBA, pngData []byte, arm combinedBenchArm) paletteBenchMeasurement {
	t.Helper()
	enc := newOpaqueEncoder(io.Discard)
	for range paletteBenchWarmups {
		if _, err := encodeCombinedBench(enc, src, arm); err != nil {
			t.Fatal(err)
		}
	}
	full := make([]time.Duration, combinedBenchSamples)
	pngPath := make([]time.Duration, combinedBenchSamples)
	stages := combinedBenchStageSamples{
		palette:  make([]time.Duration, combinedBenchSamples),
		lut:      make([]time.Duration, combinedBenchSamples),
		mapping:  make([]time.Duration, combinedBenchSamples),
		emission: make([]time.Duration, combinedBenchSamples),
	}
	for i := range combinedBenchSamples {
		start := time.Now()
		span, err := encodeCombinedBench(enc, src, arm)
		if err != nil {
			t.Fatal(err)
		}
		full[i] = time.Since(start)
		stages.palette[i] = span.palette
		stages.lut[i] = span.lut
		stages.mapping[i] = span.mapping
		stages.emission[i] = span.emission
		start = time.Now()
		decoded, err := png.Decode(bytes.NewReader(pngData))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := encodeCombinedBench(enc, decoded, arm); err != nil {
			t.Fatal(err)
		}
		pngPath[i] = time.Since(start)
	}
	var allocErr error
	allocs := testing.AllocsPerRun(3, func() {
		if allocErr == nil {
			_, allocErr = encodeCombinedBench(enc, src, arm)
		}
	})
	if allocErr != nil {
		t.Fatal(allocErr)
	}
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	before := mem.TotalAlloc
	for range 3 {
		if _, err := encodeCombinedBench(enc, src, arm); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&mem)
	var out bytes.Buffer
	if _, err := encodeCombinedBench(newOpaqueEncoder(&out), src, arm); err != nil {
		t.Fatal(err)
	}
	mean, p90 := paletteDurationStats(full)
	pngMean, pngP90 := paletteDurationStats(pngPath)
	_, paletteP90 := paletteDurationStats(stages.palette)
	_, lutP90 := paletteDurationStats(stages.lut)
	_, mappingP90 := paletteDurationStats(stages.mapping)
	_, emissionP90 := paletteDurationStats(stages.emission)
	return paletteBenchMeasurement{
		meanMS: durationMS(mean), p90MS: durationMS(p90),
		decodeMS: durationMS(pngMean), decodeP90MS: durationMS(pngP90),
		allocs: allocs, bytesAlloc: (mem.TotalAlloc - before) / 3, payload: out.Bytes(),
		selectMS: durationMS(paletteP90), lutMS: durationMS(lutP90),
		mapMS: durationMS(mappingP90), emissionMS: durationMS(emissionP90),
	}
}

func encodeCombinedBench(enc *opaqueEncoder, img image.Image, arm combinedBenchArm) (combinedBenchStages, error) {
	const colours = 255
	var stages combinedBenchStages
	rgba, ok := img.(*image.RGBA)
	if !ok {
		return stages, fmt.Errorf("unsupported benchmark image type %T", img)
	}
	start := time.Now()
	palette := samplePalette(rgba, colours)
	stages.palette = time.Since(start)
	paletted := image.NewPaletted(rgba.Bounds(), palette)
	start = time.Now()
	lut := newPaletteLUT(palette)
	stages.lut = time.Since(start)
	start = time.Now()
	if arm.parallelMapping {
		mapPaletted(paletted, rgba, lut)
	} else {
		bounds := rgba.Bounds()
		mapPalettedRows(paletted, rgba, lut, bounds.Min.Y, bounds.Max.Y)
	}
	stages.mapping = time.Since(start)

	bounds := paletted.Bounds()
	out := enc.outScratch[:0]
	if outCap := bounds.Dx()*bounds.Dy()/2 + len(palette)*16 + 64; cap(out) < outCap {
		out = make([]byte, 0, outCap)
	}
	out = append(out, "\033P0;0;8q\"1;1;"...)
	out = strconv.AppendInt(out, int64(bounds.Dx()), 10)
	out = append(out, ';')
	out = strconv.AppendInt(out, int64(bounds.Dy()), 10)
	for n, colour := range palette {
		r, g, b, _ := colour.RGBA()
		out = appendColourRegister(out, n, r*100/0xffff, g*100/0xffff, b*100/0xffff)
	}
	start = time.Now()
	if arm.parallelEmission {
		out = enc.appendBandsParallel(out, paletted, sixelWorkerCount())
	} else {
		enc.growBandScratch(1)
		out = appendBandRange(out, paletted, &enc.band[0], 0, (bounds.Dy()+5)/6)
	}
	stages.emission = time.Since(start)
	out = append(out, 0x1b, 0x5c)
	enc.outScratch = out[:0]
	_, err := enc.w.Write(out)
	return stages, err
}

func encodeCombinedBenchPNG(t *testing.T, src *image.RGBA) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, src); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func combinedBenchFlat(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			r := uint8(x & 0x1f)       //nolint:gosec // Bounded variation keeps this visually flat but adaptive.
			g := uint8(y & 0x1f)       //nolint:gosec // Bounded variation keeps this visually flat but adaptive.
			b := uint8((x + y) & 0x07) //nolint:gosec // Bounded variation keeps this visually flat but adaptive.
			img.SetRGBA(x, y, color.RGBA{R: 0x20 + r, G: 0x30 + g, B: 0x40 + b, A: 0xff})
		}
	}
	return img
}

func combinedBenchWeb(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	palette := mappingTestPalette()[:16]
	for y := range height {
		for x := range width {
			base := color.RGBAModel.Convert(palette[(x/32+y/18)%len(palette)]).(color.RGBA)
			variation := uint8((x*3 + y*5) & 0x1f) //nolint:gosec // Bounded texture forces the adaptive web path.
			img.SetRGBA(x, y, color.RGBA{R: base.R + variation, G: base.G + variation/2, B: base.B + variation/3, A: 0xff})
		}
	}
	return img
}

func combinedBenchFixture(t *testing.T, name string, size paletteBenchSize) *image.RGBA {
	t.Helper()
	switch name {
	case "flat":
		return combinedBenchFlat(size.w, size.h)
	case "web":
		return combinedBenchWeb(size.w, size.h)
	case "photo":
		return loadPaletteBenchFrame(t, "../../../testdata/linuxmatters.png", size)
	default:
		t.Fatalf("unknown combined benchmark fixture %q", name)
		return nil
	}
}

func rotateCombinedArms(values []combinedBenchArm, run int) []combinedBenchArm {
	out := append([]combinedBenchArm(nil), values...)
	shift := run % len(out)
	out = append(out[shift:], out[:shift]...)
	if run/len(out)%2 == 1 {
		sort.SliceStable(out, func(i, j int) bool { return i > j })
	}
	return out
}

func rotateCombinedFixtures(values []string, run int) []string {
	out := append([]string(nil), values...)
	shift := run % len(out)
	return append(out[shift:], out[:shift]...)
}

func BenchmarkRGB555Mapping(b *testing.B) {
	src := loadPaletteBenchFrame(b, "../../../testdata/linuxmatters.png", paletteBenchSize{w: 1920, h: 1080})
	palette := samplePalette(src, paletteColours)
	lut := newPaletteLUT(palette)
	dst := image.NewPaletted(src.Bounds(), palette)
	bounds := src.Bounds()
	for _, arm := range []struct {
		name     string
		parallel bool
	}{{name: "band-only"}, {name: "combined", parallel: true}} {
		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if arm.parallel {
					mapPaletted(dst, src, lut)
				} else {
					mapPalettedRows(dst, src, lut, bounds.Min.Y, bounds.Max.Y)
				}
			}
		})
	}
}

var paletteBenchFixtures = []paletteBenchFixture{
	{name: "linuxmatters", path: "../../../testdata/linuxmatters.png"},
	{name: "grayscale", path: "../../../testdata/testpage-01-grayscale.png"},
	{name: "basic", path: "../../../testdata/testpage-02-basic.png"},
	{name: "cmyk", path: "../../../testdata/testpage-03-cmyk.png"},
	{name: "colour", path: "../../../testdata/testpage-04-color.png"},
}

var paletteBenchSizes = []paletteBenchSize{{w: 1920, h: 1080}, {w: 2560, h: 1600}}

var paletteBenchArms = []paletteBenchArm{
	{name: "go-sixel", kind: "upstream"},
	{name: "nyala-serial", kind: "serial"},
	{name: "nyala-current", kind: "current"},
	{name: "sample-64k", kind: "64k"},
	{name: "go-quantize", kind: "goquant"},
	{name: "octreequant", kind: "octree"},
}

// TestPaletteBenchmark is an opt-in internal comparison of complete Sixel
// encoders. It never changes the production palette policy.
func TestPaletteBenchmark(t *testing.T) {
	if os.Getenv(paletteBenchEnv) != "1" {
		t.Skip("set " + paletteBenchEnv + "=1")
	}
	oldProcs := runtime.GOMAXPROCS(4)
	oldGC := debug.SetGCPercent(100)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(oldProcs)
		debug.SetGCPercent(oldGC)
	})

	run := envInt(paletteBenchRunEnv)
	arms := rotateArms(paletteBenchArms, run)
	fixtures := rotateFixtures(paletteBenchFixtures, run)
	aggregates := make(map[string]*paletteBenchAggregate, len(arms))
	for _, fixture := range fixtures {
		for _, size := range paletteBenchSizes {
			src := loadPaletteBenchFrame(t, fixture.path, size)
			jpegData := encodePaletteBenchJPEG(t, src)
			for _, arm := range arms {
				t.Run(fmt.Sprintf("%s/%dx%d/%s", fixture.name, size.w, size.h, arm.name), func(t *testing.T) {
					enc := newPaletteBenchEncoder(arm)
					m := measurePaletteArm(t, enc, src, jpegData)
					colours := countPaletteRegisters(m.payload)
					agg := aggregates[arm.name]
					if agg == nil {
						agg = &paletteBenchAggregate{}
						aggregates[arm.name] = agg
					}
					agg.add(m)
					if run != 0 {
						t.Logf("PALETTE run=%d fixture=%s size=%dx%d arm=%s mean-ms=%.3f p90-ms=%.3f decode-encode-mean-ms=%.3f decode-encode-p90-ms=%.3f payload-B=%d",
							run, fixture.name, size.w, size.h, arm.name, m.meanMS, m.p90MS, m.decodeMS, m.decodeP90MS, len(m.payload))
						return
					}
					decoded := validatePalettePayload(t, m.payload, src.Bounds())
					de := metrics.CIEDE2000(src, decoded)
					t.Logf("PALETTE run=%d fixture=%s size=%dx%d arm=%s mean-ms=%.3f p90-ms=%.3f fps-p90=%.2f decode-encode-mean-ms=%.3f decode-encode-p90-ms=%.3f decode-encode-fps-p90=%.2f allocs=%.0f B/op=%d payload-B=%d colours=%d selection-ms=%.3f mapping-ms=%.3f psnr=%.6f ssim=%.9f de-mean=%.6f de-p95=%.6f sha256=%x",
						run, fixture.name, size.w, size.h, arm.name,
						m.meanMS, m.p90MS, 1000/m.p90MS,
						m.decodeMS, m.decodeP90MS, 1000/m.decodeP90MS,
						m.allocs, m.bytesAlloc, len(m.payload), colours, m.selectMS, m.mapMS,
						metrics.PSNR(src, decoded), metrics.SSIM(src, decoded), de.Mean, de.P95,
						sha256.Sum256(m.payload))
				})
			}
		}
	}
	for _, arm := range paletteBenchArms {
		agg := aggregates[arm.name]
		if agg == nil {
			continue
		}
		count := uint64(agg.n) //nolint:gosec // The matrix has ten cells.
		t.Logf("SUMMARY run=%d arm=%s cells=%d mean-ms=%.3f mean-cell-p90-ms=%.3f fps-from-cell-p90=%.2f decode-encode-mean-ms=%.3f decode-encode-cell-p90-ms=%.3f decode-encode-fps=%.2f allocs=%.0f B/op=%d mean-payload-B=%d selection-ms=%.3f mapping-ms=%.3f",
			run, arm.name, agg.n, agg.meanMS/float64(agg.n), agg.p90MS/float64(agg.n), 1000/(agg.p90MS/float64(agg.n)),
			agg.decodeMS/float64(agg.n), agg.decodeP90MS/float64(agg.n), 1000/(agg.decodeP90MS/float64(agg.n)),
			agg.allocs/float64(agg.n), agg.bytesAlloc/count, agg.payloadBytes/agg.n,
			agg.selectionMS/float64(agg.n), agg.mappingMS/float64(agg.n))
	}
}

func (a *paletteBenchAggregate) add(m paletteBenchMeasurement) {
	a.n++
	a.meanMS += m.meanMS
	a.p90MS += m.p90MS
	a.decodeMS += m.decodeMS
	a.decodeP90MS += m.decodeP90MS
	a.allocs += m.allocs
	a.bytesAlloc += m.bytesAlloc
	a.payloadBytes += len(m.payload)
	a.selectionMS += m.selectMS
	a.mappingMS += m.mapMS
}

func newPaletteBenchEncoder(arm paletteBenchArm) *paletteBenchEncoder {
	e := &paletteBenchEncoder{arm: arm}
	if arm.kind == "upstream" {
		e.upstream = gosixel.NewEncoder(&e.out)
		e.upstream.Colors = 256
	} else {
		e.nyala = newOpaqueEncoder(&e.out)
	}
	return e
}

func (e *paletteBenchEncoder) encode(src *image.RGBA) ([]byte, time.Duration, time.Duration, error) {
	e.out.Reset()
	if e.upstream != nil {
		err := e.upstream.Encode(src)
		return append([]byte(nil), e.out.Bytes()...), 0, 0, err
	}
	if e.arm.kind == "current" {
		err := e.nyala.encode(src)
		return append([]byte(nil), e.out.Bytes()...), 0, 0, err
	}

	start := time.Now()
	var palette color.Palette
	paletted, exact := palettedFromRGBA(src, paletteColours)
	if exact {
		selection := time.Since(start)
		err := e.nyala.encode(paletted)
		return append([]byte(nil), e.out.Bytes()...), selection, 0, err
	}
	switch e.arm.kind {
	case "serial":
		palette = samplePalette(src, paletteColours)
	case "64k":
		palette = samplePaletteBudget(src, 1<<16)
	case "goquant":
		palette = goquant.MedianCutQuantizer{}.Quantize(make(color.Palette, 0, paletteColours), src)
	case "octree":
		paletted = octreequant.Paletted(src, paletteColours)
	default:
		return nil, 0, 0, fmt.Errorf("unknown palette arm %q", e.arm.kind)
	}
	selection := time.Since(start)
	start = time.Now()
	if palette != nil {
		paletted.Palette = palette
		lut := newPaletteLUT(palette)
		if e.arm.kind == "serial" {
			lut = serialPaletteLUT(palette)
		}
		mapPaletted(paletted, src, lut)
	}
	mapping := time.Since(start)
	err := e.nyala.encode(paletted)
	return append([]byte(nil), e.out.Bytes()...), selection, mapping, err
}

func measurePaletteArm(t *testing.T, enc *paletteBenchEncoder, src *image.RGBA, jpegData []byte) paletteBenchMeasurement {
	t.Helper()
	for range paletteBenchWarmups {
		if _, _, _, err := enc.encode(src); err != nil {
			t.Fatal(err)
		}
	}
	times := make([]time.Duration, paletteBenchSamples)
	decodeTimes := make([]time.Duration, paletteBenchSamples)
	var payload []byte
	var selectTotal, mapTotal time.Duration
	for i := range paletteBenchSamples {
		start := time.Now()
		var selection, mapping time.Duration
		var err error
		payload, selection, mapping, err = enc.encode(src)
		times[i] = time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		selectTotal += selection
		mapTotal += mapping

		start = time.Now()
		img, err := jpegn.Decode(bytes.NewReader(jpegData), &jpegn.Options{ToRGBA: true})
		if err != nil {
			t.Fatal(err)
		}
		rgba, ok := img.(*image.RGBA)
		if !ok {
			t.Fatalf("jpegn decoded %T, want *image.RGBA", img)
		}
		if _, _, _, err := enc.encode(rgba); err != nil {
			t.Fatal(err)
		}
		decodeTimes[i] = time.Since(start)
	}

	var allocErr error
	allocs := testing.AllocsPerRun(3, func() {
		if allocErr != nil {
			return
		}
		_, _, _, allocErr = enc.encode(src)
	})
	if allocErr != nil {
		t.Fatal(allocErr)
	}
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	before := mem.TotalAlloc
	for range 3 {
		if _, _, _, err := enc.encode(src); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&mem)
	mean, p90 := paletteDurationStats(times)
	decodeMean, decodeP90 := paletteDurationStats(decodeTimes)
	return paletteBenchMeasurement{
		meanMS:      durationMS(mean),
		p90MS:       durationMS(p90),
		decodeMS:    durationMS(decodeMean),
		decodeP90MS: durationMS(decodeP90),
		allocs:      allocs,
		bytesAlloc:  (mem.TotalAlloc - before) / 3,
		payload:     payload,
		selectMS:    durationMS(selectTotal / paletteBenchSamples),
		mapMS:       durationMS(mapTotal / paletteBenchSamples),
	}
}

func loadPaletteBenchFrame(t testing.TB, path string, size paletteBenchSize) *image.RGBA {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	scaledH := src.Bounds().Dy() * size.w / src.Bounds().Dx()
	scaled := image.NewRGBA(image.Rect(0, 0, size.w, scaledH))
	draw.Draw(scaled, scaled.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, src.Bounds(), draw.Over, nil)
	dst := image.NewRGBA(image.Rect(0, 0, size.w, size.h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), scaled, image.Point{}, draw.Src)
	assertOpaque(t, dst)
	return dst
}

func encodePaletteBenchJPEG(t *testing.T, src *image.RGBA) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func samplePaletteBudget(src *image.RGBA, budget int) color.Palette {
	bounds := src.Bounds()
	quantSrc := image.Image(src)
	if bounds.Dx()*bounds.Dy() > budget {
		step := 2
		for (bounds.Dx()/step)*(bounds.Dy()/step) > budget {
			step++
		}
		w, h := bounds.Dx()/step, bounds.Dy()/step
		sample := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				sample.SetRGBA(x, y, src.RGBAAt(bounds.Min.X+x*step, bounds.Min.Y+y*step))
			}
		}
		quantSrc = sample
	}
	return median.Quantizer(0).Quantize(make(color.Palette, 0, paletteColours), quantSrc)
}

func serialPaletteLUT(palette color.Palette) []uint8 {
	var pr, pg, pb [256]int32
	for i, colour := range palette {
		r, g, b, _ := colour.RGBA()
		pr[i], pg[i], pb[i] = int32(r>>8), int32(g>>8), int32(b>>8)
	}
	lut := make([]uint8, 1<<15)
	for i := range lut {
		r := int32(i>>10&31<<3 | 4) //nolint:gosec // A 15-bit LUT index bounds this to 252.
		g := int32(i>>5&31<<3 | 4)  //nolint:gosec // A 15-bit LUT index bounds this to 252.
		b := int32(i&31<<3 | 4)     //nolint:gosec // A 15-bit LUT index bounds this to 252.
		best, distance := 0, int32(math.MaxInt32)
		for j := range palette {
			dr, dg, db := r-pr[j], g-pg[j], b-pb[j]
			if d := dr*dr + dg*dg + db*db; d < distance {
				best, distance = j, d
			}
		}
		lut[i] = uint8(best)
	}
	return lut
}

func validatePalettePayload(t *testing.T, payload []byte, bounds image.Rectangle) *image.RGBA {
	t.Helper()
	var mattnImage image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(payload)).Decode(&mattnImage); err != nil {
		t.Fatalf("go-sixel decode: %v", err)
	}
	mattn := paletteImageRGBA(mattnImage)
	if mattn.Bounds() != bounds {
		t.Fatalf("decoded bounds = %v, want %v", mattn.Bounds(), bounds)
	}
	assertOpaque(t, mattn)
	return mattn
}

func paletteImageRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func countPaletteRegisters(payload []byte) int {
	body := payload
	count := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '#' && i+1 < len(body) {
			for i+1 < len(body) && body[i+1] >= '0' && body[i+1] <= '9' {
				i++
			}
			if i+1 < len(body) && body[i+1] == ';' {
				count++
			}
		}
	}
	return count
}

func paletteDurationStats(values []time.Duration) (time.Duration, time.Duration) {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return total / time.Duration(len(values)), ordered[int(math.Ceil(0.9*float64(len(ordered))))-1]
}

func durationMS(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }

func BenchmarkBandEmission(b *testing.B) {
	fixtures := []struct {
		name             string
		width, height    int
		colours, pattern int
	}{
		{name: "photo-1920x1080", width: 1920, height: 1080, colours: 256, pattern: 17},
		{name: "flat-1280x720", width: 1280, height: 720, colours: 4, pattern: 97},
		{name: "web-1280x720", width: 1280, height: 720, colours: 16, pattern: 43},
	}
	for _, fixture := range fixtures {
		img := bandBenchmarkImage(fixture.width, fixture.height, fixture.colours, fixture.pattern)
		var pngData bytes.Buffer
		if err := png.Encode(&pngData, img); err != nil {
			b.Fatal(err)
		}
		for _, arm := range []struct {
			name     string
			parallel bool
		}{{name: "serial"}, {name: "candidate", parallel: true}} {
			b.Run(fixture.name+"/"+arm.name+"/emission", func(b *testing.B) {
				enc := newOpaqueEncoder(io.Discard)
				var payload []byte
				for range paletteBenchWarmups {
					payload = benchmarkBandBody(enc, img, arm.parallel)
				}
				b.ReportAllocs()
				b.ResetTimer()
				runBandBenchmark(b, func() { _ = benchmarkBandBody(enc, img, arm.parallel) })
				b.ReportMetric(float64(len(payload)), "payload-B")
			})
			b.Run(fixture.name+"/"+arm.name+"/complete", func(b *testing.B) {
				enc := newOpaqueEncoder(io.Discard)
				var payload []byte
				for range paletteBenchWarmups {
					payload = benchmarkPalettedEncode(enc, img, arm.parallel)
				}
				b.ReportAllocs()
				b.ResetTimer()
				runBandBenchmark(b, func() { _ = benchmarkPalettedEncode(enc, img, arm.parallel) })
				b.ReportMetric(float64(len(payload)), "payload-B")
			})
			b.Run(fixture.name+"/"+arm.name+"/png-decode-encode", func(b *testing.B) {
				enc := newOpaqueEncoder(io.Discard)
				var payload []byte
				for range paletteBenchWarmups {
					decoded, err := png.Decode(bytes.NewReader(pngData.Bytes()))
					if err != nil {
						b.Fatal(err)
					}
					payload = benchmarkPalettedEncode(enc, decoded.(*image.Paletted), arm.parallel)
				}
				b.ReportAllocs()
				b.ResetTimer()
				runBandBenchmark(b, func() {
					decoded, err := png.Decode(bytes.NewReader(pngData.Bytes()))
					if err != nil {
						b.Fatal(err)
					}
					_ = benchmarkPalettedEncode(enc, decoded.(*image.Paletted), arm.parallel)
				})
				b.ReportMetric(float64(len(payload)), "payload-B")
			})
		}
	}
}

func runBandBenchmark(b *testing.B, fn func()) {
	times := make([]time.Duration, b.N)
	for i := range b.N {
		start := time.Now()
		fn()
		times[i] = time.Since(start)
	}
	b.StopTimer()
	_, p90 := paletteDurationStats(times)
	b.ReportMetric(float64(p90), "p90-ns")
}

func benchmarkBandBody(enc *opaqueEncoder, img *image.Paletted, parallel bool) []byte {
	out := enc.outScratch[:0]
	if parallel {
		out = enc.appendBands(out, img)
	} else {
		enc.growBandScratch(1)
		out = appendBandRange(out, img, &enc.band[0], 0, (img.Bounds().Dy()+5)/6)
	}
	enc.outScratch = out[:0]
	return out
}

func benchmarkPalettedEncode(enc *opaqueEncoder, img *image.Paletted, parallel bool) []byte {
	bounds := img.Bounds()
	out := enc.outScratch[:0]
	out = append(out, "\033P0;0;8q\"1;1;"...)
	out = fmt.Appendf(out, "%d;%d", bounds.Dx(), bounds.Dy())
	for n, colour := range img.Palette {
		r, g, blue, _ := colour.RGBA()
		out = appendColourRegister(out, n, r*100/0xffff, g*100/0xffff, blue*100/0xffff)
	}
	if parallel {
		out = enc.appendBands(out, img)
	} else {
		enc.growBandScratch(1)
		out = appendBandRange(out, img, &enc.band[0], 0, (bounds.Dy()+5)/6)
	}
	out = append(out, 0x1b, 0x5c)
	enc.outScratch = out[:0]
	return out
}

func bandBenchmarkImage(width, height, colours, pattern int) *image.Paletted {
	palette := make(color.Palette, colours)
	for i := range palette {
		palette[i] = color.RGBA{R: uint8(i), G: uint8(i * 37), B: uint8(i * 73), A: 0xff} //nolint:gosec // Overflow creates the fixture palette.
	}
	img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for y := range height {
		offset := img.PixOffset(0, y)
		for x := range width {
			img.Pix[offset+x] = uint8((x*pattern + y*31 + x*y) % colours) //nolint:gosec // Modulo bounds the palette index.
		}
	}
	return img
}

const (
	// shimmerFrames is the length of the synthetic motion sequence.
	shimmerFrames = 20
	// shimmerShiftPX is the horizontal translation of the moving region per frame.
	shimmerShiftPX = 3
	// shimmerWidth and shimmerHeight give a realistic viewport size.
	shimmerWidth  = 1280
	shimmerHeight = 720
	// shimmerStaticRows is the static page-chrome share, ~60% of the frame.
	shimmerStaticRows = shimmerHeight * 6 / 10
)

// TestPaletteShimmerBenchmark measures palette shimmer objectively: it encodes
// a deterministic motion sequence through one production Renderer session,
// decodes every payload, and reports the frame-to-frame CIEDE2000 of pixels
// that are identical in the source pair, plus palette-register churn. Under
// per-frame re-quantisation the unchanged-pixel dE is non-zero; under palette
// reuse it must be exactly zero between re-quantisations. It never changes the
// production palette policy.
func TestPaletteShimmerBenchmark(t *testing.T) {
	if os.Getenv(paletteBenchEnv) != "1" {
		t.Skip("set " + paletteBenchEnv + "=1")
	}
	frames := shimmerMotionSequence(t)
	renderer := &Renderer{}
	payloads := make([][]byte, len(frames))
	decoded := make([]*image.RGBA, len(frames))
	var out bytes.Buffer
	for i, frame := range frames {
		out.Reset()
		if err := renderer.Render(render.Frame{Img: frame}, &out); err != nil {
			t.Fatalf("frame %d render: %v", i, err)
		}
		payloads[i] = append([]byte(nil), out.Bytes()...)
		img, err := renderdecode.DecodeSixel(payloads[i])
		if err != nil {
			t.Fatalf("frame %d decode: %v", i, err)
		}
		if img.Bounds() != frame.Bounds() {
			t.Fatalf("frame %d decoded bounds = %v, want %v", i, img.Bounds(), frame.Bounds())
		}
		decoded[i] = img
	}
	pairs := len(frames) - 1
	var sumMean, sumMax, sumRegisters float64
	for i := range pairs {
		unchanged, meanDE, maxDE := shimmerUnchangedDelta(frames[i], frames[i+1], decoded[i], decoded[i+1])
		registers := countChangedRegisters(payloads[i], payloads[i+1])
		total := shimmerWidth * shimmerHeight
		t.Logf("SHIMMER pair=%02d-%02d unchanged-px=%d unchanged-share=%.3f unchanged-dE-mean=%.6f unchanged-dE-max=%.6f registers-changed=%d",
			i, i+1, unchanged, float64(unchanged)/float64(total), meanDE, maxDE, registers)
		sumMean += meanDE
		sumMax += maxDE
		sumRegisters += float64(registers)
	}
	t.Logf("SHIMMER SUMMARY frames=%d pairs=%d unchanged-dE-mean=%.6f unchanged-dE-max-mean=%.6f registers-changed-mean=%.1f",
		len(frames), pairs, sumMean/float64(pairs), sumMax/float64(pairs), sumRegisters/float64(pairs))
}

// shimmerMotionSequence builds the deterministic motion fixture: a static
// page-chrome region (solid blocks, text-like pattern, gradients) over the top
// ~60% of each frame, and a photo-like region translating shimmerShiftPX
// pixels per frame across the bottom ~40%. Every source pixel in the static
// region is byte-identical across the sequence.
func shimmerMotionSequence(t *testing.T) []*image.RGBA {
	t.Helper()
	chrome := shimmerStaticChrome()
	movingRect := image.Rect(0, shimmerStaticRows, shimmerWidth, shimmerHeight)
	photoWidth := shimmerWidth + shimmerShiftPX*(shimmerFrames-1)
	photo := shimmerPhotoField(photoWidth, movingRect.Dy())
	frames := make([]*image.RGBA, shimmerFrames)
	for i := range frames {
		frame := image.NewRGBA(image.Rect(0, 0, shimmerWidth, shimmerHeight))
		draw.Draw(frame, chrome.Bounds(), chrome, image.Point{}, draw.Src)
		draw.Draw(frame, movingRect, photo, image.Pt(i*shimmerShiftPX, 0), draw.Src)
		assertOpaque(t, frame)
		frames[i] = frame
	}
	return frames
}

// shimmerPhotoField synthesises the photo-like moving patch: a smooth dense
// colour field from fixed-wavelength sinusoids. Neighbouring pixels differ, so
// every translation changes the whole patch and the sampled colour set; the
// wavelengths share no small common period, so the sequence never repeats
// under the fixed-phase sample stride.
func shimmerPhotoField(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			fx, fy := float64(x), float64(y)
			r := 128 + 55*math.Sin(fx/17.3)*math.Cos(fy/23.7) + 40*math.Sin((fx+fy)/41.1)
			g := 128 + 60*math.Sin(fx/29.5+1.7)*math.Cos(fy/13.1) + 30*math.Cos((fx-fy)/37.3)
			b := 128 + 50*math.Cos(fx/11.9)*math.Sin(fy/31.7+0.6) + 45*math.Sin((fx+2*fy)/53.9)
			img.SetRGBA(x, y, color.RGBA{R: shimmerChannel(r), G: shimmerChannel(g), B: shimmerChannel(b), A: 0xff})
		}
	}
	return img
}

// shimmerChannel rounds a sinusoid sum to an 8-bit channel value. The field
// amplitudes keep values inside 0..255; the clamp guards the cast.
func shimmerChannel(v float64) uint8 {
	return uint8(math.Round(math.Min(255, math.Max(0, v)))) //nolint:gosec // Clamped to 0..255 above.
}

// shimmerStaticChrome renders the page-chrome band reused verbatim by every
// frame: a solid header, a horizontal gradient strip, a text-like dash
// pattern on white, and a grid of solid colour blocks.
func shimmerStaticChrome() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, shimmerWidth, shimmerStaticRows))
	palette := mappingTestPalette()[:16]
	for y := range shimmerStaticRows {
		for x := range shimmerWidth {
			var c color.RGBA
			switch {
			case y < 48:
				c = color.RGBA{R: 0x1f, G: 0x2a, B: 0x44, A: 0xff}
			case y < 96:
				level := uint8(x * 255 / (shimmerWidth - 1)) //nolint:gosec // The ramp is bounded to 0..255.
				c = color.RGBA{R: level, G: level / 2, B: 255 - level, A: 0xff}
			case y < 288:
				c = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
				if y%12 < 7 && (x*7+y*3)%11 < 5 && x%97 > 8 {
					c = color.RGBA{R: 0x22, G: 0x22, B: 0x26, A: 0xff}
				}
			default:
				base := color.RGBAModel.Convert(palette[(x/80+(y-288)/36)%len(palette)]).(color.RGBA)
				c = color.RGBA{R: base.R, G: base.G, B: base.B, A: 0xff}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// shimmerUnchangedDelta returns the count, mean CIEDE2000, and max CIEDE2000
// of the decoded pixel pairs whose source pixels are byte-identical between
// the two frames. This is the shimmer number: identical input, so any decoded
// difference comes from the palette changing under it.
func shimmerUnchangedDelta(srcA, srcB, decA, decB *image.RGBA) (int, float64, float64) {
	var unchanged int
	var sum, maxDE float64
	for y := range shimmerHeight {
		for x := range shimmerWidth {
			pa, pb := srcA.RGBAAt(x, y), srcB.RGBAAt(x, y)
			if pa != pb {
				continue
			}
			unchanged++
			da, db := decA.RGBAAt(x, y), decB.RGBAAt(x, y)
			if da == db {
				continue
			}
			de := metrics.DeltaE2000(da.R, da.G, da.B, db.R, db.G, db.B)
			sum += de
			if de > maxDE {
				maxDE = de
			}
		}
	}
	if unchanged == 0 {
		return 0, 0, 0
	}
	return unchanged, sum / float64(unchanged), maxDE
}

// shimmerRegisters holds one payload's parsed colour-register definitions:
// #n;2;r;g;b with RGB percentages in 0..100.
type shimmerRegisters struct {
	set    [256]bool
	colour [256][3]int
}

// parsePaletteDefinitions extracts every #n;2;r;g;b register definition from a
// raw Sixel payload, distinguishing definitions from bare #n colour selects by
// the ';' after the register number, as countPaletteRegisters does.
func parsePaletteDefinitions(payload []byte) shimmerRegisters {
	var regs shimmerRegisters
	i := 0
	next := func() (int, bool) {
		if i >= len(payload) || payload[i] < '0' || payload[i] > '9' {
			return 0, false
		}
		value := 0
		for i < len(payload) && payload[i] >= '0' && payload[i] <= '9' {
			value = value*10 + int(payload[i]-'0')
			i++
		}
		return value, true
	}
	expect := func(b byte) bool {
		if i < len(payload) && payload[i] == b {
			i++
			return true
		}
		return false
	}
	for i < len(payload) {
		if payload[i] != '#' {
			i++
			continue
		}
		i++
		n, ok := next()
		if !ok || !expect(';') || !expect('2') || !expect(';') {
			continue
		}
		r, ok := next()
		if !ok || !expect(';') {
			continue
		}
		g, ok := next()
		if !ok || !expect(';') {
			continue
		}
		b, ok := next()
		if !ok || n > 255 {
			continue
		}
		regs.set[n] = true
		regs.colour[n] = [3]int{r, g, b}
	}
	return regs
}

// countChangedRegisters returns how many palette registers differ between two
// payloads: defined in only one of them, or defined in both with different
// colours.
func countChangedRegisters(a, b []byte) int {
	ra, rb := parsePaletteDefinitions(a), parsePaletteDefinitions(b)
	changed := 0
	for n := range ra.set {
		if ra.set[n] != rb.set[n] || (ra.set[n] && ra.colour[n] != rb.colour[n]) {
			changed++
		}
	}
	return changed
}

func envInt(name string) int {
	var value int
	_, _ = fmt.Sscanf(os.Getenv(name), "%d", &value)
	return value
}

func rotateArms(values []paletteBenchArm, run int) []paletteBenchArm {
	out := append([]paletteBenchArm(nil), values...)
	shift := run % len(out)
	out = append(out[shift:], out[:shift]...)
	if run/len(out)%2 == 1 {
		sort.SliceStable(out, func(i, j int) bool { return i > j })
	}
	return out
}

func rotateFixtures(values []paletteBenchFixture, run int) []paletteBenchFixture {
	out := append([]paletteBenchFixture(nil), values...)
	shift := run % len(out)
	return append(out[shift:], out[:shift]...)
}
