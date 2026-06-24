package capture

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// Backend names the renderer a capture ran. It is the string the manifest and
// performance records carry, and it gates the cell-grid artefacts: only "block"
// writes terminal-model.json, because the graphics backends produce no cell
// grid.
type Backend string

const (
	// BackendBlock is the owned-grid block-cell renderer. A block capture writes
	// terminal-model.json from the composed grid.
	BackendBlock Backend = "block"
	// BackendKitty is the Kitty graphics backend. It produces no cell grid, so a
	// kitty capture omits terminal-model.json.
	BackendKitty Backend = "kitty"
	// BackendSixel is the Sixel graphics backend. It produces no cell grid, so a
	// sixel capture omits terminal-model.json.
	BackendSixel Backend = "sixel"
)

// graphicsNote is the manifest note recorded for a graphics backend: it produces
// no cell grid, so the bundle omits terminal-model.json.
const graphicsNote = "graphics backend: no cell grid, terminal-model.json omitted"

// Meta is the immutable run metadata a Bundle records into manifest.json and
// reuses across the per-frame and finalise writes. The driver fills it once
// before the loop. Command and FinalURL describe the invocation; Backend and
// TerminalCapability describe the resolved render path; FramesRequested is the
// requested frame count n.
type Meta struct {
	// Command is the command line that launched the capture, secrets omitted.
	Command string
	// Backend is the resolved render backend the capture drew with.
	Backend Backend
	// TerminalCapability is a short description of the detected terminal
	// capability, for example "block" or "kitty".
	TerminalCapability string
	// FramesRequested is the requested frame count n.
	FramesRequested int
	// NavigationURL is the URL the capture navigated to.
	NavigationURL string
	// FinalURL is the resolved final URL after navigation, if known.
	FinalURL string
}

// manifest is the JSON view written to manifest.json. It records the command
// metadata, the chosen backend, the terminal capability, the requested and
// captured frame counts, the navigation result, a pointer to report.md, and the
// graphics-backend note when the backend produces no cell grid.
type manifest struct {
	// Command is the launching command line, secrets omitted.
	Command string `json:"command"`
	// Backend is the resolved render backend.
	Backend string `json:"backend"`
	// TerminalCapability is the detected terminal capability description.
	TerminalCapability string `json:"terminal_capability"`
	// FramesRequested is the requested frame count n.
	FramesRequested int `json:"frames_requested"`
	// FramesCaptured is the number of frames the run actually drew.
	FramesCaptured int `json:"frames_captured"`
	// NavigationURL is the URL the capture navigated to.
	NavigationURL string `json:"navigation_url"`
	// FinalURL is the resolved final URL, if known.
	FinalURL string `json:"final_url,omitempty"`
	// NavigationResult is the navigation outcome: "ok" for a full bundle, or the
	// failure reason for a partial bundle.
	NavigationResult string `json:"navigation_result"`
	// Partial is true when the bundle finalised partial.
	Partial bool `json:"partial"`
	// FailureReason is the partial-bundle failure reason; empty for a full bundle.
	FailureReason string `json:"failure_reason,omitempty"`
	// Report points at the report.md file inside the bundle.
	Report string `json:"report"`
	// Note records a backend caveat, for example that a graphics backend produces
	// no cell grid; empty when there is none.
	Note string `json:"note,omitempty"`
}

// reportPlaceholder is the stub report.md body written at bundle creation. Phase
// 6 replaces it with the real report; the placeholder keeps the layout complete
// so the bundle is well formed before that wiring lands.
const reportPlaceholder = "# Nyala capture report\n\nPlaceholder. Populated by report.md generation.\n"

// Bundle is one on-disk capture directory: a nyala-capture-* temp dir holding
// the root manifest, report, performance record, terminal-stream tee, CDP files,
// and per-frame artefacts. It holds the run metadata, the open raw-file handle,
// the per-frame counter, and the accumulating performance records. Construct it
// with NewBundle; the zero value is not usable. It is not safe for concurrent
// use: the capture driver calls it from one goroutine.
type Bundle struct {
	dir        string
	meta       Meta
	raw        *os.File
	events     *os.File
	frames     int
	perf       []FramePerformance
	captured   int
	rasteriser *Rasteriser
}

