// Command placement-score reads a per-frame text-map.json (the text runs a
// NYALA_CAPTURE bundle writes per frame, with on-screen box geometry) and scores
// horizontal text placement error against the pinned 8x16 cell. It quantifies how
// far the block renderer's one-grapheme-per-cell layout drifts a run's right edge
// from the box's true right edge, so the owner can measure block-render accuracy
// across the corpus and show before/after numbers. It reuses the metrics
// Placement function, so the score matches the measured path. Run it via
// `just placement-score <path>`.
//
// The argument is either a text-map.json file or a frame directory that contains
// one; the tool resolves the directory case to the file. It prints the frame
// aggregates: mean and max right-edge error in cells, mean width error in cells,
// the drifted-box count, and the total box count.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// mapName is the per-frame text-map.json filename the bundle writes.
const mapName = "text-map.json"

// cellW and cellH are the pinned cell size in CSS pixels, matching the bundle's
// performance.json cell_px.
const (
	cellW = 8
	cellH = 16
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

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: placement-score <text-map.json | frame-dir>\n")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "placement-score: %v\n", err)
		os.Exit(1)
	}
}

// run resolves arg to a text-map.json file, decodes the boxes, scores them with
// metrics.Placement at the pinned cell, and prints the frame aggregates. It fails
// if the file is missing or not valid JSON.
func run(arg string) error {
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
	boxes := make([]metrics.PlacementBox, len(entries))
	for i, e := range entries {
		boxes[i] = metrics.PlacementBox{X: e.X, W: e.W, Col: e.Col, Text: e.Text}
	}
	s := metrics.Placement(boxes, cellW, cellH)
	fmt.Printf("boxes:                %d\n", len(boxes))
	fmt.Printf("mean right-edge err:  %.3f cells\n", s.MeanAbsRightEdgeErr)
	fmt.Printf("max right-edge err:   %.3f cells\n", s.MaxAbsRightEdgeErr)
	fmt.Printf("mean width err:       %.3f cells\n", s.MeanAbsWidthErr)
	fmt.Printf("drifted boxes (>=1):  %d\n", s.DriftedBoxes)
	return nil
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
