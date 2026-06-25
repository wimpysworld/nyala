package capture

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// testImage returns a small opaque-red RGBA image, the bitmap a drawn frame
// carries. EncodePNG turns it into screenshot.png.
func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{R: 0xff, A: 0xff}
	for y := range h {
		for x := range w {
			img.Set(x, y, red)
		}
	}
	return img
}

// testGrid returns a 3x2 block grid with a known glyph and colours, the composed
// grid a block capture serialises into terminal-model.json and flattens into
// text.txt.
func testGrid() block.Grid {
	fg := color.RGBA{R: 0xff, A: 0xff}
	bg := color.RGBA{B: 0xff, A: 0xff}
	g := block.Grid{Cols: 3, Rows: 2, Cells: make([]block.Cell, 6)}
	for i := range g.Cells {
		g.Cells[i] = block.Cell{Glyph: 'X', FG: fg, BG: bg}
	}
	return g
}

// frameInput builds one FrameInput for a block capture: a red bitmap, two glyph
// runs, the test grid, screencast metadata, and a viewer-phase perf record.
func blockFrameInput() FrameInput {
	grid := testGrid()
	return FrameInput{
		Frame: render.Frame{
			Img: testImage(24, 32),
			Text: []render.GlyphRun{
				{Col: 0, Row: 0, Text: "Hi", FG: color.RGBA{A: 0xff}, X: 1, Y: 2, W: 16, H: 16},
				{Col: 1, Row: 1, Text: "Z", FG: color.RGBA{A: 0xff}, X: 8, Y: 16, W: 8, H: 16},
			},
			Stats: &render.FrameStats{
				ScreencastPayloadBytes: 4096,
				ScreencastDecodeMS:     1.5,
				DOMSnapshotMS:          2.5,
			},
		},
		Meta: &render.ScreencastMeta{
			OffsetTop:       0,
			PageScaleFactor: 1,
			DeviceWidth:     800,
			DeviceHeight:    600,
			ScrollOffsetY:   120,
		},
		Grid: &grid,
		Perf: ViewerPerf{
			GridCols: 3, GridRows: 2,
			CellPxW: 8, CellPxH: 16,
			CapturePxW: 24, CapturePxH: 32,
			DrawPxW: 24, DrawPxH: 32,
			GlyphRuns: 2, OverlayCells: 3,
			FitDownscaleMS:  0.3,
			BackendEncodeMS: 0.4,
			BlockComposeMS:  0.2,
			TerminalWriteMS: 0.5,
			TerminalBytes:   1024,
		},
	}
}

// graphicsMeta returns run metadata for a graphics-backend capture (kitty or
// sixel). The bundle omits terminal-model.json and the manifest carries the note.
func graphicsMeta(backend Backend) Meta {
	return Meta{
		Command:            "nyala https://example.test",
		Backend:            backend,
		TerminalCapability: string(backend),
		FramesRequested:    1,
		NavigationURL:      "https://example.test",
	}
}

func blockMeta(n int) Meta {
	return Meta{
		Command:            "nyala https://example.test",
		Backend:            BackendBlock,
		TerminalCapability: "block",
		FramesRequested:    n,
		NavigationURL:      "https://example.test",
	}
}

// readDirNames lists the entry names directly under dir.
func readDirNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

func TestBlockBundleLayout(t *testing.T) {
	b, err := NewBundle(blockMeta(2))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	for i := range 2 {
		if werr := b.WriteFrame(blockFrameInput()); werr != nil {
			t.Fatalf("WriteFrame %d: %v", i, werr)
		}
	}
	if ferr := b.FinaliseFull(); ferr != nil {
		t.Fatalf("FinaliseFull: %v", ferr)
	}

	root := readDirNames(t, b.Path())
	wantRoot := []string{
		"manifest.json", "report.md", "performance.json", "terminal-stream.raw",
		"cdp-session.json", "cdp-events.ndjson", "frames",
	}
	for _, name := range wantRoot {
		if !root[name] {
			t.Errorf("root missing %s", name)
		}
	}
	// cdp files sit at root, not under cdp/.
	if root["cdp"] {
		t.Errorf("cdp files must sit at root, not under cdp/")
	}

	framesDir := filepath.Join(b.Path(), "frames")
	frameNames := readDirNames(t, framesDir)
	for _, want := range []string{"frame-000001", "frame-000002"} {
		if !frameNames[want] {
			t.Errorf("frames missing %s", want)
		}
	}

	frame1 := filepath.Join(framesDir, "frame-000001")
	per := readDirNames(t, frame1)
	for _, name := range []string{"screenshot.png", "text.txt", "cdp-payload.json", "text-map.json", "terminal-model.json", "block-render.png"} {
		if !per[name] {
			t.Errorf("frame-000001 missing %s", name)
		}
	}
}

