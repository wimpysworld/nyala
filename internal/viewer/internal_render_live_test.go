//go:build live

package viewer

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
	"image/png"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

const (
	internalRenderSamples     = 30
	internalRenderCellW       = 10
	internalRenderCellH       = 20
	internalRenderScrollStyle = `<style>body { min-height: 150vh; }</style>`
)

type internalRenderCase struct {
	name          string
	backend       Backend
	engineBackend Backend
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
// It is opt-in because it launches nine Chromium sessions and encodes large Sixel
// frames. Run with NYALA_INTERNAL_RENDER_METRICS=1 and the live build tag.
func TestInternalRenderingMetrics(t *testing.T) {
	if os.Getenv("NYALA_INTERNAL_RENDER_METRICS") != "1" {
		t.Skip("set NYALA_INTERNAL_RENDER_METRICS=1 to run the internal rendering test")
	}
	fixture, err := filepath.Abs("../../testdata/corpus/layout-13-linuxmatters/index.html")
	if err != nil {
		t.Fatal(err)
	}
	cases := []internalRenderCase{
		{name: "block/1920x1080", backend: BackendBlock, engineBackend: BackendBlock, width: 1920, height: 1080},
		{name: "kitty/1920x1080", backend: BackendKitty, engineBackend: BackendKitty, width: 1920, height: 1080},
		{name: "sixel/1920x1080", backend: BackendSixel, engineBackend: BackendSixel, width: 1920, height: 1080},
		{name: "block/2560x1600", backend: BackendBlock, engineBackend: BackendBlock, width: 2560, height: 1600},
		{name: "kitty/2560x1600", backend: BackendKitty, engineBackend: BackendKitty, width: 2560, height: 1600},
		{name: "sixel/2560x1600", backend: BackendSixel, engineBackend: BackendSixel, width: 2560, height: 1600},
		{name: "block/2560x2880", backend: BackendBlock, engineBackend: BackendBlock, width: 2560, height: 2880},
		{name: "kitty/2560x2880", backend: BackendKitty, engineBackend: BackendKitty, width: 2560, height: 2880},
		{name: "sixel/2560x2880", backend: BackendSixel, engineBackend: BackendSixel, width: 2560, height: 2880},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames, prepMS := captureInternalRenderFrames(t, fixture, tc)
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

func captureInternalRenderFrames(t *testing.T, fixture string, tc internalRenderCase) ([2]render.Frame, float64) {
	t.Helper()
	fixtureURL := scrollableInternalRenderFixtureURL(t, fixture)
	t.Setenv("NYALA_FPS_TIMING", "1")
	e := engine.New("")
	e.SetDiagnosticFrameMeta(true)
	e.SetCellPx(internalRenderCellW, internalRenderCellH)
	e.SetBackend(tc.engineBackend)
	e.Resize(tc.width/internalRenderCellW, tc.height/internalRenderCellH)
	start := time.Now()
	if err := e.Navigate(fixtureURL); err != nil {
		t.Fatalf("navigate benchmark fixture: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	first := waitSettledInternalRenderFrame(t, e.Frames(), tc, "top", 0, 0, e.IngestSummary)
	firstHash := internalFrameHash(first)
	var firstScrollY float64
	if first.Diagnostics != nil {
		firstScrollY = first.Diagnostics.Meta.ScrollOffsetY
	}
	e.SendScroll(tc.height / 2)
	second := waitSettledInternalRenderFrame(t, e.Frames(), tc, "scrolled", firstHash, firstScrollY, e.IngestSummary)
	return [2]render.Frame{first, second}, float64(time.Since(start)) / float64(time.Millisecond)
}

func scrollableInternalRenderFixtureURL(t *testing.T, fixture string) string {
	t.Helper()
	source, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read benchmark fixture: %v", err)
	}
	closingHead := []byte("</head>")
	if !bytes.Contains(source, closingHead) {
		t.Fatal("benchmark fixture has no closing head tag")
	}
	document := bytes.Replace(source, closingHead, append([]byte(internalRenderScrollStyle), closingHead...), 1)
	path := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("write scrollable benchmark fixture: %v", err)
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func waitSettledInternalRenderFrame(t *testing.T, frames <-chan render.Frame, tc internalRenderCase, phase string, rejectHash uint64, afterScroll float64, ingestSummary func() render.IngestStats) render.Frame {
	t.Helper()
	frame, err := waitInternalRenderFrame(frames, tc, phase, rejectHash, afterScroll, 30*time.Second, 600*time.Millisecond, ingestSummary)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

type internalRenderWaitObservation struct {
	started          time.Time
	lastArrival      time.Time
	total            int
	accepted         int
	noImage          int
	missingBlockText int
	malformedPNG     int
	wrongDimensions  int
	preScrollHash    int
	unchangedScroll  int
	lastShape        string
	lastHash         uint64
	lastGap          time.Duration
	lastDiagnostics  *render.FrameDiagnostics
	acceptedHashes   []uint64
}

func waitInternalRenderFrame(frames <-chan render.Frame, tc internalRenderCase, phase string, rejectHash uint64, afterScroll float64, deadlineDuration, idleDuration time.Duration, ingestSummary func() render.IngestStats) (render.Frame, error) {
	deadline := time.NewTimer(deadlineDuration)
	defer deadline.Stop()
	idle := time.NewTimer(idleDuration)
	defer idle.Stop()
	var latest render.Frame
	observation := internalRenderWaitObservation{started: time.Now()}
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return render.Frame{}, fmt.Errorf("engine frame channel closed; %s", observation.describe(phase, rejectHash, ingestSummary()))
			}
			arrival := time.Now()
			observation.observeArrival(frame, arrival)
			rejection := internalRenderFrameRejection(frame, tc)
			if rejection != "" {
				observation.reject(rejection)
				continue
			}
			hash := observation.lastHash
			if hash == rejectHash {
				observation.preScrollHash++
				continue
			}
			if phase == "scrolled" && !internalRenderScrollAdvanced(frame, afterScroll) {
				observation.unchangedScroll++
				continue
			}
			observation.accepted++
			observation.acceptedHashes = append(observation.acceptedHashes, hash)
			latest = frame
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleDuration)
		case <-idle.C:
			if latest.HasImage() {
				return latest, nil
			}
			idle.Reset(idleDuration)
		case <-deadline.C:
			return render.Frame{}, fmt.Errorf("timed out waiting for settled %s frame; %s", tc.engineBackend, observation.describe(phase, rejectHash, ingestSummary()))
		}
	}
}

func (o *internalRenderWaitObservation) observeArrival(frame render.Frame, arrival time.Time) {
	o.total++
	if !o.lastArrival.IsZero() {
		o.lastGap = arrival.Sub(o.lastArrival)
	}
	o.lastArrival = arrival
	o.lastHash = internalFrameHash(frame)
	o.lastDiagnostics = frame.Diagnostics
	if frame.Img != nil {
		o.lastShape = fmt.Sprintf("bounds=%v", frame.Img.Bounds())
	} else {
		o.lastShape = fmt.Sprintf("png-bytes=%d", len(frame.EncodedPNG))
	}
}

func (o *internalRenderWaitObservation) reject(reason string) {
	switch reason {
	case "no-image":
		o.noImage++
	case "missing-block-text":
		o.missingBlockText++
	case "malformed-png":
		o.malformedPNG++
	case "wrong-dimensions":
		o.wrongDimensions++
	}
}

func (o internalRenderWaitObservation) describe(phase string, rejectHash uint64, ingest render.IngestStats) string {
	meta := "unavailable"
	if o.lastDiagnostics != nil {
		m := o.lastDiagnostics.Meta
		meta = fmt.Sprintf("device=%.0fx%.0f scroll=(%.1f,%.1f)", m.DeviceWidth, m.DeviceHeight, m.ScrollOffsetX, m.ScrollOffsetY)
	}
	return fmt.Sprintf("phase=%s elapsed=%s published=%d accepted=%d rejected(no-image=%d missing-block-text=%d malformed-png=%d wrong-dimensions=%d pre-scroll-hash=%d unchanged-scroll=%d) reject-hash=%x last(%s hash=%x gap=%s %s) accepted-hashes=%x ingest(frames=%d mean-fps=%.2f min-fps=%.2f max-fps=%.2f median-fps=%.2f worst-gap-ms=%.2f decode-mean-ms=%.2f decode-max-ms=%.2f)",
		phase, time.Since(o.started).Round(time.Millisecond), o.total, o.accepted,
		o.noImage, o.missingBlockText, o.malformedPNG, o.wrongDimensions, o.preScrollHash, o.unchangedScroll,
		rejectHash, o.lastShape, o.lastHash, o.lastGap.Round(time.Millisecond), meta, o.acceptedHashes,
		ingest.Frames, ingest.MeanFPS, ingest.MinFPS, ingest.MaxFPS, ingest.MedianFPS, ingest.WorstGapMS, ingest.DecodeMeanMS, ingest.DecodeMaxMS)
}

func internalRenderFrameRejection(frame render.Frame, tc internalRenderCase) string {
	if !frame.HasImage() {
		return "no-image"
	}
	if tc.backend == BackendBlock && len(frame.Text) == 0 {
		return "missing-block-text"
	}
	if frame.Img != nil {
		b := frame.Img.Bounds()
		if b.Dx() != tc.width || b.Dy() != tc.height {
			return "wrong-dimensions"
		}
		return ""
	}
	if tc.backend != BackendKitty {
		return "no-image"
	}
	cfg, err := png.DecodeConfig(base64.NewDecoder(base64.StdEncoding, bytes.NewBufferString(frame.EncodedPNG)))
	if err != nil {
		return "malformed-png"
	}
	if cfg.Width != tc.width || cfg.Height != tc.height {
		return "wrong-dimensions"
	}
	return ""
}

func validInternalRenderFrame(frame render.Frame, tc internalRenderCase) bool {
	return internalRenderFrameRejection(frame, tc) == ""
}

func internalRenderScrollAdvanced(frame render.Frame, after float64) bool {
	return frame.Diagnostics != nil && frame.Diagnostics.Meta.ScrollOffsetY > after
}

func TestInternalRenderScrollAdvanced(t *testing.T) {
	unscrolled := render.Frame{Diagnostics: &render.FrameDiagnostics{Meta: render.ScreencastMeta{ScrollOffsetY: 0}}}
	if internalRenderScrollAdvanced(unscrolled, 0) {
		t.Fatal("zero-range fixture reported an advanced scroll")
	}
	scrolled := render.Frame{Diagnostics: &render.FrameDiagnostics{Meta: render.ScreencastMeta{ScrollOffsetY: 1}}}
	if !internalRenderScrollAdvanced(scrolled, 0) {
		t.Fatal("positive scroll offset was not accepted")
	}
	if internalRenderScrollAdvanced(render.Frame{}, 0) {
		t.Fatal("frame without diagnostic scroll metadata was accepted")
	}
}

func TestWaitInternalRenderFrameFailureEvidence(t *testing.T) {
	exactPNG := encodedInternalRenderTestPNG(t, 4, 3)
	rejectFrame := render.Frame{
		EncodedPNG: exactPNG,
		Diagnostics: &render.FrameDiagnostics{Meta: render.ScreencastMeta{
			DeviceWidth: 4, DeviceHeight: 3, ScrollOffsetX: 1, ScrollOffsetY: 2,
		}},
	}
	rejectHash := internalFrameHash(rejectFrame)
	frames := make(chan render.Frame, 6)
	frames <- render.Frame{}
	frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 4, 3))}
	frames <- render.Frame{EncodedPNG: "not-base64"}
	frames <- render.Frame{EncodedPNG: encodedInternalRenderTestPNG(t, 3, 3)}
	frames <- rejectFrame
	frames <- render.Frame{
		Img: image.NewRGBA(image.Rect(0, 0, 4, 3)),
		Diagnostics: &render.FrameDiagnostics{Meta: render.ScreencastMeta{
			DeviceWidth: 4, DeviceHeight: 3, ScrollOffsetX: 1, ScrollOffsetY: 2,
		}},
	}

	stats := render.IngestStats{Frames: 9, MeanFPS: 8, MinFPS: 7, MaxFPS: 10, MedianFPS: 9, WorstGapMS: 11, DecodeMeanMS: 12, DecodeMaxMS: 13}
	_, err := waitInternalRenderFrame(frames, internalRenderCase{backend: BackendKitty, engineBackend: BackendKitty, width: 4, height: 3}, "scrolled", rejectHash, 0, 10*time.Millisecond, time.Second, func() render.IngestStats { return stats })
	if err == nil {
		t.Fatal("waitInternalRenderFrame() error = nil")
	}
	for _, want := range []string{
		"phase=scrolled", "elapsed=", "published=6", "accepted=1", "no-image=1", "missing-block-text=0",
		"malformed-png=1", "wrong-dimensions=1", "pre-scroll-hash=1", "unchanged-scroll=1", "bounds=(0,0)-(4,3)",
		"hash=", "gap=", "device=4x3 scroll=(1.0,2.0)", "accepted-hashes=[", "ingest(frames=9",
		"worst-gap-ms=11.00", "decode-mean-ms=12.00", "decode-max-ms=13.00",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure evidence %q does not contain %q", err, want)
		}
	}
}

