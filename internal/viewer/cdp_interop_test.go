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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/blockinterop"
	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/kittyinterop"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

const (
	cdpInteropCols        = 120
	cdpInteropRows        = 40
	cdpInteropCellW       = 8
	cdpInteropCellH       = 16
	cdpInteropLayoutWidth = 1280
	cdpInteropIdle        = 600 * time.Millisecond
	cdpInteropDeadline    = 30 * time.Second
	cdpInteropSixelDelta  = 2
	cdpInteropPSNRFloor   = 24.0
	cdpInteropSSIMFloor   = 0.90
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

	state := NewState(cdpInteropCols, cdpInteropRows, result.SourceWidth, result.SourceHeight)
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
	e.SetLayoutWidth(cdpInteropLayoutWidth)
	e.SetScheme("quadrant")
	e.SetBackend(tc.backend)
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
	state.SetCaptureGrid(&expected)
	state.SetDiff(&block.GridDiff{})
	result.GridDiffActive = true
	for i, frame := range frames {
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
	rasteriser, err := capture.NewRasteriser()
	if err != nil {
		return fmt.Errorf("create Block rasteriser: %w", err)
	}
	want := rasteriser.Rasterise(expected)
	got := rasteriser.Rasterise(decoded)
	if err := writeCDPInteropPNG(filepath.Join(caseDir, "expected-composed.png"), want); err != nil {
		return err
	}
	if err := writeCDPInteropPNG(filepath.Join(caseDir, "decoded-terminal.png"), got); err != nil {
		return err
	}
	result.DecodedWidth, result.DecodedHeight = got.Bounds().Dx(), got.Bounds().Dy()
	result.CellModelExact = equalCDPInteropGrids(decoded, expected)
	result.ExactPixels = got.Bounds().Eq(want.Bounds()) && bytes.Equal(got.Pix, want.Pix)
	if !result.CellModelExact || !result.ExactPixels {
		return errors.New("Block terminal model or raster differs from the production composed grid")
	}
	terminalText := cdpInteropGridText(decoded)
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
	goDecoded, err := capture.DecodeSixel(payload)
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
	expectedViewerPrefix := []byte(cursorPosition(0, 0))
	if !bytes.HasPrefix(stream, expectedViewerPrefix) {
		return nil, errors.New("terminal stream lacks the expected viewer placement prefix")
	}
	stream = stream[len(expectedViewerPrefix):]
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
	for _, name := range []string{"source-cdp.png", "decoded-terminal.png", "expected-composed.png", "diff.png", "result.json"} {
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
	if err := capture.EncodePNG(f, img); err != nil {
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
