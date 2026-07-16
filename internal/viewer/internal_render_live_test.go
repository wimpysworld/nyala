//go:build live

package viewer

import (
	"fmt"
	"hash/fnv"
	"image"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

const (
	internalRenderSamples = 30
	internalRenderCellW   = 10
	internalRenderCellH   = 20
)

type internalRenderCase struct {
	name          string
	backend       Backend
	engineBackend string
	width         int
	height        int
}

type internalRenderResult struct {
	meanMS, p90MS float64
	meanBytes     float64
	prepMS        float64
	allocs        float64
}

// TestInternalRenderingMetrics measures Nyala's internal production rendering
// paths against production-shaped frames captured from a local page through CDP.
// It is opt-in because it launches six Chromium sessions and encodes large Sixel
// frames. Run with NYALA_INTERNAL_RENDER_METRICS=1 and the live build tag.
func TestInternalRenderingMetrics(t *testing.T) {
	if os.Getenv("NYALA_INTERNAL_RENDER_METRICS") != "1" {
		t.Skip("set NYALA_INTERNAL_RENDER_METRICS=1 to run the internal rendering test")
	}
	fixture, err := filepath.Abs("../../testdata/corpus/layout-13-linuxmatters/index.html")
	if err != nil {
		t.Fatal(err)
	}
	fixtureURL := (&url.URL{Scheme: "file", Path: fixture}).String()
	cases := []internalRenderCase{
		{name: "block/1920x1080", backend: BackendBlock, engineBackend: "block", width: 1920, height: 1080},
		{name: "kitty/1920x1080", backend: BackendKitty, engineBackend: "kitty", width: 1920, height: 1080},
		{name: "sixel/1920x1080", backend: BackendSixel, engineBackend: "sixel", width: 1920, height: 1080},
		{name: "block/2560x1600", backend: BackendBlock, engineBackend: "block", width: 2560, height: 1600},
		{name: "kitty/2560x1600", backend: BackendKitty, engineBackend: "kitty", width: 2560, height: 1600},
		{name: "sixel/2560x1600", backend: BackendSixel, engineBackend: "sixel", width: 2560, height: 1600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames, prepMS := captureInternalRenderFrames(t, fixtureURL, tc)
			result := measureInternalRender(t, tc, frames)
			result.prepMS = prepMS
			t.Logf("backend=%s size=%dx%d grid=%dx%d mean-ms=%.3f p90-ms=%.3f fps-from-p90=%.2f mean-terminal-B=%.0f allocs/op=%.1f prep-ms=%.1f samples=%d go=%s",
				tc.engineBackend, tc.width, tc.height,
				tc.width/internalRenderCellW, tc.height/internalRenderCellH,
				result.meanMS, result.p90MS, 1000/result.p90MS,
				result.meanBytes, result.allocs, result.prepMS, internalRenderSamples, runtime.Version())
		})
	}
}

func captureInternalRenderFrames(t *testing.T, fixtureURL string, tc internalRenderCase) ([2]render.Frame, float64) {
	t.Helper()
	e := engine.New("")
	e.SetCellPx(internalRenderCellW, internalRenderCellH)
	e.SetLayoutWidth(1280)
	e.SetScheme("quadrant")
	e.SetBackend(tc.engineBackend)
	e.Resize(tc.width/internalRenderCellW, tc.height/internalRenderCellH)
	start := time.Now()
	if err := e.Navigate(fixtureURL); err != nil {
		t.Fatalf("navigate benchmark fixture: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	first := waitSettledInternalRenderFrame(t, e.Frames(), tc, 0)
	firstHash := internalFrameHash(first)
	e.SendScroll(tc.height / 2)
	second := waitSettledInternalRenderFrame(t, e.Frames(), tc, firstHash)
	return [2]render.Frame{first, second}, float64(time.Since(start)) / float64(time.Millisecond)
}

func waitSettledInternalRenderFrame(t *testing.T, frames <-chan render.Frame, tc internalRenderCase, rejectHash uint64) render.Frame {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	idle := time.NewTimer(600 * time.Millisecond)
	defer idle.Stop()
	var latest render.Frame
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("engine frame channel closed")
			}
			if !validInternalRenderFrame(frame, tc) || internalFrameHash(frame) == rejectHash {
				continue
			}
			latest = frame
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(600 * time.Millisecond)
		case <-idle.C:
			if latest.HasImage() {
				return latest
			}
			idle.Reset(600 * time.Millisecond)
		case <-deadline.C:
			t.Fatalf("timed out waiting for settled %s frame", tc.engineBackend)
		}
	}
}

