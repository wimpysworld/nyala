//go:build cdpinterop

package viewer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/blockinterop"
	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/kittyinterop"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

const (
	cdpInteropCols        = 120
	cdpInteropRows        = 40
	cdpInteropCellW       = 8
	cdpInteropCellH       = 16
	cdpInteropIdle        = 600 * time.Millisecond
	cdpInteropDeadline    = 30 * time.Second
	cdpInteropSixelDelta  = 2
	cdpInteropPSNRFloor   = 24.0
	cdpInteropSSIMFloor   = 0.90
	cdpCorpusAttempts     = 2
	cdpCalibrationSettle  = 2 * time.Second
	cdpScrollRunTolerance = 2
	// The four approved counts below are the measured layout-13-linuxmatters
	// figures, not floors: cdpScrollRunTolerance supplies the whole slack. Change
	// one only against a fresh measurement, and update the fixture's
	// scroll_dance_gate reason in the same edit, because that is where the
	// baseline is recorded.
	cdpScrollApprovedPreRuns    = 56
	cdpScrollApprovedPreBody    = 43
	cdpScrollApprovedSettleBody = 48
	cdpScrollApprovedFixedRuns  = 13
)

type cdpInteropCase struct {
	fixture string
	backend string
	visible string
	hidden  []string
	wide    string
}

type cdpInteropResult struct {
	Fixture               string   `json:"fixture"`
	Backend               string   `json:"backend"`
	Passed                bool     `json:"passed"`
	Error                 string   `json:"error,omitempty"`
	SourceWidth           int      `json:"source_width,omitempty"`
	SourceHeight          int      `json:"source_height,omitempty"`
	DecodedWidth          int      `json:"decoded_width,omitempty"`
	DecodedHeight         int      `json:"decoded_height,omitempty"`
	VisibleSentinel       bool     `json:"visible_sentinel,omitempty"`
	HiddenSentinels       []string `json:"hidden_sentinels,omitempty"`
	HiddenSentinelsAbsent bool     `json:"hidden_sentinels_absent,omitempty"`
	CellModelExact        bool     `json:"cell_model_exact,omitempty"`
	ExactPixels           bool     `json:"exact_pixels,omitempty"`
	PNGPayloadExact       bool     `json:"png_payload_exact,omitempty"`
	DecoderMaxRGBDelta    int      `json:"decoder_max_rgb_delta,omitempty"`
	GoDecoderPSNR         float64  `json:"go_decoder_psnr,omitempty"`
	GoDecoderSSIM         float64  `json:"go_decoder_ssim,omitempty"`
	LibsixelPSNR          float64  `json:"libsixel_psnr,omitempty"`
	LibsixelSSIM          float64  `json:"libsixel_ssim,omitempty"`
	PaintedSource         bool     `json:"painted_source,omitempty"`
	GridDiffActive        bool     `json:"grid_diff_active,omitempty"`
	SuccessiveFrames      int      `json:"successive_frames,omitempty"`
	CJKWideCellValid      bool     `json:"cjk_wide_cell_valid,omitempty"`
}

type cdpGeometry struct {
	name         string
	cols, rows   int
	cellW, cellH int
}

type cdpCorpusResult struct {
	Fixture       string                     `json:"fixture"`
	Geometry      string                     `json:"geometry"`
	Cols          int                        `json:"cols"`
	Rows          int                        `json:"rows"`
	CellPx        [2]int                     `json:"cell_px"`
	LayoutScale   float64                    `json:"layout_scale"`
	SourceWidth   int                        `json:"source_width"`
	SourceHeight  int                        `json:"source_height"`
	PaintWidth    int                        `json:"paint_width"`
	PaintHeight   int                        `json:"paint_height"`
	Placement     metrics.PlacementStats     `json:"placement"`
	PlacementGate metrics.PlacementGate      `json:"placement_gate"`
	Paint         metrics.PaintDisplaceStats `json:"paint_displacement"`
	PaintGate     metrics.PaintGate          `json:"paint_gate"`
	PaintState    string                     `json:"paint_state"`
	PaintBaseline cdpPaintBaselineCase       `json:"paint_baseline"`
	Diagnostics   *render.FrameDiagnostics   `json:"frame_diagnostics,omitempty"`
	Error         string                     `json:"error,omitempty"`
}

type cdpPaintBaseline struct {
	SchemaVersion int                             `json:"schema_version"`
	Cases         map[string]cdpPaintBaselineCase `json:"cases"`
}

type cdpPaintBaselineCase struct {
	State              string  `json:"state"`
	MinMeasured        int     `json:"min_measured"`
	MaxSkippedFraction float64 `json:"max_skipped_fraction"`
	MaxMeanAbsH        float64 `json:"max_mean_abs_h"`
	MaxMaxAbsH         float64 `json:"max_max_abs_h"`
	MaxMeanAbsV        float64 `json:"max_mean_abs_v"`
	MaxMaxAbsV         float64 `json:"max_max_abs_v"`
}

type cdpScrollResult struct {
	Fixture       string                   `json:"fixture"`
	Geometry      string                   `json:"geometry"`
	PreSettle     metrics.ScrollDanceStats `json:"pre_settle"`
	Settled       metrics.ScrollDanceStats `json:"settled"`
	Gate          metrics.ScrollGate       `json:"gate"`
	ScrollOffsets []float64                `json:"scroll_offsets"`
	Error         string                   `json:"error,omitempty"`
}

var cdpCorpusGeometries = []cdpGeometry{
	{name: "120x40-8x16", cols: 120, rows: 40, cellW: 8, cellH: 16},
	{name: "240x48-8x16", cols: 240, rows: 48, cellW: 8, cellH: 16},
	{name: "158x86-16x33", cols: 158, rows: 86, cellW: 16, cellH: 33},
}

