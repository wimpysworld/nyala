package block_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/harness"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

const (
	colour256BenchEnv    = "NYALA_COLOUR256_BENCH"
	colour256BenchRunEnv = "NYALA_COLOUR256_BENCH_RUN"
	colour256Samples     = 21
	colour256Warmups     = 4
)

type colour256Mapper struct {
	name string
	mapc func(color.RGBA) uint8
}

type colour256Size struct {
	cols int
	rows int
}

type colour256Aggregate struct {
	n             int
	mapperSamples []time.Duration
	pathSamples   []time.Duration
	allocs        float64
	payload       int
	psnrMean      float64
	psnrWorst     float64
	ssimMean      float64
	ssimWorst     float64
	deMean        float64
	deP95         float64
	worstDEMean   float64
	worstDEP95    float64
	lowContrast   int
}

var colour256Sizes = []colour256Size{{cols: 120, rows: 40}, {cols: 240, rows: 86}}

var colour256MapperSink uint8

// TestColour256Benchmark is an opt-in internal comparison of the current,
// nearest-two, and perceptual-LUT xterm-256 conversion policies.
func TestColour256Benchmark(t *testing.T) {
	if os.Getenv(colour256BenchEnv) != "1" {
		t.Skip("set " + colour256BenchEnv + "=1")
	}
	oldProcs := runtime.GOMAXPROCS(4)
	oldGC := debug.SetGCPercent(100)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(oldProcs)
		debug.SetGCPercent(oldGC)
	})
	run := colour256EnvInt(colour256BenchRunEnv)
	mappers := []colour256Mapper{
		{name: "current", mapc: currentXTerm256},
		{name: "nearest-two", mapc: nearestTwoXTerm256},
		{name: "perceptual-lut", mapc: block.XTerm256},
	}
	mappers = rotateColour256Mappers(mappers, run)
	aggregates := make(map[string]*colour256Aggregate, len(mappers))
	for _, mapper := range mappers {
		aggregates[mapper.name] = &colour256Aggregate{psnrWorst: math.Inf(1), ssimWorst: math.Inf(1)}
	}
	rasteriser, err := cellraster.NewRasteriser()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range rotateColour256Fixtures(harness.OverlayFixtures, run) {
		src := loadColour256Fixture(t, fixture.PNG)
		runs := loadColour256Runs(t, fixture.Runs)
		for _, size := range colour256Sizes {
			name := fmt.Sprintf("%s/%dx%d", fixture.Page, size.cols, size.rows)
			t.Run(name, func(t *testing.T) {
				pxW, pxH := render.BlockCaptureSize(render.BlockQuadrant, size.cols, size.rows)
				frame := render.Frame{
					Img:  metrics.Downscale(src, pxW, pxH),
					Text: scaleColour256Runs(runs, size),
				}
				renderer := block.Renderer{Cols: size.cols, Rows: size.rows, Scheme: block.SchemeQuadrant}
				base := renderer.Compose(frame)
				reference := rasteriser.Rasterise(base)
				for _, mapper := range mappers {
					mapperSamples := measureMapper(base, mapper.mapc)
					pathSamples, allocs, payload := measureColour256Path(t, renderer, frame, mapper.mapc)
					mapperMean, mapperP90 := durationStats(mapperSamples)
					pathMean, pathP90 := durationStats(pathSamples)
					mapped := mapColour256Grid(base, mapper.mapc)
					actual := rasteriser.Rasterise(mapped)
					de := metrics.CIEDE2000(reference, actual)
					psnr, ssim := metrics.PSNR(reference, actual), metrics.SSIM(reference, actual)
					lowContrast := lowContrastTextCells(mapped)
					aggregates[mapper.name].add(mapperSamples, pathSamples, allocs, payload, psnr, ssim, de, lowContrast)
					t.Logf("COLOUR256 run=%d arm=%s mapper-mean-us=%.3f mapper-p90-us=%.3f path-mean-ms=%.3f path-p90-ms=%.3f fps-p90=%.2f allocs=%.0f payload-B=%d psnr=%.6f ssim=%.9f de-mean=%.6f de-p95=%.6f low-contrast=%d",
						run, mapper.name, durationUS(mapperMean), durationUS(mapperP90), durationMS(pathMean), durationMS(pathP90),
						1000/durationMS(pathP90), allocs, payload, psnr, ssim, de.Mean, de.P95, lowContrast)
				}
			})
		}
	}
	for _, mapper := range mappers {
		agg := aggregates[mapper.name]
		mapperMean, mapperP90 := durationStats(agg.mapperSamples)
		pathMean, pathP90 := durationStats(agg.pathSamples)
		t.Logf("COLOUR256-SUMMARY run=%d arm=%s cells=%d samples=%d mapper-mean-us=%.3f mapper-p90-us=%.3f path-mean-ms=%.3f path-p90-ms=%.3f fps-p90=%.2f allocs=%.1f payload-mean-B=%d psnr-mean=%.6f psnr-worst=%.6f ssim-mean=%.9f ssim-worst=%.9f de-mean=%.6f de-cell-p95=%.6f de-worst-mean=%.6f de-worst-p95=%.6f low-contrast-total=%d",
			run, mapper.name, agg.n, len(agg.pathSamples), durationUS(mapperMean), durationUS(mapperP90),
			durationMS(pathMean), durationMS(pathP90), 1000/durationMS(pathP90),
			agg.allocs/float64(agg.n), agg.payload/agg.n,
			agg.psnrMean/float64(agg.n), agg.psnrWorst, agg.ssimMean/float64(agg.n), agg.ssimWorst,
			agg.deMean/float64(agg.n), agg.deP95/float64(agg.n), agg.worstDEMean, agg.worstDEP95, agg.lowContrast)
		t.Logf("COLOUR256-RAW run=%d arm=%s mapper-ns=%s path-ns=%s", run, mapper.name,
			formatColour256Durations(agg.mapperSamples), formatColour256Durations(agg.pathSamples))
	}
}