func TestSingleFrameUsesFrame000001(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	if ferr := b.FinaliseFull(); ferr != nil {
		t.Fatalf("FinaliseFull: %v", ferr)
	}
	names := readDirNames(t, filepath.Join(b.Path(), "frames"))
	if !names["frame-000001"] {
		t.Errorf("single-frame capture must use frame-000001, got %v", names)
	}
	if names["frame-000002"] {
		t.Errorf("single-frame capture must not create frame-000002")
	}
}

func TestScreenshotDecodesNonEmptyRGBA(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	f, err := os.Open(filepath.Join(b.Path(), "frames", "frame-000001", "screenshot.png"))
	if err != nil {
		t.Fatalf("open screenshot.png: %v", err)
	}
	defer func() { _ = f.Close() }()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode screenshot.png: %v", err)
	}
	bnds := img.Bounds()
	if bnds.Dx() == 0 || bnds.Dy() == 0 {
		t.Errorf("screenshot.png decoded to an empty image: %v", bnds)
	}
}

func TestCDPPayloadCarriesNoImageBytes(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "frames", "frame-000001", "cdp-payload.json"))
	if err != nil {
		t.Fatalf("read cdp-payload.json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal cdp-payload.json: %v", err)
	}
	for _, banned := range []string{"data", "Data"} {
		if _, ok := payload[banned]; ok {
			t.Errorf("cdp-payload.json carries image field %q", banned)
		}
	}
	// The metadata fields must be present, proving it is the screencast metadata.
	if _, ok := payload["scroll_offset_y"]; !ok {
		t.Errorf("cdp-payload.json missing screencast metadata field scroll_offset_y")
	}
}

// TestCDPPayloadCarriesScreencastMeta asserts a frame whose FrameInput.Meta is
// set writes a cdp-payload.json carrying the scroll offset and page scale values
// under snake_case keys, not an empty object, and still no image bytes.
func TestCDPPayloadCarriesScreencastMeta(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	in := blockFrameInput()
	in.Meta = &render.ScreencastMeta{
		PageScaleFactor: 2,
		DeviceWidth:     1024,
		DeviceHeight:    768,
		ScrollOffsetX:   33,
		ScrollOffsetY:   240,
	}
	if werr := b.WriteFrame(in); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "frames", "frame-000001", "cdp-payload.json"))
	if err != nil {
		t.Fatalf("read cdp-payload.json: %v", err)
	}
	if strings.TrimSpace(string(raw)) == "{}" {
		t.Fatalf("cdp-payload.json is empty object, want the screencast metadata")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal cdp-payload.json: %v", err)
	}
	if got := payload["scroll_offset_y"]; got != float64(240) {
		t.Errorf("cdp-payload.json scroll_offset_y = %v, want 240", got)
	}
	if got := payload["scroll_offset_x"]; got != float64(33) {
		t.Errorf("cdp-payload.json scroll_offset_x = %v, want 33", got)
	}
	if got := payload["page_scale_factor"]; got != float64(2) {
		t.Errorf("cdp-payload.json page_scale_factor = %v, want 2", got)
	}
	for _, banned := range []string{"data", "Data"} {
		if _, ok := payload[banned]; ok {
			t.Errorf("cdp-payload.json carries image field %q", banned)
		}
	}
}

func TestTextMapSerialisesGlyphRuns(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "frames", "frame-000001", "text-map.json"))
	if err != nil {
		t.Fatalf("read text-map.json: %v", err)
	}
	var runs []render.GlyphRun
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatalf("unmarshal text-map.json: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("text-map.json: got %d runs, want 2", len(runs))
	}
	if runs[0].Text != "Hi" || runs[0].Col != 0 || runs[0].Row != 0 {
		t.Errorf("text-map.json run 0 = %+v", runs[0])
	}
	if runs[0].X != 1 || runs[0].Y != 2 || runs[0].W != 16 || runs[0].H != 16 {
		t.Errorf("text-map.json run 0 geometry = (X %v, Y %v, W %v, H %v), want (1, 2, 16, 16)", runs[0].X, runs[0].Y, runs[0].W, runs[0].H)
	}
}