// NewBundle creates a fresh nyala-capture-* temp directory, writes the root
// report.md placeholder, opens terminal-stream.raw for the tee, and creates the
// root cdp-session.json and cdp-events.ndjson placeholders so their root
// placement is part of the layout from the start; their content lands via the
// engine recorder. It records meta for the manifest written at finalise. It
// fails if the directory or any root file cannot be created.
func NewBundle(meta Meta) (*Bundle, error) {
	dir, err := os.MkdirTemp("", "nyala-capture-*")
	if err != nil {
		return nil, fmt.Errorf("create bundle dir: %w", err)
	}
	if werr := os.WriteFile(filepath.Join(dir, "report.md"), []byte(reportPlaceholder), 0o644); werr != nil {
		return nil, fmt.Errorf("write report placeholder: %w", werr)
	}
	// cdp-session.json and cdp-events.ndjson sit at the bundle root, not under a
	// cdp/ directory. Create cdp-session.json empty here so the root layout is
	// complete; WriteSession fills it from the engine. cdp-events.ndjson opens for
	// the recorder to append event lines through CDPEventsFile.
	if cerr := os.WriteFile(filepath.Join(dir, "cdp-session.json"), nil, 0o644); cerr != nil {
		return nil, fmt.Errorf("create cdp-session.json: %w", cerr)
	}
	events, err := os.Create(filepath.Join(dir, "cdp-events.ndjson"))
	if err != nil {
		return nil, fmt.Errorf("create cdp-events.ndjson: %w", err)
	}
	raw, err := os.Create(filepath.Join(dir, "terminal-stream.raw"))
	if err != nil {
		_ = events.Close()
		return nil, fmt.Errorf("create terminal-stream.raw: %w", err)
	}
	return &Bundle{dir: dir, meta: meta, raw: raw, events: events}, nil
}

// Path returns the bundle directory path. The driver prints it on every exit.
func (b *Bundle) Path() string { return b.dir }

// RawFile returns the open terminal-stream.raw handle. The per-frame tee mirrors
// the drawn terminal bytes into it. It is exposed as io.Writer so the driver
// wraps it in an io.MultiWriter alongside the tty.
func (b *Bundle) RawFile() io.Writer { return b.raw }

// CDPEventsFile returns the open cdp-events.ndjson handle the engine recorder
// appends whitelisted event lines to. It is exposed as io.Writer so the driver
// hands it to the engine before Navigate; the engine owns the writes. Finalise
// closes it.
func (b *Bundle) CDPEventsFile() io.Writer { return b.events }

// WriteSession writes the cdp-session.json bytes the engine assembled during
// navigation into the bundle root, overwriting the empty placeholder. The driver
// calls it once after Navigate returns, before finalise. Empty data leaves the
// placeholder untouched, so a navigate that filled no session does not blank the
// file.
func (b *Bundle) WriteSession(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(b.dir, "cdp-session.json"), data, 0o644); err != nil {
		return fmt.Errorf("write cdp-session.json: %w", err)
	}
	return nil
}

// FrameInput carries everything WriteFrame serialises for one drawn frame. The
// driver fills it inline: Frame carries the bitmap, the text overlay, and the
// engine-phase Stats; Meta is the screencast frame metadata, recorded without
// the image bytes; Grid is the composed letterbox grid for a block capture and
// nil for a graphics backend; Perf is the viewer-phase timing and counts the
// driver measured.
type FrameInput struct {
	// Frame is the drawn render.Frame: the bitmap, the text overlay, and the
	// engine-phase Stats.
	Frame render.Frame
	// Meta is the screencast frame metadata. WriteFrame serialises it without the
	// image bytes. It may be nil.
	Meta *render.ScreencastMeta
	// Grid is the composed letterbox grid for a block capture, the same grid the
	// viewer drew. It is nil for a graphics backend, which writes no cell grid.
	Grid *block.Grid
	// Perf is the viewer-phase timing and counts for this frame, filled by the
	// driver. WriteFrame folds it with the engine-phase Stats into the
	// performance record.
	Perf ViewerPerf
}

