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
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"testing"
	"time"

	xsixel "github.com/charmbracelet/x/ansi/sixel"
	"github.com/delthas/octreequant"
	goquant "github.com/ericpauley/go-quantize/quantize"
	"github.com/gen2brain/jpegn"
	gosixel "github.com/mattn/go-sixel"
	"github.com/soniakeys/quant/median"
	xdraw "golang.org/x/image/draw"

	"github.com/wimpysworld/nyala/internal/metrics"
)

const (
	paletteBenchEnv     = "NYALA_PALETTE_BENCH"
	paletteBenchRunEnv  = "NYALA_PALETTE_BENCH_RUN"
	paletteBenchSamples = 3
	paletteBenchWarmups = 4
	paletteColours      = 255
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
	mapMS       float64
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
	paletted := palettedFromRGBA(src, paletteColours)
	if paletted != nil {
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
	if paletted == nil {
		paletted = image.NewPaletted(src.Bounds(), palette)
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

func loadPaletteBenchFrame(t *testing.T, path string, size paletteBenchSize) *image.RGBA {
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
	body := sixelBody(t, payload)
	charmImage, err := (&xsixel.Decoder{}).Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Charm decode: %v", err)
	}
	mattn := paletteImageRGBA(mattnImage)
	charm := paletteImageRGBA(charmImage)
	if mattn.Bounds() != bounds || charm.Bounds() != bounds {
		t.Fatalf("decoded bounds mattn=%v charm=%v want=%v", mattn.Bounds(), charm.Bounds(), bounds)
	}
	assertOpaque(t, mattn)
	return mattn
}

func sixelBody(t *testing.T, payload []byte) []byte {
	t.Helper()
	q := bytes.IndexByte(payload, 'q')
	if q < 2 || !bytes.HasPrefix(payload, []byte("\x1bP")) || !bytes.HasSuffix(payload, []byte("\x1b\\")) {
		t.Fatal("invalid Sixel DCS framing")
	}
	return payload[q+1 : len(payload)-2]
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
