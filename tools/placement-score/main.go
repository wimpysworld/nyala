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
// falls back to 8x16 when absent. It prints the frame aggregates: mean and max
// right-edge error in cells, mean width error in cells, the drifted-box count,
// and the total box count.
//
// With -gate the tool exits non-zero when an aggregate breaches its threshold:
// mean right-edge error at least -mean cells, max right-edge error at least -max
// cells, or drifted-box count above -drifted. Without -gate it only prints.
//
// With -thresholds <fixture.json> the gate defaults come from the file's optional
// placement_gate object; absent object keys fall back to the -mean, -max, and
// -drifted flag values. A missing or unreadable file named by the flag is an error.
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

// fallbackCellW and fallbackCellH are the cell size in CSS pixels used when the
// bundle's performance.json is absent; headless capture is deterministic at 8x16.
const (
	fallbackCellW = 8
	fallbackCellH = 16
)

// entry mirrors one text-map.json record: the metric reads X, W, Col, and Text,
// and the rest is decoded but unused. A local struct keeps the tool free of the
// render seam, which the capture writer owns.
type entry struct {
	Col  int     `json:"Col"`
	Text string  `json:"Text"`
	X    float64 `json:"X"`
	W    float64 `json:"W"`
}

// perfDoc mirrors the fields of performance.json the tool reads: the first
// frame's cell_px. The rest of the document is decoded but unused.
type perfDoc struct {
	Frames []struct {
		CellPx [2]int `json:"cell_px"`
	} `json:"frames"`
}

func main() {
	gate := flag.Bool("gate", false, "exit non-zero when a threshold is breached")
	meanMax := flag.Float64("mean", 0.5, "gate: fail when mean right-edge error is at least this many cells")
	maxMax := flag.Float64("max", 1.0, "gate: fail when max right-edge error is at least this many cells")
	driftedMax := flag.Int("drifted", 0, "gate: fail when drifted-box count is above this")
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
	gc := gateConfig{on: *gate, mean: *meanMax, max: *maxMax, drifted: *driftedMax}
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
		Mean    *float64 `json:"mean"`
		Max     *float64 `json:"max"`
		Drifted *int     `json:"drifted"`
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
	return nil
}

// gateConfig holds the gate mode switch and its thresholds.
type gateConfig struct {
	on      bool
	mean    float64
	max     float64
	drifted int
}

// run resolves arg to a text-map.json file, decodes the boxes, reads the cell
// size from the sibling performance.json, scores the boxes with metrics.Placement,
// and prints the frame aggregates. In gate mode it returns an error when an
// aggregate breaches its threshold. It fails if the file is missing or not valid
// JSON.
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
	cellW, cellH := readCellPx(filepath.Dir(mapPath))
	boxes := make([]metrics.PlacementBox, len(entries))
	for i, e := range entries {
		boxes[i] = metrics.PlacementBox{X: e.X, W: e.W, Col: e.Col, Text: e.Text}
	}
	s := metrics.Placement(boxes, cellW, cellH)
	fmt.Printf("cell px:              %dx%d\n", cellW, cellH)
	fmt.Printf("boxes:                %d\n", len(boxes))
	fmt.Printf("mean right-edge err:  %.3f cells\n", s.MeanAbsRightEdgeErr)
	fmt.Printf("max right-edge err:   %.3f cells\n", s.MaxAbsRightEdgeErr)
	fmt.Printf("mean width err:       %.3f cells\n", s.MeanAbsWidthErr)
	fmt.Printf("drifted boxes (>=1):  %d\n", s.DriftedBoxes)
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
