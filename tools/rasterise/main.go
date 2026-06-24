// Command rasterise turns a captured terminal-model.json (the composed block
// cell grid a NYALA_CAPTURE bundle writes per frame, block backend only) into a
// block-render.png beside it. It lets the owner compare a capture's source
// screenshot.png against the block render to judge text-layout accuracy. It
// reuses the capture rasteriser and PNG encoder, so the render matches the
// shipped path. Run it via `just rasterise <path>`.
//
// The argument is either a terminal-model.json file or a frame directory that
// contains one; the tool resolves the directory case to the file. It writes
// block-render.png in the same directory and prints the path. A 120x40 grid
// rasterises to 960x640 at the pinned 8x16 cell.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wimpysworld/nyala/internal/capture"
)

// modelName is the per-frame terminal-model.json filename the bundle writes.
const modelName = "terminal-model.json"

// outName is the block-render.png filename the tool writes beside the model.
const outName = "block-render.png"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: rasterise <terminal-model.json | frame-dir>\n")
		os.Exit(2)
	}
	out, err := run(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "rasterise: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// run resolves arg to a terminal-model.json file, rasterises the grid through
// the capture rasteriser, and writes block-render.png beside the model. It
// returns the output path. It fails if the model is missing, is not a valid
// block model, or the image cannot be written.
func run(arg string) (string, error) {
	modelPath, err := resolveModel(arg)
	if err != nil {
		return "", err
	}
	grid, err := capture.ReadTerminalModel(modelPath)
	if err != nil {
		return "", err
	}
	r, err := capture.NewRasteriser()
	if err != nil {
		return "", fmt.Errorf("build rasteriser: %w", err)
	}
	img := r.Rasterise(grid)
	outPath := filepath.Join(filepath.Dir(modelPath), outName)
	f, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", outName, err)
	}
	defer func() { _ = f.Close() }()
	if err := capture.EncodePNG(f, img); err != nil {
		return "", fmt.Errorf("encode %s: %w", outName, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", outName, err)
	}
	return outPath, nil
}

// resolveModel maps arg to a terminal-model.json path: a directory yields the
// model inside it, a file is used as is. It fails when the path does not exist
// or a directory holds no terminal-model.json.
func resolveModel(arg string) (string, error) {
	info, err := os.Stat(arg)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		modelPath := filepath.Join(arg, modelName)
		if _, serr := os.Stat(modelPath); serr != nil {
			return "", fmt.Errorf("no %s in %s (block backend only)", modelName, arg)
		}
		return modelPath, nil
	}
	return arg, nil
}