// WriteFrame records one drawn frame's artefacts into frames/frame-NNNNNN/:
// screenshot.png from the frame bitmap, cdp-payload.json from the screencast
// metadata (no image bytes), text-map.json from the frame text runs, text.txt
// from the grid flattened one line per row, and, for a block capture, an explicit
// terminal-model.json view of the composed grid plus block-render.png, the grid
// rasterised at the pinned 8x16 cell. It also accumulates this frame's performance
// record. The frame index is one-based and zero-padded to six digits.
func (b *Bundle) WriteFrame(in FrameInput) error {
	b.frames++
	b.captured = b.frames
	frameDir := filepath.Join(b.dir, "frames", fmt.Sprintf("frame-%06d", b.frames))
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return fmt.Errorf("create frame dir: %w", err)
	}
	if err := b.writeScreenshot(frameDir, in.Frame); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(frameDir, "cdp-payload.json"), cdpPayloadView(in.Meta)); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(frameDir, "text-map.json"), in.Frame.Text); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(frameDir, "text.txt"), []byte(flattenText(in.Grid, in.Frame.Text)), 0o644); err != nil {
		return fmt.Errorf("write text.txt: %w", err)
	}
	if in.Grid != nil {
		if err := writeJSON(filepath.Join(frameDir, "terminal-model.json"), gridView(in.Grid)); err != nil {
			return err
		}
		if err := b.writeBlockRender(frameDir, *in.Grid); err != nil {
			return err
		}
	}
	b.perf = append(b.perf, performanceRecord(b.meta.Backend, in))
	return nil
}

// writeBlockRender rasterises grid to block-render.png in frameDir, the composed
// block grid drawn at the pinned 8x16 cell. It builds the bundle Rasteriser once
// and reuses it across frames, because the rasteriser loads a font. A graphics
// backend never reaches here, since WriteFrame gates this call on a non-nil grid.
func (b *Bundle) writeBlockRender(frameDir string, grid block.Grid) error {
	if b.rasteriser == nil {
		r, err := NewRasteriser()
		if err != nil {
			return fmt.Errorf("build rasteriser: %w", err)
		}
		b.rasteriser = r
	}
	img := b.rasteriser.Rasterise(grid)
	f, err := os.Create(filepath.Join(frameDir, "block-render.png"))
	if err != nil {
		return fmt.Errorf("create block-render.png: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := EncodePNG(f, img); err != nil {
		return fmt.Errorf("encode block-render.png: %w", err)
	}
	return nil
}

// writeScreenshot encodes the frame bitmap to screenshot.png via EncodePNG. A nil
// bitmap is an error: a drawn frame always carries an image.
func (b *Bundle) writeScreenshot(frameDir string, frame render.Frame) error {
	if frame.Img == nil {
		return fmt.Errorf("write screenshot: frame has no image")
	}
	f, err := os.Create(filepath.Join(frameDir, "screenshot.png"))
	if err != nil {
		return fmt.Errorf("create screenshot.png: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := EncodePNG(f, frame.Img); err != nil {
		return fmt.Errorf("encode screenshot.png: %w", err)
	}
	return nil
}

// FinaliseFull writes performance.json and the full-bundle manifest, then closes
// the raw-file handle. The driver calls it after n frames drew. The navigation
// result is "ok" and the bundle is not partial.
func (b *Bundle) FinaliseFull() error {
	return b.finalise(false, "")
}

// FinalisePartial writes performance.json and the partial-bundle manifest with
// the failure reason, then closes the raw-file handle. The driver calls it on a
// timeout, an early channel close, or a forced-backend rejection.
func (b *Bundle) FinalisePartial(reason string) error {
	return b.finalise(true, reason)
}

// finalise writes performance.json and manifest.json, then closes the raw-file
// handle. partial and reason distinguish a full from a partial bundle. The
// manifest records the captured count, the navigation result, and the
// graphics-backend note when the backend produces no cell grid.
func (b *Bundle) finalise(partial bool, reason string) error {
	if err := writeJSON(filepath.Join(b.dir, "performance.json"), b.performanceDoc()); err != nil {
		return err
	}
	result := "ok"
	if partial {
		result = reason
	}
	m := manifest{
		Command:            b.meta.Command,
		Backend:            string(b.meta.Backend),
		TerminalCapability: b.meta.TerminalCapability,
		FramesRequested:    b.meta.FramesRequested,
		FramesCaptured:     b.captured,
		NavigationURL:      b.meta.NavigationURL,
		FinalURL:           b.meta.FinalURL,
		NavigationResult:   result,
		Partial:            partial,
		FailureReason:      reason,
		Report:             "report.md",
	}
	if isGraphics(b.meta.Backend) {
		m.Note = graphicsNote
	}
	if err := writeJSON(filepath.Join(b.dir, "manifest.json"), m); err != nil {
		return err
	}
	if werr := os.WriteFile(filepath.Join(b.dir, "report.md"), renderReport(m), 0o644); werr != nil {
		return fmt.Errorf("write report.md: %w", werr)
	}
	if cerr := b.events.Close(); cerr != nil {
		return fmt.Errorf("close cdp-events.ndjson: %w", cerr)
	}
	return b.raw.Close()
}

// isGraphics reports whether backend is a graphics backend (kitty or sixel),
// which produces no cell grid.
func isGraphics(backend Backend) bool {
	return backend == BackendKitty || backend == BackendSixel
}

// cdpPayloadView is the JSON view written to cdp-payload.json: the screencast
// frame metadata only, never the frame image bytes or any base64 payload. A nil
// metadata yields an empty object. The keys are snake_case so the file reads
// cleanly.
func cdpPayloadView(meta *render.ScreencastMeta) any {
	if meta == nil {
		return struct{}{}
	}
	return cdpPayloadDoc{
		OffsetTop:       meta.OffsetTop,
		PageScaleFactor: meta.PageScaleFactor,
		DeviceWidth:     meta.DeviceWidth,
		DeviceHeight:    meta.DeviceHeight,
		ScrollOffsetX:   meta.ScrollOffsetX,
		ScrollOffsetY:   meta.ScrollOffsetY,
		Timestamp:       meta.Timestamp,
	}
}

// cdpPayloadDoc is the explicit JSON view written to cdp-payload.json: the
// screencast frame metadata fields under snake_case keys, with no image bytes.
type cdpPayloadDoc struct {
	OffsetTop       float64 `json:"offset_top"`
	PageScaleFactor float64 `json:"page_scale_factor"`
	DeviceWidth     float64 `json:"device_width"`
	DeviceHeight    float64 `json:"device_height"`
	ScrollOffsetX   float64 `json:"scroll_offset_x"`
	ScrollOffsetY   float64 `json:"scroll_offset_y"`
	Timestamp       float64 `json:"timestamp"`
}

// writeJSON marshals v as indented JSON and writes it to path. It fails if the
// marshal or the file write fails.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	if werr := os.WriteFile(path, data, 0o644); werr != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), werr)
	}
	return nil
}