func TestCDPPaintBaselineDirectional(t *testing.T) {
	baseline := cdpPaintBaselineCase{
		State: "expected_failure", MinMeasured: 8, MaxSkippedFraction: 0.4,
		MaxMeanAbsH: 1, MaxMaxAbsH: 2, MaxMeanAbsV: 1, MaxMaxAbsV: 2,
	}
	improved := metrics.PaintDisplaceStats{Measured: 9, SkippedFraction: 0.3, MeanAbsH: 0.5, MaxAbsH: 1, MeanAbsV: 0.5, MaxAbsV: 1}
	if err := checkCDPPaintBaseline("expected_failure", improved, baseline); err != nil {
		t.Fatalf("directional improvement failed: %v", err)
	}
	breaches := []metrics.PaintDisplaceStats{
		{Measured: 7},
		{Measured: 8, SkippedFraction: 0.5},
		{Measured: 8, MeanAbsH: 1.1},
		{Measured: 8, MaxAbsH: 2.1},
		{Measured: 8, MeanAbsV: 1.1},
		{Measured: 8, MaxAbsV: 2.1},
	}
	for i, stats := range breaches {
		if err := checkCDPPaintBaseline("expected_failure", stats, baseline); err == nil {
			t.Errorf("breach %d passed", i)
		}
	}
	if err := checkCDPPaintBaseline("pass", improved, baseline); err == nil {
		t.Error("state change passed without baseline approval")
	}
}

// TestCDPInteropMatrix drives local pages from Chromium through each production
// terminal path and checks the emitted bytes with independent terminal decoders.
func TestCDPInteropMatrix(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "diagnostics", "cdp-interop")
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("clear prior CDP interop artefacts: %v", err)
	}
	cases := []cdpInteropCase{
		{fixture: "layout-03-cjk-vertical-ruby", backend: "block", visible: "Nyala CJK", wide: "朝"},
		{fixture: "layout-06-overlap-abspos", backend: "block", visible: "Offset label"},
		{
			fixture: "layout-09-hidden-player", backend: "block", visible: "Episode 314",
			hidden: []string{
				"Use the up and down arrow keys", "Use the Arrow keys",
				"Rewind fifteen seconds", "Skip forward fifteen seconds",
				"1.75x", "1.50x", "1.25x", "1.00x", "0.75x",
				"Playback speed control tooltip text",
			},
		},
		{fixture: "layout-13-linuxmatters", backend: "block", visible: "Linux Matters"},
		{fixture: "layout-06-overlap-abspos", backend: "kitty", visible: "Offset label"},
		{fixture: "layout-13-linuxmatters", backend: "kitty", visible: "DOSing about"},
		{fixture: "layout-06-overlap-abspos", backend: "sixel", visible: "Offset label"},
		{fixture: "layout-13-linuxmatters", backend: "sixel", visible: "DOSing about"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture+"/"+tc.backend, func(t *testing.T) {
			result := cdpInteropResult{Fixture: tc.fixture, Backend: tc.backend, HiddenSentinels: tc.hidden}
			caseDir := filepath.Join("..", "..", "testdata", "diagnostics", "cdp-interop", tc.fixture, tc.backend)
			if err := prepareCDPInteropDir(caseDir); err != nil {
				t.Fatal(err)
			}
			runErr := runCDPInteropCase(tc, caseDir, &result)
			result.Passed = runErr == nil
			if runErr != nil {
				result.Error = runErr.Error()
			}
			if err := writeCDPInteropResult(caseDir, result); err != nil {
				t.Errorf("write result: %v", err)
			}
			if runErr != nil {
				t.Error(runErr)
			}
		})
	}
}

