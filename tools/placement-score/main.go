// Command placement-score reads a per-frame text-map.json (the text runs a
// NYALA_CAPTURE bundle writes per frame, with on-screen box geometry) and scores
// horizontal text placement error against the terminal cell. It quantifies how
// far the block renderer's one-grapheme-per-cell layout drifts a run's right edge
// from the box's true right edge, so the owner can measure block-render accuracy
// across the corpus and show before/after numbers. It reuses the metrics
// Placement function, so the score matches the measured path. Run it via
// `just placement-score <path>`.
//
// The argument is either a text-map.json file or a frame directory that contains
// one; the tool resolves the directory case to the file. The cell size reads from
// the bundle's performance.json cell_px, resolved from the frame directory, and
// falls back to 8x16 when absent. The grid size reads from the frame's
// terminal-model.json; a graphics-backend bundle omits it, so the off-grid check
// is skipped. The off-grid count is horizontal and negative escapes only (Col < 0,
// Row < 0, Col >= cols); a row past the grid is legitimate below-fold content on a
// document-absolute text map, so it is not counted. It prints the frame
// aggregates: mean and max right-edge error in cells, mean width error in cells,
// mean row error in cells, the drifted-box count, the same-row collision count,
// the off-grid box count, and the total box count.
//
// With -gate the tool exits non-zero when an aggregate breaches its threshold:
// mean right-edge error at least -mean cells, max right-edge error at least -max
// cells, drifted-box count above -drifted, collision count above -collisions, or
// off-grid box count above -offgrid. Without -gate it only prints.
//
// With -thresholds <fixture.json> the gate defaults come from the file's optional
// placement_gate object; absent object keys fall back to the -mean, -max,
// -drifted, -collisions, and -offgrid flag values. A missing or unreadable file
// named by the flag is an error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// mapName is the per-frame text-map.json filename the bundle writes.
const mapName = "text-map.json"

// perfName is the bundle-root performance.json filename the cell size reads from.
const perfName = "performance.json"

// modelName is the per-frame terminal-model.json filename the grid size reads
// from. A graphics-backend bundle omits it.
const modelName = "terminal-model.json"

// fallbackCellW and fallbackCellH are the cell size in CSS pixels used when the
// bundle's performance.json is absent; headless capture is deterministic at 8x16.
const (
	fallbackCellW = 8
	fallbackCellH = 16
)

// entry mirrors one text-map.json record: the metric reads Col, Row, Text, X, Y,
// W, and H, and the rest is decoded but unused. A local struct keeps the tool
// free of the render seam, which the capture writer owns.
type entry struct {
	Col  int     `json:"Col"`
	Row  int     `json:"Row"`
	Text string  `json:"Text"`
	X    float64 `json:"X"`
	Y    float64 `json:"Y"`
	W    float64 `json:"W"`
	H    float64 `json:"H"`
}

// perfDoc mirrors the fields of performance.json the tool reads: the first
// frame's cell_px. The rest of the document is decoded but unused.
type perfDoc struct {
	Frames []struct {
		CellPx [2]int `json:"cell_px"`
	} `json:"frames"`
}

// gridDoc mirrors the grid size in a frame's terminal-model.json. The rest of
// the document is decoded but unused. A local struct keeps the tool free of the
// capture seam, which owns the model.
type gridDoc struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func main() {
	gate := flag.Bool("gate", false, "exit non-zero when a threshold is breached")
	meanMax := flag.Float64("mean", 0.5, "gate: fail when mean right-edge error is at least this many cells")
	maxMax := flag.Float64("max", 1.0, "gate: fail when max right-edge error is at least this many cells")
	driftedMax := flag.Int("drifted", 0, "gate: fail when drifted-box count is above this")
	collisionsMax := flag.Int("collisions", 0, "gate: fail when same-row cell-overlap collision count is above this")
	offgridMax := flag.Int("offgrid", 0, "gate: fail when off-grid box count is above this")
	thresholds := flag.String("thresholds", "", "gate: fixture.json whose placement_gate object overrides the flag defaults")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: placement-score [flags] <text-map.json | frame-dir>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	gc := gateConfig{on: *gate, mean: *meanMax, max: *maxMax, drifted: *driftedMax, collisions: *collisionsMax, offgrid: *offgridMax}
	if *thresholds != "" {
		if err := applyThresholds(&gc, *thresholds); err != nil {
			fmt.Fprintf(os.Stderr, "placement-score: %v\n", err)
			os.Exit(1)
		}
	}
	if err := run(flag.Arg(0), gc); err != nil {
		fmt.Fprintf(os.Stderr, "placement-score: %v\n", err)
		os.Exit(1)
	}
}

// gateDoc mirrors the placement_gate object in a fixture.json: the per-fixture
// gate override. A pointer per field distinguishes an absent key (keep the flag
// default) from a present zero value.
type gateDoc struct {
	PlacementGate *struct {
		Mean       *float64 `json:"mean"`
		Max        *float64 `json:"max"`
		Drifted    *int     `json:"drifted"`
		Collisions *int     `json:"collisions"`
		Offgrid    *int     `json:"offgrid"`
	} `json:"placement_gate"`
}