// cellView is the explicit human-readable JSON view of one block.Cell. Glyph is a
// string, not a rune number; FG and BG are readable RGBA; Wide marks the trailing
// cell of a wide overlay glyph. Text is the overlay grapheme, empty for a bitmap
// cell.
type cellView struct {
	// Glyph is the bitmap block glyph as a string.
	Glyph string `json:"glyph"`
	// Text is the overlay grapheme, empty for a bitmap cell.
	Text string `json:"text,omitempty"`
	// FG is the foreground colour.
	FG rgbaView `json:"fg"`
	// BG is the background colour.
	BG rgbaView `json:"bg"`
	// Wide marks the trailing cell of a wide overlay glyph.
	Wide bool `json:"wide,omitempty"`
}

// rgbaView is a readable RGBA colour: four 0..255 channels, so the serialised
// colour is inspectable instead of a packed number.
type rgbaView struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
	A uint8 `json:"a"`
}

// gridDoc is the explicit JSON view of a composed block.Grid: the grid size and
// the per-cell views in row-major order. It makes terminal-model.json
// inspectable, since a raw block.Cell marshals the glyph as a rune number.
type gridDoc struct {
	// Cols is the grid width in cells.
	Cols int `json:"cols"`
	// Rows is the grid height in cells.
	Rows int `json:"rows"`
	// Cells is the per-cell view, row-major, len == Cols*Rows.
	Cells []cellView `json:"cells"`
}