// TestCDPCorpusCorrectness runs every local corpus fixture through the normal
// Block engine path at the three committed geometries and scores the in-memory
// text runs without a capture bundle.
func TestCDPCorpusCorrectness(t *testing.T) {
	corpusRoot := filepath.Join("..", "..", "testdata", "diagnostics", "cdp-corpus")
	if err := os.MkdirAll(corpusRoot, 0o755); err != nil {
		t.Fatalf("create corpus diagnostics: %v", err)
	}
	paintBaseline, err := loadCDPPaintBaseline(filepath.Join("..", "..", "testdata", "corpus", "paint-baseline.json"))
	if err != nil {
		t.Fatalf("load paint baseline: %v", err)
	}
	fixtures, err := filepath.Glob(filepath.Join("..", "..", "testdata", "corpus", "layout-*"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("discover corpus fixtures: %v (%d found)", err, len(fixtures))
	}
	for _, fixtureDir := range fixtures {
		fixture := filepath.Base(fixtureDir)
		gates, err := metrics.LoadFixtureGates(filepath.Join(fixtureDir, "fixture.json"))
		if err != nil {
			t.Fatalf("load %s thresholds: %v", fixture, err)
		}
		for _, geom := range cdpCorpusGeometries {
			t.Run(fixture+"/"+geom.name, func(t *testing.T) {
				baselineKey := fixture + "/" + geom.name
				paintCase, ok := paintBaseline.Cases[baselineKey]
				if !ok {
					t.Fatalf("paint baseline has no case %q", baselineKey)
				}
				caseDir := filepath.Join("..", "..", "testdata", "diagnostics", "cdp-corpus", fixture, geom.name)
				if err := prepareCDPInteropDir(caseDir); err != nil {
					t.Fatal(err)
				}
				frame, placementErr := settledCorpusFrame(fixture, geom, false)
				result := cdpCorpusResult{Fixture: fixture, Geometry: geom.name, Cols: geom.cols, Rows: geom.rows, CellPx: [2]int{geom.cellW, geom.cellH}, PlacementGate: gates.Placement, PaintGate: gates.Paint, PaintBaseline: paintCase}
				result.LayoutScale = cdpBlockLayoutScale(geom.cols, geom.cellW)
				result.Diagnostics = frame.Diagnostics
				if placementErr == nil {
					var source *image.RGBA
					source, _, placementErr = materialiseCDPInteropFrame(frame)
					if placementErr == nil {
						result.SourceWidth, result.SourceHeight = source.Bounds().Dx(), source.Bounds().Dy()
						placementErr = writeCDPInteropPNG(filepath.Join(caseDir, "source-cdp.png"), source)
					}
					if placementErr == nil {
						boxes, _ := metricRuns(frame.Text)
						cssW, cssH := float64(geom.cellW)/result.LayoutScale, float64(geom.cellH)/result.LayoutScale
						result.Placement = metrics.Placement(boxes, cssW, cssH, geom.cols, geom.rows)
						placementErr = metrics.CheckPlacementGate(result.Placement, gates.Placement)
					}
				}
				paintFrame, paintErr := settledCorpusFrame(fixture, geom, true)
				if paintErr == nil {
					var paintSource *image.RGBA
					paintSource, _, paintErr = materialiseCDPInteropFrame(paintFrame)
					if paintErr == nil {
						paintErr = writeCDPInteropPNG(filepath.Join(caseDir, "source-paint-cdp.png"), paintSource)
					}
					if paintErr == nil {
						result.PaintWidth, result.PaintHeight = paintSource.Bounds().Dx(), paintSource.Bounds().Dy()
						_, paintRuns := metricRuns(paintFrame.Text)
						result.Paint = metrics.PaintDisplacement(paintSource, paintRuns, geom.cellW, geom.cellH)
						if result.Paint.Measured == 0 {
							paintErr = errors.New("measured zero runs")
						}
						switch {
						case metrics.CheckPaintGate(result.Paint, gates.Paint) != nil:
							result.PaintState = "expected_failure"
						default:
							result.PaintState = "pass"
						}
						if baselineErr := checkCDPPaintBaseline(result.PaintState, result.Paint, paintCase); baselineErr != nil {
							paintErr = baselineErr
						}
					}
				}
				if paintErr != nil {
					paintErr = fmt.Errorf("source-paint pass: %w", paintErr)
				}
				err = errors.Join(placementErr, paintErr)
				if err != nil {
					result.Error = err.Error()
				}
				if werr := writeJSON(filepath.Join(caseDir, "result.json"), result); werr != nil {
					t.Errorf("write result: %v", werr)
				}
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
	if got, want := len(paintBaseline.Cases), len(fixtures)*len(cdpCorpusGeometries); got != want {
		t.Errorf("paint baseline cases = %d, want %d", got, want)
	}
	t.Run("layout-13-linuxmatters/scroll", func(t *testing.T) {
		geom := cdpCorpusGeometries[2]
		gates, err := metrics.LoadFixtureGates(filepath.Join("..", "..", "testdata", "corpus", "layout-13-linuxmatters", "fixture.json"))
		if err != nil {
			t.Fatal(err)
		}
		result, runErr := runCorpusScroll("layout-13-linuxmatters", geom, gates.Scroll)
		caseDir := filepath.Join("..", "..", "testdata", "diagnostics", "cdp-corpus", "layout-13-linuxmatters", "scroll")
		if err := prepareCDPInteropDir(caseDir); err != nil {
			t.Fatal(err)
		}
		if runErr != nil {
			result.Error = runErr.Error()
		}
		if err := writeJSON(filepath.Join(caseDir, "result.json"), result); err != nil {
			t.Fatal(err)
		}
		if runErr != nil {
			t.Error(runErr)
		}
	})
}

func loadCDPPaintBaseline(path string) (cdpPaintBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cdpPaintBaseline{}, err
	}
	var baseline cdpPaintBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return cdpPaintBaseline{}, err
	}
	if baseline.SchemaVersion != 1 {
		return cdpPaintBaseline{}, fmt.Errorf("schema version %d, want 1", baseline.SchemaVersion)
	}
	return baseline, nil
}

func checkCDPPaintBaseline(state string, stats metrics.PaintDisplaceStats, baseline cdpPaintBaselineCase) error {
	const epsilon = 1e-12
	switch {
	case state != baseline.State:
		return fmt.Errorf("paint state %q, approved %q", state, baseline.State)
	case stats.Measured < baseline.MinMeasured:
		return fmt.Errorf("paint measured coverage %d < approved minimum %d", stats.Measured, baseline.MinMeasured)
	case stats.SkippedFraction > baseline.MaxSkippedFraction+epsilon:
		return fmt.Errorf("paint skipped fraction %.6f > approved maximum %.6f", stats.SkippedFraction, baseline.MaxSkippedFraction)
	case stats.MeanAbsH > baseline.MaxMeanAbsH+epsilon:
		return fmt.Errorf("paint mean abs h %.6f > approved maximum %.6f", stats.MeanAbsH, baseline.MaxMeanAbsH)
	case stats.MaxAbsH > baseline.MaxMaxAbsH+epsilon:
		return fmt.Errorf("paint max abs h %.6f > approved maximum %.6f", stats.MaxAbsH, baseline.MaxMaxAbsH)
	case stats.MeanAbsV > baseline.MaxMeanAbsV+epsilon:
		return fmt.Errorf("paint mean abs v %.6f > approved maximum %.6f", stats.MeanAbsV, baseline.MaxMeanAbsV)
	case stats.MaxAbsV > baseline.MaxMaxAbsV+epsilon:
		return fmt.Errorf("paint max abs v %.6f > approved maximum %.6f", stats.MaxAbsV, baseline.MaxMaxAbsV)
	}
	return nil
}

func settledCorpusFrame(fixture string, geom cdpGeometry, paintText bool) (render.Frame, error) {
	var lastErr error
	for range cdpCorpusAttempts {
		e, err := startCorpusEngine(fixture, geom, paintText)
		if err != nil {
			lastErr = err
			continue
		}
		frame, err := waitCorpusFrame(e.Frames(), geom, 0, false, true)
		_ = e.Close()
		if err == nil {
			return frame, nil
		}
		lastErr = err
	}
	return render.Frame{}, lastErr
}

func startCorpusEngine(fixture string, geom cdpGeometry, paintText bool) (*engine.ChromiumEngine, error) {
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "corpus", fixture, "index.html"))
	if err != nil {
		return nil, err
	}
	e := engine.New("")
	e.SetDiagnosticFrameMeta(true)
	e.SetCellPx(geom.cellW, geom.cellH)
	e.SetBackend(BackendBlock)
	e.SetDiagnosticTextPaint(paintText)
	e.Resize(geom.cols, geom.rows)
	if err := e.Navigate((&url.URL{Scheme: "file", Path: path}).String()); err != nil {
		_ = e.Close()
		return nil, err
	}
	return e, nil
}

func waitCorpusFrame(frames <-chan render.Frame, geom cdpGeometry, afterScroll float64, first, requireCalibration bool) (render.Frame, error) {
	deadline := time.NewTimer(cdpInteropDeadline)
	defer deadline.Stop()
	idle := time.NewTimer(cdpInteropIdle)
	defer idle.Stop()
	var latest render.Frame
	var geometryHash uint64
	geometryStable := false
	calibration := time.NewTimer(cdpCalibrationSettle)
	if !requireCalibration {
		if !calibration.Stop() {
			<-calibration.C
		}
	}
	defer calibration.Stop()
	calibrationComplete := !requireCalibration
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return render.Frame{}, errors.New("engine frame channel closed")
			}
			if !validCorpusFrame(frame, geom) || (afterScroll > 0 && (frame.Diagnostics == nil || frame.Diagnostics.Meta.ScrollOffsetY <= afterScroll)) {
				continue
			}
			if first {
				return frame, nil
			}
			latest = frame
			if frame.Diagnostics != nil && frame.Diagnostics.CalibrationSerial > 0 && frame.Diagnostics.CalibrationSucceeded {
				calibrationComplete = true
			}
			geometry := corpusTextGeometryHash(frame.Text)
			if geometry != geometryHash {
				geometryHash = geometry
				geometryStable = false
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(cdpInteropIdle)
			}
		case <-idle.C:
			geometryStable = latest.HasImage()
			if geometryStable && calibrationComplete {
				return latest, nil
			}
			idle.Reset(cdpInteropIdle)
		case <-calibration.C:
			if !calibrationComplete {
				return render.Frame{}, fmt.Errorf("timed out waiting for calibrated %s text geometry", geom.name)
			}
			if geometryStable && latest.HasImage() {
				return latest, nil
			}
		case <-deadline.C:
			return render.Frame{}, fmt.Errorf("timed out waiting for %s frame", geom.name)
		}
	}
}