func TestWaitInternalRenderFrameSuccessUnchanged(t *testing.T) {
	want := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 4, 3))}
	frames := make(chan render.Frame, 1)
	frames <- want
	got, err := waitInternalRenderFrame(frames, internalRenderCase{backend: BackendSixel, engineBackend: BackendSixel, width: 4, height: 3}, "top", 0, 0, time.Second, time.Millisecond, func() render.IngestStats { return render.IngestStats{} })
	if err != nil {
		t.Fatalf("waitInternalRenderFrame(): %v", err)
	}
	if got.Img != want.Img {
		t.Fatal("waitInternalRenderFrame() did not return the accepted settled frame")
	}
}

func TestValidInternalRenderFrame(t *testing.T) {
	exactPNG := encodedInternalRenderTestPNG(t, 4, 3)
	rawPNG, err := base64.StdEncoding.DecodeString(exactPNG)
	if err != nil {
		t.Fatalf("decode test PNG: %v", err)
	}
	exactImage := image.NewRGBA(image.Rect(0, 0, 4, 3))
	wrongImage := image.NewRGBA(image.Rect(0, 0, 3, 3))
	cases := []struct {
		name  string
		frame render.Frame
		tc    internalRenderCase
		want  bool
	}{
		{name: "kitty exact-size PNG", frame: render.Frame{EncodedPNG: exactPNG}, tc: internalRenderCase{backend: BackendKitty, width: 4, height: 3}, want: true},
		{name: "kitty wrong-size PNG", frame: render.Frame{EncodedPNG: exactPNG}, tc: internalRenderCase{backend: BackendKitty, width: 5, height: 3}, want: false},
		{name: "kitty malformed PNG", frame: render.Frame{EncodedPNG: base64.StdEncoding.EncodeToString([]byte("not a PNG"))}, tc: internalRenderCase{backend: BackendKitty, width: 4, height: 3}, want: false},
		{name: "kitty truncated PNG", frame: render.Frame{EncodedPNG: base64.StdEncoding.EncodeToString(rawPNG[:16])}, tc: internalRenderCase{backend: BackendKitty, width: 4, height: 3}, want: false},
		{name: "decoded image exact size", frame: render.Frame{Img: exactImage}, tc: internalRenderCase{backend: BackendSixel, width: 4, height: 3}, want: true},
		{name: "decoded image wrong size", frame: render.Frame{Img: wrongImage}, tc: internalRenderCase{backend: BackendSixel, width: 4, height: 3}, want: false},
		{name: "block image with text", frame: render.Frame{Img: exactImage, Text: []render.GlyphRun{{Text: "x"}}}, tc: internalRenderCase{backend: BackendBlock, width: 4, height: 3}, want: true},
		{name: "block image without text", frame: render.Frame{Img: exactImage}, tc: internalRenderCase{backend: BackendBlock, width: 4, height: 3}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validInternalRenderFrame(tc.frame, tc.tc); got != tc.want {
				t.Fatalf("validInternalRenderFrame() = %t, want %t", got, tc.want)
			}
		})
	}
}

func encodedInternalRenderTestPNG(t *testing.T, width, height int) string {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes())
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
	// One extra terminal row for the bottom bar keeps the compose grid at
	// exactly the captured frame grid.
	state := State{
		Backend: tc.backend,
		Scheme:  block.SchemeQuadrant,
		Colour:  ColourTrue,
		Cols:    tc.width / internalRenderCellW,
		Rows:    tc.height/internalRenderCellH + 1,
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