// ReadTerminalModel reads a terminal-model.json file at path and reconstructs
// the full composed block.Grid the bundle wrote: every cell, not just the text
// cells, with the glyph, overlay text, foreground, background, and wide flag
// restored. It is the inverse of gridView, so the schema stays in one place. It
// fails if the file cannot be read, is not valid JSON, or the cell count does
// not match Cols*Rows (a malformed or non-block model).
func ReadTerminalModel(path string) (block.Grid, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return block.Grid{}, fmt.Errorf("read terminal-model.json: %w", err)
	}
	var doc gridDoc
	if uerr := json.Unmarshal(data, &doc); uerr != nil {
		return block.Grid{}, fmt.Errorf("unmarshal terminal-model.json: %w", uerr)
	}
	if doc.Cols <= 0 || doc.Rows <= 0 {
		return block.Grid{}, fmt.Errorf("terminal-model.json has empty grid: %dx%d", doc.Cols, doc.Rows)
	}
	if want := doc.Cols * doc.Rows; len(doc.Cells) != want {
		return block.Grid{}, fmt.Errorf("terminal-model.json cell count = %d, want %d for %dx%d", len(doc.Cells), want, doc.Cols, doc.Rows)
	}
	g := block.Grid{Cols: doc.Cols, Rows: doc.Rows, Cells: make([]block.Cell, len(doc.Cells))}
	for i, c := range doc.Cells {
		g.Cells[i] = block.Cell{
			Glyph: firstRune(c.Glyph),
			Text:  c.Text,
			FG:    color.RGBA{c.FG.R, c.FG.G, c.FG.B, c.FG.A},
			BG:    color.RGBA{c.BG.R, c.BG.G, c.BG.B, c.BG.A},
			Wide:  c.Wide,
		}
	}
	return g, nil
}

// firstRune returns the first rune of s, or 0 when s is empty. The glyph view is
// a one-rune string, so this restores the rune the bitmap cell carried.
func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// gridView builds the explicit JSON view of g: the size and a readable view of
// every cell, with the glyph as a string and the colours as readable RGBA.
func gridView(g *block.Grid) gridDoc {
	doc := gridDoc{Cols: g.Cols, Rows: g.Rows, Cells: make([]cellView, len(g.Cells))}
	for i, c := range g.Cells {
		doc.Cells[i] = cellView{
			Glyph: string(c.Glyph),
			Text:  c.Text,
			FG:    rgbaView{c.FG.R, c.FG.G, c.FG.B, c.FG.A},
			BG:    rgbaView{c.BG.R, c.BG.G, c.BG.B, c.BG.A},
			Wide:  c.Wide,
		}
	}
	return doc
}

// flattenText flattens the drawn overlay to one line per grid row. For a block
// capture it derives the row and column count from the composed letterbox grid,
// then places each GlyphRun at its column, leaving an uncovered cell blank, so
// the result mirrors what the viewer drew. With no grid (a graphics backend) it
// falls back to the runs' own extent. A wide grapheme occupies its lead column
// only here, matching one entry per run.
func flattenText(g *block.Grid, runs []render.GlyphRun) string {
	rows, cols := flattenExtent(g, runs)
	if rows <= 0 || cols <= 0 {
		return ""
	}
	lines := make([][]rune, rows)
	for r := range lines {
		lines[r] = make([]rune, cols)
		for c := range lines[r] {
			lines[r][c] = ' '
		}
	}
	for _, run := range runs {
		if run.Row < 0 || run.Row >= rows {
			continue
		}
		col := run.Col
		for _, ch := range run.Text {
			if col >= 0 && col < cols {
				lines[run.Row][col] = ch
			}
			col++
		}
	}
	var sb strings.Builder
	for r, line := range lines {
		if r > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(strings.TrimRight(string(line), " "))
	}
	sb.WriteByte('\n')
	return sb.String()
}

// flattenExtent returns the row and column count for the text flatten. It uses
// the composed grid size when present, so the flatten matches the letterboxed
// grid the viewer drew; with no grid it bounds the lines to the runs' own extent.
func flattenExtent(g *block.Grid, runs []render.GlyphRun) (rows, cols int) {
	if g != nil {
		return g.Rows, g.Cols
	}
	for _, run := range runs {
		if run.Row+1 > rows {
			rows = run.Row + 1
		}
		end := run.Col + len([]rune(run.Text))
		if end > cols {
			cols = end
		}
	}
	return rows, cols
}