func corpusTextGeometryHash(runs []render.GlyphRun) uint64 {
	h := fnv.New64a()
	for _, run := range runs {
		_, _ = fmt.Fprintf(h, "%s\x00%d,%d,%.6f,%.6f,%.6f,%.6f;", run.Text, run.Col, run.Row, run.X, run.Y, run.W, run.H)
	}
	return h.Sum64()
}

func validCorpusFrame(frame render.Frame, geom cdpGeometry) bool {
	if frame.Img == nil || len(frame.Text) == 0 || frame.Diagnostics == nil {
		return false
	}
	wantW, wantH := corpusPaintedSurface(geom)
	bounds := frame.Img.Bounds()
	return croppedDimension(bounds.Dx(), wantW) && croppedDimension(bounds.Dy(), wantH)
}

func corpusPaintedSurface(geom cdpGeometry) (int, int) {
	surfaceW, surfaceH := geom.cols*geom.cellW, geom.rows*geom.cellH
	z := cdpBlockLayoutScale(geom.cols, geom.cellW)
	cssW := int(math.Round(float64(surfaceW) / z))
	cssH := int(math.Floor(float64(surfaceH) / z))
	return int(math.Round(float64(cssW) * z)), int(math.Round(float64(cssH) * z))
}

// cdpBlockLayoutScale mirrors the engine's block cell-density layout scale Z:
// the target CSS width anchors 8 CSS px per column held to at least the 1024
// floor, and the CSS layout width is min(target, surface), so Z = cellW/8 in
// the common case and never drops below 1.
func cdpBlockLayoutScale(cols, cellW int) float64 {
	surfW := cols * cellW
	cssW := min(max(cols*8, 1024), surfW)
	return float64(surfW) / float64(cssW)
}

// croppedDimension accepts the production painted size and Chromium's documented
// one-device-pixel edge crop, but no arbitrary resize.
func croppedDimension(got, want int) bool {
	return got == want || got == want-1
}

func metricRuns(runs []render.GlyphRun) ([]metrics.PlacementBox, []metrics.PaintRun) {
	boxes := make([]metrics.PlacementBox, len(runs))
	paint := make([]metrics.PaintRun, len(runs))
	for i, run := range runs {
		boxes[i] = metrics.PlacementBox{X: run.X, Y: run.Y, W: run.W, H: run.H, Col: run.Col, Row: run.Row, Text: run.Text}
		paint[i] = metrics.PaintRun{Text: run.Text, Col: run.Col, Row: run.Row, FG: metrics.PaintColour{R: run.FG.R, G: run.FG.G, B: run.FG.B}, X: run.X, Y: run.Y, W: run.W, H: run.H, CellW: run.CellW}
	}
	return boxes, paint
}

func scrollRuns(runs []render.GlyphRun) []metrics.ScrollRun {
	out := make([]metrics.ScrollRun, len(runs))
	for i, run := range runs {
		out[i] = metrics.ScrollRun{Text: run.Text, Col: run.Col, Row: run.Row, X: run.X, Y: run.Y}
	}
	return out
}