func (a *colour256Aggregate) add(mapperSamples, pathSamples []time.Duration, allocs float64, payload int, psnr, ssim float64, de metrics.CIEDE2000Stats, lowContrast int) {
	a.n++
	a.mapperSamples = append(a.mapperSamples, mapperSamples...)
	a.pathSamples = append(a.pathSamples, pathSamples...)
	a.allocs += allocs
	a.payload += payload
	a.psnrMean += psnr
	a.psnrWorst = min(a.psnrWorst, psnr)
	a.ssimMean += ssim
	a.ssimWorst = min(a.ssimWorst, ssim)
	a.deMean += de.Mean
	a.deP95 += de.P95
	a.worstDEMean = max(a.worstDEMean, de.Mean)
	a.worstDEP95 = max(a.worstDEP95, de.P95)
	a.lowContrast += lowContrast
}

func loadColour256Fixture(t *testing.T, name string) image.Image {
	t.Helper()
	f, err := os.Open("../../../testdata/" + name)
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

func loadColour256Runs(t *testing.T, name string) []render.GlyphRun {
	t.Helper()
	runs, err := harness.LoadRunSet("../../../testdata/overlay/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func scaleColour256Runs(runs []render.GlyphRun, size colour256Size) []render.GlyphRun {
	out := make([]render.GlyphRun, len(runs))
	for i, run := range runs {
		out[i] = run
		out[i].Col = run.Col * size.cols / 120
		out[i].Row = run.Row * size.rows / 40
	}
	return out
}

func measureMapper(grid block.Grid, mapper func(color.RGBA) uint8) []time.Duration {
	times := make([]time.Duration, colour256Samples)
	var sink uint8
	for sample := range times {
		start := time.Now()
		for range 64 {
			for i := range grid.Cells {
				sink ^= mapper(grid.Cells[i].FG)
				sink ^= mapper(grid.Cells[i].BG)
			}
		}
		times[sample] = time.Since(start) / 64
	}
	colour256MapperSink = sink
	return times
}

func measureColour256Path(t *testing.T, renderer block.Renderer, frame render.Frame, mapper func(color.RGBA) uint8) ([]time.Duration, float64, int) {
	t.Helper()
	var grid block.Grid
	var diff block.GridDiff
	var out bytes.Buffer
	fg := colour256SGR("\x1b[38;5;", mapper)
	bg := colour256SGR("\x1b[48;5;", mapper)
	run := func() {
		out.Reset()
		diff.Reset()
		renderer.ComposeInto(&grid, frame)
		if err := diff.Emit(&out, grid, fg, bg); err != nil {
			t.Fatal(err)
		}
	}
	for range colour256Warmups {
		run()
	}
	allocs := testing.AllocsPerRun(5, run)
	times := make([]time.Duration, colour256Samples)
	for i := range times {
		start := time.Now()
		run()
		times[i] = time.Since(start)
	}
	return times, allocs, out.Len()
}

func colour256SGR(prefix string, mapper func(color.RGBA) uint8) block.SGRFunc {
	return func(dst []byte, c color.RGBA) []byte {
		dst = append(dst, prefix...)
		dst = strconv.AppendInt(dst, int64(mapper(c)), 10)
		return append(dst, 'm')
	}
}

func mapColour256Grid(src block.Grid, mapper func(color.RGBA) uint8) block.Grid {
	dst := block.Grid{Cols: src.Cols, Rows: src.Rows, Cells: append([]block.Cell(nil), src.Cells...)}
	for i := range dst.Cells {
		dst.Cells[i].FG = xterm256Colour(mapper(dst.Cells[i].FG))
		dst.Cells[i].BG = xterm256Colour(mapper(dst.Cells[i].BG))
	}
	return dst
}

func lowContrastTextCells(grid block.Grid) int {
	var count int
	for _, cell := range grid.Cells {
		if cell.Text == "" || cell.Wide {
			continue
		}
		if metrics.WCAGContrast(cell.FG.R, cell.FG.G, cell.FG.B, cell.BG.R, cell.BG.G, cell.BG.B) < metrics.WCAGThreshold {
			count++
		}
	}
	return count
}

func currentXTerm256(c color.RGBA) uint8 {
	if c.R == c.G && c.G == c.B {
		if c.R < 8 {
			return 16
		}
		if c.R > 248 {
			return 231
		}
		return uint8(232 + min((int(c.R)-8)/10, 23))
	}
	return cubeXTerm256(c)
}

func nearestTwoXTerm256(c color.RGBA) uint8 {
	cube := cubeXTerm256(c)
	grey := nearestGreyXTerm256(c)
	if colourDistance2(c, xterm256Colour(grey)) < colourDistance2(c, xterm256Colour(cube)) {
		return grey
	}
	return cube
}

func cubeXTerm256(c color.RGBA) uint8 {
	return uint8(16 + 36*cubeAxis(c.R) + 6*cubeAxis(c.G) + cubeAxis(c.B)) //nolint:gosec // Cube axes bound the result to 16..231.
}

func cubeAxis(c uint8) int {
	switch {
	case c < 48:
		return 0
	case c < 115:
		return 1
	default:
		return int((c - 35) / 40)
	}
}

func nearestGreyXTerm256(c color.RGBA) uint8 {
	mean := (int(c.R) + int(c.G) + int(c.B)) / 3
	step := (mean - 8 + 5) / 10
	step = max(0, min(step, 23))
	return uint8(232 + step) //nolint:gosec // step is clamped to 0..23.
}

func colourDistance2(a, b color.RGBA) int {
	dr, dg, db := int(a.R)-int(b.R), int(a.G)-int(b.G), int(a.B)-int(b.B)
	return dr*dr + dg*dg + db*db
}

func xterm256Colour(index uint8) color.RGBA {
	if index >= 232 {
		v := uint8(8 + 10*(int(index)-232)) //nolint:gosec // Canonical grey indices bound the result to 8..238.
		return color.RGBA{R: v, G: v, B: v, A: 0xff}
	}
	n := int(index) - 16
	return color.RGBA{
		R: cubeLevel(n / 36),
		G: cubeLevel(n / 6 % 6),
		B: cubeLevel(n % 6),
		A: 0xff,
	}
}

func cubeLevel(axis int) uint8 {
	if axis == 0 {
		return 0
	}
	return uint8(55 + 40*axis) //nolint:gosec // Canonical cube axes are 1..5.
}

func durationStats(values []time.Duration) (time.Duration, time.Duration) {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return total / time.Duration(len(values)), ordered[int(math.Ceil(0.9*float64(len(values))))-1]
}

func formatColour256Durations(values []time.Duration) string {
	var out strings.Builder
	for i, value := range values {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.FormatInt(value.Nanoseconds(), 10))
	}
	return out.String()
}

func colour256EnvInt(name string) int {
	value, _ := strconv.Atoi(os.Getenv(name))
	return value
}

func rotateColour256Mappers(values []colour256Mapper, run int) []colour256Mapper {
	out := append([]colour256Mapper(nil), values...)
	shift := run % len(out)
	out = append(out[shift:], out[:shift]...)
	if run/len(out)%2 == 1 {
		sort.SliceStable(out, func(i, j int) bool { return i > j })
	}
	return out
}

func rotateColour256Fixtures(values []harness.OverlayFixture, run int) []harness.OverlayFixture {
	out := append([]harness.OverlayFixture(nil), values...)
	shift := run % len(out)
	out = append(out[shift:], out[:shift]...)
	if run/len(out)%2 == 1 {
		sort.SliceStable(out, func(i, j int) bool { return i > j })
	}
	return out
}

func durationUS(value time.Duration) float64 { return float64(value) / float64(time.Microsecond) }
func durationMS(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }
