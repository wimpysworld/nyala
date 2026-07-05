// Command paint-displacement reads a capture bundle frame directory (its
// screenshot.png plus text-map.json, and terminal-model.json for the grid) and
// scores paint-referenced glyph displacement: how far each run's DOM-snapped cell
// sits from the run's own painted ink in the SOURCE page paint. It reads
// screenshot.png, the page's own painted text forced to monospace with no overlay,
// NOT block-render.png, which folds our crisp overlay onto the paint so the overlay
// glyph would sit at Col by construction and the measurement would cancel. Because
// Col comes from the DOM box and the ink comes from the page's paint, a wrong cell
// width, a wrong scale, a bad text-to-bitmap pairing, or a scroll skew moves one and
// not the other, so the score cannot self-cancel the way a box-against-itself score
// does. It reuses the metrics PaintDisplacement function, so the score matches the
// measured path. Run it via `just paint-displacement <path>`.
//
// The argument is a frame directory that holds screenshot.png and text-map.json, or a
// text-map.json file whose directory holds the screenshot. The device scale from CSS
// to the screenshot is cell_px_w/run.CellW, isotropic; cell_px reads from the bundle's
// performance.json (falling back to 8x16), and each run carries its own CellW so the
// scale is exact per run. It prints the per-run displacement of each measured run and
// the frame aggregates: the measured and skipped run counts, the skipped fraction, and
// mean and max absolute displacement in cells on each axis.
//
// With -gate the tool exits non-zero when an aggregate breaches its threshold: mean
// absolute horizontal displacement at least -mean-h cells, max at least -max-h, mean
// vertical at least -mean-v, or max vertical at least -max-v. Without -gate it only
// prints. The thresholds default to the correctness target: mean-abs below 0.5 cell,
// max-abs below 1.0 cell, both axes.
//
// With -thresholds <fixture.json> the gate defaults come from the file's optional
// paint_displace_gate object (mean_abs_h, max_abs_h, mean_abs_v, max_abs_v); absent
// object keys fall back to the flag values. A missing or unreadable file named by the
// flag is an error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// mapName is the per-frame text-map.json filename the bundle writes.
const mapName = "text-map.json"

// shotName is the per-frame screenshot.png filename, the source page paint the metric
// reads.
const shotName = "screenshot.png"

// perfName is the bundle-root performance.json filename the cell size reads from.
const perfName = "performance.json"

// fallbackCellW and fallbackCellH are the cell size in device pixels used when the
// bundle's performance.json is absent; headless capture is deterministic at 8x16.
const (
	fallbackCellW = 8
	fallbackCellH = 16
)

// entry mirrors one text-map.json record: the metric reads Col, Row, Text, FG, X, Y,
// W, H, and CellW. A local struct keeps the tool free of the render seam, which the
// capture writer owns.
type entry struct {
	Col  int     `json:"Col"`
	Row  int     `json:"Row"`
	Text string  `json:"Text"`
	FG   fgView  `json:"FG"`
	X    float64 `json:"X"`
	Y    float64 `json:"Y"`
	W    float64 `json:"W"`
	H    float64 `json:"H"`

	CellW float64 `json:"CellW"`
}

// fgView mirrors a text-map.json run's FG object: the 0..255 RGBA channels. The metric
// reads only R, G, B.
type fgView struct {
	R uint8 `json:"R"`
	G uint8 `json:"G"`
	B uint8 `json:"B"`
	A uint8 `json:"A"`
}

// perfDoc mirrors the fields of performance.json the tool reads: the first frame's
// cell_px in device pixels. The rest is decoded but unused.
type perfDoc struct {
	Frames []struct {
		CellPx [2]int `json:"cell_px"`
	} `json:"frames"`
}

func main() {
	gate := flag.Bool("gate", false, "exit non-zero when a threshold is breached")
	meanH := flag.Float64("mean-h", 0.5, "gate: fail when mean absolute horizontal displacement is at least this many cells")
	maxH := flag.Float64("max-h", 1.0, "gate: fail when max absolute horizontal displacement is at least this many cells")
	meanV := flag.Float64("mean-v", 0.5, "gate: fail when mean absolute vertical displacement is at least this many cells")
	maxV := flag.Float64("max-v", 1.0, "gate: fail when max absolute vertical displacement is at least this many cells")
	thresholds := flag.String("thresholds", "", "gate: fixture.json whose paint_displace_gate object overrides the flag defaults")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: paint-displacement [flags] <frame-dir | text-map.json>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	gc := gateConfig{on: *gate, meanH: *meanH, maxH: *maxH, meanV: *meanV, maxV: *maxV}
	if *thresholds != "" {
		if err := applyThresholds(&gc, *thresholds); err != nil {
			fmt.Fprintf(os.Stderr, "paint-displacement: %v\n", err)
			os.Exit(1)
		}
	}
	if err := run(flag.Arg(0), gc); err != nil {
		fmt.Fprintf(os.Stderr, "paint-displacement: %v\n", err)
		os.Exit(1)
	}
}

// gateConfig holds the gate mode switch and its per-axis thresholds.
type gateConfig struct {
	on    bool
	meanH float64
	maxH  float64
	meanV float64
	maxV  float64
}

// gateDoc mirrors the paint_displace_gate object in a fixture.json: the per-fixture
// gate override. A pointer per field distinguishes an absent key (keep the flag
// default) from a present zero value.
type gateDoc struct {
	PaintDisplaceGate *struct {
		MeanAbsH *float64 `json:"mean_abs_h"`
		MaxAbsH  *float64 `json:"max_abs_h"`
		MeanAbsV *float64 `json:"mean_abs_v"`
		MaxAbsV  *float64 `json:"max_abs_v"`
	} `json:"paint_displace_gate"`
}

// applyThresholds reads the fixture.json at path and overrides gc's thresholds from
// its paint_displace_gate object. A present field overrides the flag default; an
// absent field keeps it. A missing or unreadable file, or invalid JSON, is an error. A
// readable file with no paint_displace_gate object leaves gc unchanged.
func applyThresholds(gc *gateConfig, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc gateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if doc.PaintDisplaceGate == nil {
		return nil
	}
	if doc.PaintDisplaceGate.MeanAbsH != nil {
		gc.meanH = *doc.PaintDisplaceGate.MeanAbsH
	}
	if doc.PaintDisplaceGate.MaxAbsH != nil {
		gc.maxH = *doc.PaintDisplaceGate.MaxAbsH
	}
	if doc.PaintDisplaceGate.MeanAbsV != nil {
		gc.meanV = *doc.PaintDisplaceGate.MeanAbsV
	}
	if doc.PaintDisplaceGate.MaxAbsV != nil {
		gc.maxV = *doc.PaintDisplaceGate.MaxAbsV
	}
	return nil
}

// run resolves arg to a frame directory, decodes the runs, reads the source paint
// screenshot.png and the cell size, scores the runs with metrics.PaintDisplacement,
// and prints the per-run and aggregate displacement. In gate mode it returns an error
// when an aggregate breaches its threshold. It fails if a needed file is missing or
// not valid JSON, or the screenshot cannot be decoded.
func run(arg string, gc gateConfig) error {
	frameDir, err := resolveFrameDir(arg)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(frameDir, mapName))
	if err != nil {
		return err
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("decode %s: %w", mapName, err)
	}
	img, err := readShot(filepath.Join(frameDir, shotName))
	if err != nil {
		return err
	}
	cellW, cellH := readCellPx(frameDir)
	runs := make([]metrics.PaintRun, len(entries))
	for i, e := range entries {
		runs[i] = metrics.PaintRun{
			Text:  e.Text,
			Col:   e.Col,
			Row:   e.Row,
			FG:    metrics.PaintColour{R: e.FG.R, G: e.FG.G, B: e.FG.B},
			X:     e.X,
			Y:     e.Y,
			W:     e.W,
			H:     e.H,
			CellW: e.CellW,
		}
	}
	s := metrics.PaintDisplacement(img, runs, cellW, cellH)
	printStats(s, cellW, cellH)
	if gc.on {
		return checkGate(s, gc)
	}
	return nil
}