func runCorpusScroll(fixture string, geom cdpGeometry, gate metrics.ScrollGate) (cdpScrollResult, error) {
	var result cdpScrollResult
	var lastErr error
	for range cdpCorpusAttempts {
		result, lastErr = runCorpusScrollAttempt(fixture, geom, gate)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return result, lastErr
	}
	if result.PreSettle.Runs == 0 || result.Settled.Runs == 0 {
		return result, errors.New("scroll coverage has zero matched runs")
	}
	minRuns := cdpScrollApprovedPreRuns - cdpScrollRunTolerance
	minPreBody := cdpScrollApprovedPreBody - cdpScrollRunTolerance
	minSettleBody := cdpScrollApprovedSettleBody - cdpScrollRunTolerance
	if result.PreSettle.Runs < minRuns {
		return result, fmt.Errorf("pre-settle matched runs = %d, want at least %d", result.PreSettle.Runs, minRuns)
	}
	if body := result.PreSettle.Runs - result.PreSettle.FixedRuns; body < minPreBody {
		return result, fmt.Errorf("pre-settle non-fixed matched runs = %d, want at least %d", body, minPreBody)
	}
	if body := result.Settled.Runs - result.Settled.FixedRuns; body < minSettleBody {
		return result, fmt.Errorf("settled non-fixed matched runs = %d, want at least %d", body, minSettleBody)
	}
	// The fixed navigation cohort must be the same size mid-scroll as it is at rest.
	// The compose-time shift holds a viewport-anchored run in place rather than
	// translating it with the page, so the nav's crisp text stays on the nav's pixels
	// while the body scrolls under it. A pre-settle cohort of zero is the regression
	// this guards: it means the shift moved the fixed nav off its own background.
	if result.PreSettle.FixedRuns != cdpScrollApprovedFixedRuns {
		return result, fmt.Errorf("pre-settle fixed navigation cohort = %d, want %d", result.PreSettle.FixedRuns, cdpScrollApprovedFixedRuns)
	}
	if result.Settled.FixedRuns != cdpScrollApprovedFixedRuns {
		return result, fmt.Errorf("settled fixed navigation cohort = %d, want %d", result.Settled.FixedRuns, cdpScrollApprovedFixedRuns)
	}
	if err := metrics.CheckScrollGate(result.PreSettle, gate); err != nil {
		return result, fmt.Errorf("pre-settle: %w", err)
	}
	if err := metrics.CheckScrollGate(result.Settled, gate); err != nil {
		return result, fmt.Errorf("settled: %w", err)
	}
	return result, nil
}

func runCorpusScrollAttempt(fixture string, geom cdpGeometry, gate metrics.ScrollGate) (cdpScrollResult, error) {
	result := cdpScrollResult{Fixture: fixture, Geometry: geom.name, Gate: gate}
	e, err := startCorpusEngine(fixture, geom, false)
	if err != nil {
		return result, err
	}
	defer e.Close()
	before, err := waitCorpusFrame(e.Frames(), geom, 0, false, true)
	if err != nil {
		return result, err
	}
	baseScroll := before.Diagnostics.Meta.ScrollOffsetY
	e.SendScroll(600)
	pre, err := waitCorpusFrame(e.Frames(), geom, baseScroll, true, false)
	if err != nil {
		return result, err
	}
	settled, err := waitCorpusFrame(e.Frames(), geom, baseScroll, false, false)
	if err != nil {
		return result, err
	}
	result.ScrollOffsets = []float64{baseScroll, pre.Diagnostics.Meta.ScrollOffsetY, settled.Diagnostics.Meta.ScrollOffsetY}
	cssScale := cdpBlockLayoutScale(geom.cols, geom.cellW)
	cellH, bucket := float64(geom.cellH)/cssScale, float64(geom.cellW)/cssScale
	result.PreSettle = metrics.ScrollDance([][]metrics.ScrollRun{scrollRuns(before.Text), scrollRuns(pre.Text)}, result.ScrollOffsets[:2], cellH, bucket)
	result.Settled = metrics.ScrollDance([][]metrics.ScrollRun{scrollRuns(before.Text), scrollRuns(settled.Text)}, []float64{result.ScrollOffsets[0], result.ScrollOffsets[2]}, cellH, bucket)
	return result, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func runCDPInteropCase(tc cdpInteropCase, caseDir string, result *cdpInteropResult) error {
	frames, err := captureCDPInteropFrames(tc)
	if err != nil {
		return err
	}
	result.SuccessiveFrames = len(frames)
	frame := frames[len(frames)-1]
	source, sourcePNG, err := materialiseCDPInteropFrame(frame)
	if err != nil {
		return err
	}
	result.SourceWidth, result.SourceHeight = source.Bounds().Dx(), source.Bounds().Dy()
	if tc.backend != "block" {
		result.PaintedSource = paintedCDPInteropSource(source)
		if !result.PaintedSource {
			return fmt.Errorf("fixture %s produced a blank or placeholder source frame", tc.fixture)
		}
	}
	if err := writeCDPInteropPNG(filepath.Join(caseDir, "source-cdp.png"), source); err != nil {
		return err
	}

	// The engine grid at cdpInteropRows is the page grid: the viewer State
	// carries one extra terminal row for the bottom bar, so the production
	// compose and placement seams target exactly the captured grid.
	state := NewState(cdpInteropCols, cdpInteropRows+1)
	state.Scheme = block.SchemeQuadrant
	state.Colour = ColourTrue
	state.Backend = cdpInteropBackend(tc.backend)
	var terminal bytes.Buffer
	switch tc.backend {
	case "block":
		return runCDPInteropBlock(state, frames, tc, caseDir, result, &terminal)
	case "kitty":
		return runCDPInteropKitty(state, frame, source, sourcePNG, caseDir, result, &terminal)
	case "sixel":
		sixel2png, err := exec.LookPath("sixel2png")
		if err != nil {
			return errors.New("sixel2png not found; run this gate through `just cdp-interop` in the Nix dev shell")
		}
		return runCDPInteropSixel(state, frame, source, sixel2png, caseDir, result, &terminal)
	default:
		return fmt.Errorf("unknown interop backend %q", tc.backend)
	}
}

func captureCDPInteropFrames(tc cdpInteropCase) ([]render.Frame, error) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "corpus", tc.fixture, "index.html"))
	if err != nil {
		return nil, fmt.Errorf("resolve fixture: %w", err)
	}
	fixtureURL := (&url.URL{Scheme: "file", Path: fixture}).String()
	e := engine.New("")
	e.SetCellPx(cdpInteropCellW, cdpInteropCellH)
	e.SetBackend(cdpInteropBackend(tc.backend))
	e.Resize(cdpInteropCols, cdpInteropRows)
	if err := e.Navigate(fixtureURL); err != nil {
		_ = e.Close()
		return nil, fmt.Errorf("navigate %s: %w", tc.fixture, err)
	}
	defer e.Close()
	first, err := waitCDPInteropFrame(e.Frames(), tc.backend, 0)
	if err != nil {
		return nil, err
	}
	frames := []render.Frame{first}
	if tc.backend == "block" && tc.fixture == "layout-13-linuxmatters" {
		e.SendScroll(cdpInteropRows * cdpInteropCellH / 2)
		second, err := waitCDPInteropFrame(e.Frames(), tc.backend, cdpInteropFrameHash(first))
		if err != nil {
			return nil, err
		}
		frames = append(frames, second)
	}
	return frames, nil
}