// applyThresholds reads the fixture.json at path and overrides gc's thresholds
// from its placement_gate object. A present field overrides the flag default; an
// absent field keeps it. A missing or unreadable file, or invalid JSON, is an
// error. A readable file with no placement_gate object leaves gc unchanged.
func applyThresholds(gc *gateConfig, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc gateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if doc.PlacementGate == nil {
		return nil
	}
	if doc.PlacementGate.Mean != nil {
		gc.mean = *doc.PlacementGate.Mean
	}
	if doc.PlacementGate.Max != nil {
		gc.max = *doc.PlacementGate.Max
	}
	if doc.PlacementGate.Drifted != nil {
		gc.drifted = *doc.PlacementGate.Drifted
	}
	if doc.PlacementGate.Collisions != nil {
		gc.collisions = *doc.PlacementGate.Collisions
	}
	if doc.PlacementGate.Offgrid != nil {
		gc.offgrid = *doc.PlacementGate.Offgrid
	}
	return nil
}

// gateConfig holds the gate mode switch and its thresholds.
type gateConfig struct {
	on         bool
	mean       float64
	max        float64
	drifted    int
	collisions int
	offgrid    int
}

// run resolves arg to a text-map.json file, decodes the boxes, reads the cell
// size from the bundle performance.json and the grid size from the frame's
// terminal-model.json, scores the boxes with metrics.Placement, and prints the
// frame aggregates. In gate mode it returns an error when an aggregate breaches
// its threshold. It fails if the file is missing or not valid JSON.
func run(arg string, gc gateConfig) error {
	mapPath, err := resolveMap(arg)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(mapPath)
	if err != nil {
		return err
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("decode %s: %w", mapName, err)
	}
	frameDir := filepath.Dir(mapPath)
	cellW, cellH := readCellPx(frameDir)
	cols, rows := readGrid(frameDir)
	boxes := make([]metrics.PlacementBox, len(entries))
	for i, e := range entries {
		boxes[i] = metrics.PlacementBox{X: e.X, Y: e.Y, W: e.W, H: e.H, Col: e.Col, Row: e.Row, Text: e.Text}
	}
	s := metrics.Placement(boxes, cellW, cellH, cols, rows)
	fmt.Printf("cell px:              %dx%d\n", cellW, cellH)
	fmt.Printf("grid:                 %dx%d\n", cols, rows)
	fmt.Printf("boxes:                %d\n", len(boxes))
	fmt.Printf("mean right-edge err:  %.3f cells\n", s.MeanAbsRightEdgeErr)
	fmt.Printf("max right-edge err:   %.3f cells\n", s.MaxAbsRightEdgeErr)
	fmt.Printf("mean width err:       %.3f cells\n", s.MeanAbsWidthErr)
	fmt.Printf("mean row err:         %.3f cells\n", s.MeanAbsRowErr)
	fmt.Printf("drifted boxes (>=1):  %d\n", s.DriftedBoxes)
	fmt.Printf("collisions:           %d\n", s.Collisions)
	fmt.Printf("off-grid boxes:       %d\n", s.OffGrid)
	if gc.on {
		return checkGate(s, gc)
	}
	return nil
}

// checkGate returns an error naming every threshold the stats breach, or nil when
// all pass.
func checkGate(s metrics.PlacementStats, gc gateConfig) error {
	if s.MeanAbsRightEdgeErr >= gc.mean {
		return fmt.Errorf("gate: mean right-edge err %.3f >= %.3f cells", s.MeanAbsRightEdgeErr, gc.mean)
	}
	if s.MaxAbsRightEdgeErr >= gc.max {
		return fmt.Errorf("gate: max right-edge err %.3f >= %.3f cells", s.MaxAbsRightEdgeErr, gc.max)
	}
	if s.DriftedBoxes > gc.drifted {
		return fmt.Errorf("gate: drifted boxes %d > %d", s.DriftedBoxes, gc.drifted)
	}
	if s.Collisions > gc.collisions {
		return fmt.Errorf("gate: collisions %d > %d", s.Collisions, gc.collisions)
	}
	if s.OffGrid > gc.offgrid {
		return fmt.Errorf("gate: off-grid boxes %d > %d", s.OffGrid, gc.offgrid)
	}
	return nil
}

// readCellPx reads cell_px from the bundle-root performance.json, resolved from
// the frame directory (its parent's parent, since frames live in frames/frame-*/).
// It falls back to 8x16 when the file is absent, unreadable, or carries no frame
// record. Only a present, positive pair overrides the fallback.
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

// readGrid reads cols and rows from the frame's terminal-model.json. It returns
// 0, 0 when the file is absent (a graphics-backend bundle has no cell grid),
// unreadable, or not valid JSON, so metrics.Placement skips the off-grid check
// and the tool behaviour degrades to the pre-grid path.
func readGrid(frameDir string) (int, int) {
	raw, err := os.ReadFile(filepath.Join(frameDir, modelName))
	if err != nil {
		return 0, 0
	}
	var doc gridDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, 0
	}
	return doc.Cols, doc.Rows
}

// resolveMap maps arg to a text-map.json path: a directory yields the map inside
// it, a file is used as is. It fails when the path does not exist or a directory
// holds no text-map.json.
func resolveMap(arg string) (string, error) {
	info, err := os.Stat(arg)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		mapPath := filepath.Join(arg, mapName)
		if _, serr := os.Stat(mapPath); serr != nil {
			return "", fmt.Errorf("no %s in %s", mapName, arg)
		}
		return mapPath, nil
	}
	return arg, nil
}