func TestTextFlattenPlacesRunsAtColumns(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "frames", "frame-000001", "text.txt"))
	if err != nil {
		t.Fatalf("read text.txt: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("text.txt: got %d lines, want 2 (one per grid row): %q", len(lines), string(raw))
	}
	// Run "Hi" at row 0 col 0.
	if lines[0] != "Hi" {
		t.Errorf("text.txt row 0 = %q, want %q", lines[0], "Hi")
	}
	// Run "Z" at row 1 col 1: a blank column precedes it.
	if lines[1] != " Z" {
		t.Errorf("text.txt row 1 = %q, want %q (blank between)", lines[1], " Z")
	}
}

func TestTerminalModelExplicitView(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "frames", "frame-000001", "terminal-model.json"))
	if err != nil {
		t.Fatalf("read terminal-model.json: %v", err)
	}
	var doc struct {
		Cols  int `json:"cols"`
		Rows  int `json:"rows"`
		Cells []struct {
			Glyph string                     `json:"glyph"`
			FG    struct{ R, G, B, A uint8 } `json:"fg"`
			BG    struct{ R, G, B, A uint8 } `json:"bg"`
			Wide  bool                       `json:"wide"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal terminal-model.json: %v", err)
	}
	if doc.Cols != 3 || doc.Rows != 2 {
		t.Errorf("terminal-model.json size = %dx%d, want 3x2", doc.Cols, doc.Rows)
	}
	if len(doc.Cells) != 6 {
		t.Fatalf("terminal-model.json cells = %d, want 6", len(doc.Cells))
	}
	// Glyph is a string, not a rune number.
	if doc.Cells[0].Glyph != "X" {
		t.Errorf("terminal-model.json glyph = %q, want \"X\"", doc.Cells[0].Glyph)
	}
	if doc.Cells[0].FG.R != 0xff {
		t.Errorf("terminal-model.json fg.r = %d, want 255", doc.Cells[0].FG.R)
	}
	if doc.Cells[0].BG.B != 0xff {
		t.Errorf("terminal-model.json bg.b = %d, want 255", doc.Cells[0].BG.B)
	}
	// The raw JSON must not encode the glyph as a bare rune number.
	if strings.Contains(string(raw), `"glyph": 88`) {
		t.Errorf("terminal-model.json encoded glyph as a rune number")
	}
}

// TestBlockRenderDimensions writes a block frame whose grid is 120x40 and asserts
// the bundle rasterises it to block-render.png at 960x640, the pinned 8x16 cell.
func TestBlockRenderDimensions(t *testing.T) {
	b, err := NewBundle(blockMeta(1))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	grid := block.Grid{Cols: 120, Rows: 40, Cells: make([]block.Cell, 120*40)}
	fg := color.RGBA{R: 0xff, A: 0xff}
	bg := color.RGBA{B: 0xff, A: 0xff}
	for i := range grid.Cells {
		grid.Cells[i] = block.Cell{Glyph: 'X', FG: fg, BG: bg}
	}
	in := blockFrameInput()
	in.Grid = &grid
	if werr := b.WriteFrame(in); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	f, err := os.Open(filepath.Join(b.Path(), "frames", "frame-000001", "block-render.png"))
	if err != nil {
		t.Fatalf("open block-render.png: %v", err)
	}
	defer func() { _ = f.Close() }()
	img, derr := png.Decode(f)
	if derr != nil {
		t.Fatalf("decode block-render.png: %v", derr)
	}
	bnds := img.Bounds()
	if bnds.Dx() != 960 || bnds.Dy() != 640 {
		t.Errorf("block-render.png = %dx%d, want 960x640", bnds.Dx(), bnds.Dy())
	}
}

// TestReadTerminalModelRoundTrip writes a grid the way the bundle does, then
// reads it back with ReadTerminalModel and asserts every cell field survives:
// glyph, overlay text, foreground, background, and the wide flag.
func TestReadTerminalModelRoundTrip(t *testing.T) {
	fg := color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff}
	bg := color.RGBA{R: 0x40, G: 0x50, B: 0x60, A: 0xff}
	want := block.Grid{Cols: 2, Rows: 2, Cells: []block.Cell{
		{Glyph: 'A', FG: fg, BG: bg},
		{Glyph: 'B', Text: "界", FG: fg, BG: bg},
		{Wide: true, BG: bg},
		{Glyph: ' ', FG: fg, BG: bg},
	}}

	path := filepath.Join(t.TempDir(), "terminal-model.json")
	if err := writeJSON(path, gridView(&want)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	got, err := ReadTerminalModel(path)
	if err != nil {
		t.Fatalf("ReadTerminalModel: %v", err)
	}
	if got.Cols != want.Cols || got.Rows != want.Rows {
		t.Fatalf("size = %dx%d, want %dx%d", got.Cols, got.Rows, want.Cols, want.Rows)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip grid mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestReadTerminalModelRejectsBadCount rejects a model whose cell count does not
// match Cols*Rows, the shape a non-block or truncated model would have.
func TestReadTerminalModelRejectsBadCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal-model.json")
	if err := os.WriteFile(path, []byte(`{"cols":3,"rows":3,"cells":[]}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadTerminalModel(path); err == nil {
		t.Fatal("ReadTerminalModel accepted a mismatched cell count, want error")
	}
}

func TestGraphicsBackendsOmitTerminalModel(t *testing.T) {
	for _, backend := range []Backend{BackendKitty, BackendSixel} {
		t.Run(string(backend), func(t *testing.T) {
			b, err := NewBundle(graphicsMeta(backend))
			if err != nil {
				t.Fatalf("NewBundle: %v", err)
			}
			in := blockFrameInput()
			in.Grid = nil // a graphics backend composes no cell grid
			if werr := b.WriteFrame(in); werr != nil {
				t.Fatalf("WriteFrame: %v", werr)
			}
			if ferr := b.FinaliseFull(); ferr != nil {
				t.Fatalf("FinaliseFull: %v", ferr)
			}
			per := readDirNames(t, filepath.Join(b.Path(), "frames", "frame-000001"))
			if per["terminal-model.json"] {
				t.Errorf("%s capture must omit terminal-model.json", backend)
			}
			if per["block-render.png"] {
				t.Errorf("%s capture must omit block-render.png", backend)
			}
			// screenshot, cdp-payload, text-map, text still present.
			for _, name := range []string{"screenshot.png", "cdp-payload.json", "text-map.json", "text.txt"} {
				if !per[name] {
					t.Errorf("%s frame missing %s", backend, name)
				}
			}
			// Manifest records the backend and the graphics note.
			raw, err := os.ReadFile(filepath.Join(b.Path(), "manifest.json"))
			if err != nil {
				t.Fatalf("read manifest.json: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal manifest.json: %v", err)
			}
			if m["backend"] != string(backend) {
				t.Errorf("manifest backend = %v, want %s", m["backend"], backend)
			}
			note, _ := m["note"].(string)
			if note == "" || !strings.Contains(note, "no cell grid") {
				t.Errorf("manifest note = %q, want a graphics no-cell-grid note", note)
			}
		})
	}
}

func TestPartialBundleRecordsFailureReason(t *testing.T) {
	b, err := NewBundle(blockMeta(3))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if werr := b.WriteFrame(blockFrameInput()); werr != nil {
		t.Fatalf("WriteFrame: %v", werr)
	}
	const reason = "first-draw timeout"
	if ferr := b.FinalisePartial(reason); ferr != nil {
		t.Fatalf("FinalisePartial: %v", ferr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if m["partial"] != true {
		t.Errorf("manifest partial = %v, want true", m["partial"])
	}
	if m["failure_reason"] != reason {
		t.Errorf("manifest failure_reason = %v, want %q", m["failure_reason"], reason)
	}
	if m["navigation_result"] != reason {
		t.Errorf("manifest navigation_result = %v, want %q", m["navigation_result"], reason)
	}
	if got := m["frames_captured"]; got != float64(1) {
		t.Errorf("manifest frames_captured = %v, want 1", got)
	}
}

func TestManifestFullBundle(t *testing.T) {
	b, err := NewBundle(blockMeta(2))
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	for range 2 {
		if werr := b.WriteFrame(blockFrameInput()); werr != nil {
			t.Fatalf("WriteFrame: %v", werr)
		}
	}
	if ferr := b.FinaliseFull(); ferr != nil {
		t.Fatalf("FinaliseFull: %v", ferr)
	}
	raw, err := os.ReadFile(filepath.Join(b.Path(), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if m["backend"] != "block" {
		t.Errorf("manifest backend = %v, want block", m["backend"])
	}
	if m["navigation_result"] != "ok" {
		t.Errorf("manifest navigation_result = %v, want ok", m["navigation_result"])
	}
	if m["frames_requested"] != float64(2) || m["frames_captured"] != float64(2) {
		t.Errorf("manifest counts = req %v cap %v, want 2/2", m["frames_requested"], m["frames_captured"])
	}
	if m["report"] != "report.md" {
		t.Errorf("manifest report = %v, want report.md", m["report"])
	}
	if _, ok := m["note"]; ok {
		t.Errorf("block manifest must carry no graphics note, got %v", m["note"])
	}
}