func waitCDPInteropFrame(frames <-chan render.Frame, backend string, rejectHash uint64) (render.Frame, error) {
	deadline := time.NewTimer(cdpInteropDeadline)
	defer deadline.Stop()
	idle := time.NewTimer(cdpInteropIdle)
	defer idle.Stop()
	var latest render.Frame
	observed := "no frames"
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return render.Frame{}, errors.New("engine frame channel closed")
			}
			observed = describeCDPInteropFrame(frame)
			if !validCDPInteropFrame(frame, backend) || cdpInteropFrameHash(frame) == rejectHash {
				continue
			}
			latest = frame
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(cdpInteropIdle)
		case <-idle.C:
			if latest.HasImage() {
				return latest, nil
			}
			idle.Reset(cdpInteropIdle)
		case <-deadline.C:
			return render.Frame{}, fmt.Errorf("timed out waiting for settled %s frame; last observed %s", backend, observed)
		}
	}
}

func describeCDPInteropFrame(frame render.Frame) string {
	if frame.Img != nil {
		return fmt.Sprintf("image %v with %d text runs", frame.Img.Bounds(), len(frame.Text))
	}
	return fmt.Sprintf("encoded PNG bytes %d with %d text runs", len(frame.EncodedPNG), len(frame.Text))
}

func validCDPInteropFrame(frame render.Frame, backend string) bool {
	if !frame.HasImage() || (backend == "block" && len(frame.Text) == 0) {
		return false
	}
	img, _, err := materialiseCDPInteropFrame(frame)
	if err != nil {
		return false
	}
	wantW, wantH := cdpInteropCols*cdpInteropCellW, cdpInteropRows*cdpInteropCellH
	b := img.Bounds()
	if b.Dx() != wantW || b.Dy() != wantH {
		return false
	}
	return backend == "block" || paintedCDPInteropSource(img)
}

func cdpInteropFrameHash(frame render.Frame) uint64 {
	h := fnv.New64a()
	if frame.EncodedPNG != "" {
		_, _ = io.WriteString(h, frame.EncodedPNG)
	} else if frame.Img != nil {
		b := frame.Img.Bounds()
		var pixel [4]byte
		for y := b.Min.Y; y < b.Max.Y; y += 16 {
			for x := b.Min.X; x < b.Max.X; x += 16 {
				c := color.RGBAModel.Convert(frame.Img.At(x, y)).(color.RGBA)
				pixel = [4]byte{c.R, c.G, c.B, c.A}
				_, _ = h.Write(pixel[:])
			}
		}
	}
	for _, run := range frame.Text {
		_, _ = fmt.Fprintf(h, "%d,%d,%s;", run.Col, run.Row, run.Text)
	}
	return h.Sum64()
}

func paintedCDPInteropSource(img *image.RGBA) bool {
	b := img.Bounds()
	background := img.RGBAAt(b.Min.X, b.Min.Y)
	distinct := make(map[uint16]struct{})
	different := 0
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := b.Min.X; x < b.Max.X; x += 4 {
			c := img.RGBAAt(x, y)
			key := uint16(c.R>>4)<<8 | uint16(c.G>>4)<<4 | uint16(c.B>>4)
			distinct[key] = struct{}{}
			if int(absCDPInteropByte(c.R, background.R))+int(absCDPInteropByte(c.G, background.G))+int(absCDPInteropByte(c.B, background.B)) >= 24 {
				different++
			}
		}
	}
	return different >= 200 && len(distinct) >= 8
}