func validInternalRenderFrame(frame render.Frame, tc internalRenderCase) bool {
	if !frame.HasImage() {
		return false
	}
	if tc.backend == BackendBlock && len(frame.Text) == 0 {
		return false
	}
	if frame.Img != nil {
		b := frame.Img.Bounds()
		return b.Dx() == tc.width && b.Dy() == tc.height
	}
	return tc.backend == BackendKitty && frame.EncodedPNG != ""
}

func internalFrameHash(frame render.Frame) uint64 {
	h := fnv.New64a()
	if frame.EncodedPNG != "" {
		_, _ = io.WriteString(h, frame.EncodedPNG)
	} else if frame.Img != nil {
		writeImageSamples(h, frame.Img)
	}
	for _, run := range frame.Text {
		_, _ = fmt.Fprintf(h, "%d,%d,%s;", run.Col, run.Row, run.Text)
	}
	return h.Sum64()
}

func writeImageSamples(w io.Writer, img image.Image) {
	b := img.Bounds()
	var rgba [4]byte
	for sy := range 32 {
		y := b.Min.Y + sy*(b.Dy()-1)/31
		for sx := range 32 {
			x := b.Min.X + sx*(b.Dx()-1)/31
			r, g, blue, a := img.At(x, y).RGBA()
			rgba[0], rgba[1], rgba[2], rgba[3] = byte(r>>8), byte(g>>8), byte(blue>>8), byte(a>>8)
			_, _ = w.Write(rgba[:])
		}
	}
}

func measureInternalRender(t *testing.T, tc internalRenderCase, frames [2]render.Frame) internalRenderResult {
	t.Helper()
	state := State{
		Backend: tc.backend,
		Scheme:  block.SchemeQuadrant,
		Colour:  ColourTrue,
		Cols:    tc.width / internalRenderCellW,
		Rows:    tc.height / internalRenderCellH,
	}
	if tc.backend == BackendBlock {
		state.SetDiff(&block.GridDiff{})
	}
	for i := range 4 {
		if err := state.RenderStreamFrame(frames[i%2], io.Discard); err != nil {
			t.Fatalf("warm renderer: %v", err)
		}
	}
	runtime.GC()
	batch := internalRenderBatch(tc.backend)
	durations := make([]float64, internalRenderSamples)
	bytesPerFrame := make([]int64, internalRenderSamples)
	for i := range internalRenderSamples {
		var output internalCountingWriter
		start := time.Now()
		for j := range batch {
			if err := state.RenderStreamFrame(frames[(i*batch+j)%2], &output); err != nil {
				t.Fatalf("render sample %d: %v", i, err)
			}
		}
		durations[i] = float64(time.Since(start)) / float64(time.Millisecond) / float64(batch)
		bytesPerFrame[i] = output.n / int64(batch)
	}
	ordered := append([]float64(nil), durations...)
	sort.Float64s(ordered)
	var durationTotal float64
	var byteTotal int64
	for i := range durations {
		durationTotal += durations[i]
		byteTotal += bytesPerFrame[i]
	}
	p90Index := int(math.Ceil(0.9*float64(len(ordered)))) - 1
	nextFrame := 0
	var allocErr error
	allocs := testing.AllocsPerRun(5, func() {
		if allocErr != nil {
			return
		}
		allocErr = state.RenderStreamFrame(frames[nextFrame%2], io.Discard)
		nextFrame++
	})
	if allocErr != nil {
		t.Fatalf("measure allocations: %v", allocErr)
	}
	return internalRenderResult{
		meanMS:    durationTotal / float64(len(durations)),
		p90MS:     ordered[p90Index],
		meanBytes: float64(byteTotal) / float64(len(bytesPerFrame)),
		allocs:    allocs,
	}
}

func internalRenderBatch(backend Backend) int {
	switch backend {
	case BackendKitty:
		return 1000
	case BackendBlock:
		return 10
	default:
		return 1
	}
}

type internalCountingWriter struct{ n int64 }

func (w *internalCountingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