// printStats prints the per-run and aggregate displacement.
func printStats(s metrics.PaintDisplaceStats, cellW, cellH int) {
	fmt.Printf("cell px:              %dx%d\n", cellW, cellH)
	fmt.Printf("measured runs:        %d\n", s.Measured)
	fmt.Printf("skipped runs:         %d\n", s.Skipped)
	fmt.Printf("skipped fraction:     %.3f\n", s.SkippedFraction)
	fmt.Printf("mean abs h disp:      %.3f cells\n", s.MeanAbsH)
	fmt.Printf("max abs h disp:       %.3f cells\n", s.MaxAbsH)
	fmt.Printf("mean abs v disp:      %.3f cells\n", s.MeanAbsV)
	fmt.Printf("max abs v disp:       %.3f cells\n", s.MaxAbsV)
}

// checkGate returns an error naming the first threshold the stats breach, or nil when
// all pass.
func checkGate(s metrics.PaintDisplaceStats, gc gateConfig) error {
	if s.MeanAbsH >= gc.meanH {
		return fmt.Errorf("gate: mean abs h disp %.3f >= %.3f cells", s.MeanAbsH, gc.meanH)
	}
	if s.MaxAbsH >= gc.maxH {
		return fmt.Errorf("gate: max abs h disp %.3f >= %.3f cells", s.MaxAbsH, gc.maxH)
	}
	if s.MeanAbsV >= gc.meanV {
		return fmt.Errorf("gate: mean abs v disp %.3f >= %.3f cells", s.MeanAbsV, gc.meanV)
	}
	if s.MaxAbsV >= gc.maxV {
		return fmt.Errorf("gate: max abs v disp %.3f >= %.3f cells", s.MaxAbsV, gc.maxV)
	}
	return nil
}

// readShot decodes screenshot.png at path into an *image.RGBA in RGBA byte order.
// A non-RGBA decode is copied into an RGBA image so the metric reads a known layout.
func readShot(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", shotName, err)
	}
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba, nil
	}
	b := img.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out, nil
}

// readCellPx reads cell_px from the bundle-root performance.json, resolved from the
// frame directory (its parent's parent, since frames live in frames/frame-*/). It
// falls back to 8x16 when the file is absent, unreadable, carries no frame record, or
// reports a non-positive cell size.
func readCellPx(frameDir string) (int, int) {
	bundleRoot := filepath.Dir(filepath.Dir(frameDir))
	raw, err := os.ReadFile(filepath.Join(bundleRoot, perfName))
	if err != nil {
		return fallbackCellW, fallbackCellH
	}
	var doc perfDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fallbackCellW, fallbackCellH
	}
	if len(doc.Frames) == 0 {
		return fallbackCellW, fallbackCellH
	}
	w, h := doc.Frames[0].CellPx[0], doc.Frames[0].CellPx[1]
	if w <= 0 || h <= 0 {
		return fallbackCellW, fallbackCellH
	}
	return w, h
}

// resolveFrameDir maps arg to a frame directory: a directory is used as is, a
// text-map.json file resolves to its directory. It fails when the path does not exist
// or a directory holds no text-map.json.
func resolveFrameDir(arg string) (string, error) {
	info, err := os.Stat(arg)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		if _, serr := os.Stat(filepath.Join(arg, mapName)); serr != nil {
			return "", fmt.Errorf("no %s in %s", mapName, arg)
		}
		return arg, nil
	}
	return filepath.Dir(arg), nil
}