func runCDPInteropBlock(state State, frames []render.Frame, tc cdpInteropCase, caseDir string, result *cdpInteropResult, terminal *bytes.Buffer) error {
	var expected block.Grid
	state.SetDiff(&block.GridDiff{})
	result.GridDiffActive = true
	for i, frame := range frames {
		grid := state.ComposeBlockFrame(frame)
		if grid == nil {
			return fmt.Errorf("compose Block frame %d returned nil", i+1)
		}
		expected = block.Grid{Cols: grid.Cols, Rows: grid.Rows, Cells: append([]block.Cell(nil), grid.Cells...)}
		before := terminal.Len()
		if err := state.RenderStreamFrame(frame, terminal); err != nil {
			return fmt.Errorf("render Block frame %d: %w", i+1, err)
		}
		if terminal.Len() == before {
			return fmt.Errorf("GridDiff emitted no bytes for browser frame %d", i+1)
		}
	}
	decoded, err := blockinterop.Decode(terminal.Bytes(), cdpInteropCols, cdpInteropRows)
	if err != nil {
		return fmt.Errorf("decode Block terminal stream: %w", err)
	}
	result.CellModelExact = equalCDPInteropGrids(decoded, expected)
	if !result.CellModelExact {
		return errors.New("Block terminal model differs from the production composed grid")
	}
	terminalText := cdpInteropGridText(decoded)
	if err := os.WriteFile(filepath.Join(caseDir, "grid.txt"), []byte(terminalText), 0o600); err != nil {
		return fmt.Errorf("write Block grid: %w", err)
	}
	result.VisibleSentinel = strings.Contains(terminalText, tc.visible)
	if !result.VisibleSentinel {
		return fmt.Errorf("decoded Block terminal lacks visible sentinel %q", tc.visible)
	}
	result.HiddenSentinelsAbsent = true
	for _, hidden := range tc.hidden {
		if strings.Contains(terminalText, hidden) {
			result.HiddenSentinelsAbsent = false
			return fmt.Errorf("decoded Block terminal contains hidden text %q", hidden)
		}
	}
	if tc.wide != "" {
		result.CJKWideCellValid = validCDPInteropCJKWideCell(decoded, tc.wide)
		if !result.CJKWideCellValid {
			return errors.New("decoded Block terminal lacks a valid CJK wide lead and trailing cell")
		}
	}
	return nil
}

func runCDPInteropKitty(state State, frame render.Frame, source *image.RGBA, sourcePNG []byte, caseDir string, result *cdpInteropResult, terminal *bytes.Buffer) error {
	if err := state.RenderStreamFrame(frame, terminal); err != nil {
		return fmt.Errorf("render Kitty frame: %w", err)
	}
	decoded, err := kittyinterop.Decode(terminal.Bytes())
	if err != nil {
		return fmt.Errorf("decode Kitty terminal stream: %w", err)
	}
	if len(decoded.Events) != 1 || decoded.Events[0].Kind != kittyinterop.EventTransmit || decoded.LastPlaced == 0 {
		return fmt.Errorf("Kitty stream produced %d events and last id %d, want one placed transmit", len(decoded.Events), decoded.LastPlaced)
	}
	event := decoded.Events[0]
	if event.Format != 100 {
		return fmt.Errorf("Kitty format = %d, want direct PNG f=100", event.Format)
	}
	if err := writeCDPInteropPNG(filepath.Join(caseDir, "decoded-terminal.png"), event.Image); err != nil {
		return err
	}
	result.DecodedWidth, result.DecodedHeight = event.Width, event.Height
	result.PNGPayloadExact = bytes.Equal(event.Payload, sourcePNG)
	result.ExactPixels = equalCDPInteropImages(event.Image, source)
	if !result.PNGPayloadExact || !result.ExactPixels {
		return errors.New("Kitty payload or decoded pixels differ from the retained CDP PNG")
	}
	return nil
}

func runCDPInteropSixel(state State, frame render.Frame, source *image.RGBA, sixel2png, caseDir string, result *cdpInteropResult, terminal *bytes.Buffer) error {
	if err := state.RenderStreamFrame(frame, terminal); err != nil {
		return fmt.Errorf("render Sixel frame: %w", err)
	}
	payload, err := isolateCDPInteropSixel(terminal.Bytes())
	if err != nil {
		return err
	}
	goDecoded, err := renderdecode.DecodeSixel(payload)
	if err != nil {
		return fmt.Errorf("decode Sixel with Nyala decoder: %w", err)
	}
	libDecoded, err := decodeCDPInteropLibsixel(sixel2png, payload)
	if err != nil {
		return err
	}
	if err := writeCDPInteropPNG(filepath.Join(caseDir, "decoded-terminal.png"), libDecoded); err != nil {
		return err
	}
	diff, maxDelta, err := diffCDPInteropImages(goDecoded, libDecoded)
	if err != nil {
		return err
	}
	if err := writeCDPInteropPNG(filepath.Join(caseDir, "diff.png"), diff); err != nil {
		return err
	}
	result.DecodedWidth, result.DecodedHeight = libDecoded.Bounds().Dx(), libDecoded.Bounds().Dy()
	result.DecoderMaxRGBDelta = maxDelta
	if !source.Bounds().Eq(goDecoded.Bounds()) || !source.Bounds().Eq(libDecoded.Bounds()) {
		return fmt.Errorf("Sixel bounds source=%v go=%v libsixel=%v", source.Bounds(), goDecoded.Bounds(), libDecoded.Bounds())
	}
	goScore := metrics.ScorePair(goDecoded, source)
	libScore := metrics.ScorePair(libDecoded, source)
	result.GoDecoderPSNR, result.GoDecoderSSIM = goScore.PSNR, goScore.SSIM
	result.LibsixelPSNR, result.LibsixelSSIM = libScore.PSNR, libScore.SSIM
	if maxDelta > cdpInteropSixelDelta {
		return fmt.Errorf("Sixel decoders differ by %d RGB levels, want <= %d", maxDelta, cdpInteropSixelDelta)
	}
	if goScore.PSNR < cdpInteropPSNRFloor || libScore.PSNR < cdpInteropPSNRFloor || goScore.SSIM < cdpInteropSSIMFloor || libScore.SSIM < cdpInteropSSIMFloor {
		return fmt.Errorf("Sixel source quality below floor: go %.2f dB/%.5f, libsixel %.2f dB/%.5f", goScore.PSNR, goScore.SSIM, libScore.PSNR, libScore.SSIM)
	}
	return nil
}

func materialiseCDPInteropFrame(frame render.Frame) (*image.RGBA, []byte, error) {
	if frame.EncodedPNG != "" {
		raw, err := base64.StdEncoding.DecodeString(frame.EncodedPNG)
		if err != nil {
			return nil, nil, fmt.Errorf("decode retained CDP base64: %w", err)
		}
		decoded, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, nil, fmt.Errorf("decode retained CDP PNG: %w", err)
		}
		return opaqueCDPInteropRGBA(decoded), raw, nil
	}
	if frame.Img == nil {
		return nil, nil, errors.New("frame has no materialisable image")
	}
	return opaqueCDPInteropRGBA(frame.Img), nil, nil
}

func isolateCDPInteropSixel(stream []byte) ([]byte, error) {
	if bytes.Count(stream, []byte("\x1bP")) != 1 || bytes.Count(stream, []byte("\x1b\\")) != 1 {
		return nil, errors.New("terminal stream must contain exactly one complete Sixel DCS")
	}
	if !bytes.HasPrefix(stream, []byte("\x1bP0;0;8q")) || !bytes.HasSuffix(stream, []byte("\x1b\\")) {
		return nil, errors.New("terminal stream has invalid Sixel introducer or terminator")
	}
	wantRaster := []byte(fmt.Sprintf("\"1;1;%d;%d", cdpInteropCols*cdpInteropCellW, cdpInteropRows*cdpInteropCellH))
	if !bytes.Contains(stream, wantRaster) {
		return nil, fmt.Errorf("Sixel DCS lacks raster dimensions %q", wantRaster)
	}
	if !bytes.Contains(stream, []byte{'#'}) {
		return nil, errors.New("Sixel DCS lacks a palette definition")
	}
	return append([]byte(nil), stream...), nil
}

func decodeCDPInteropLibsixel(tool string, payload []byte) (*image.RGBA, error) {
	dir, err := os.MkdirTemp("", "nyala-cdp-interop-sixel-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input.sixel"), filepath.Join(dir, "output.png")
	if err := os.WriteFile(input, payload, 0o600); err != nil {
		return nil, err
	}
	if combined, err := exec.Command(tool, "-i", input, "-o", output).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sixel2png: %w: %s", err, combined)
	}
	f, err := os.Open(output)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoded, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode libsixel PNG: %w", err)
	}
	return opaqueCDPInteropRGBA(decoded), nil
}

func prepareCDPInteropDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"source-cdp.png", "decoded-terminal.png", "diff.png", "grid.txt", "result.json"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeCDPInteropPNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := cellraster.EncodePNG(f, img); err != nil {
		_ = f.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func writeCDPInteropResult(dir string, result cdpInteropResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dir, "result.json"), data, 0o600)
}

func cdpInteropGridText(grid block.Grid) string {
	var text strings.Builder
	for row := range grid.Rows {
		for col := range grid.Cols {
			text.WriteString(visibleCDPInteropCell(grid.At(col, row)))
		}
		text.WriteByte('\n')
	}
	return text.String()
}

func validCDPInteropCJKWideCell(grid block.Grid, sentinel string) bool {
	for row := range grid.Rows {
		for col := 0; col+1 < grid.Cols; col++ {
			lead := grid.At(col, row)
			text := visibleCDPInteropCell(lead)
			if text != sentinel || render.CellWidth(text) != 2 {
				continue
			}
			trail := grid.At(col+1, row)
			if trail.Wide && trail.FG == lead.FG && trail.BG == lead.BG && visibleCDPInteropCell(trail) == "" {
				return true
			}
		}
	}
	return false
}

func cdpInteropBackend(name string) Backend {
	switch name {
	case "kitty":
		return BackendKitty
	case "sixel":
		return BackendSixel
	default:
		return BackendBlock
	}
}

func opaqueCDPInteropRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

func equalCDPInteropImages(got, want *image.RGBA) bool {
	return got != nil && want != nil && got.Bounds().Eq(want.Bounds()) && bytes.Equal(got.Pix, want.Pix)
}

func equalCDPInteropGrids(got, want block.Grid) bool {
	if got.Cols != want.Cols || got.Rows != want.Rows {
		return false
	}
	for row := range want.Rows {
		for col := range want.Cols {
			g, w := got.At(col, row), want.At(col, row)
			if g.Wide != w.Wide || g.FG != w.FG || g.BG != w.BG || visibleCDPInteropCell(g) != visibleCDPInteropCell(w) {
				return false
			}
		}
	}
	return true
}

func visibleCDPInteropCell(cell *block.Cell) string {
	if cell.Wide {
		return ""
	}
	if cell.Text != "" {
		return cell.Text
	}
	return string(cell.Glyph)
}

func diffCDPInteropImages(a, b *image.RGBA) (*image.RGBA, int, error) {
	if !a.Bounds().Eq(b.Bounds()) {
		return nil, 0, fmt.Errorf("decoder image bounds differ: %v and %v", a.Bounds(), b.Bounds())
	}
	diff := image.NewRGBA(a.Bounds())
	maxDelta := 0
	for y := range a.Bounds().Dy() {
		for x := range a.Bounds().Dx() {
			ac, bc := a.RGBAAt(x, y), b.RGBAAt(x, y)
			dr, dg, db := absCDPInteropByte(ac.R, bc.R), absCDPInteropByte(ac.G, bc.G), absCDPInteropByte(ac.B, bc.B)
			maxDelta = max(maxDelta, int(dr), int(dg), int(db))
			diff.SetRGBA(x, y, color.RGBA{R: dr, G: dg, B: db, A: 0xff})
		}
	}
	return diff, maxDelta, nil
}

func absCDPInteropByte(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}
