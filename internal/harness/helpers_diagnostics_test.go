//go:build diagnostics || ocr

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wimpysworld/nyala/internal/cellraster"
)

// These helpers serve the heavy, rasteriser-driven tests behind the diagnostics
// and ocr tags. They leave the default build, so the fast `just test` gate
// neither compiles nor runs them.

// logProgress returns a Progress callback that streams each line live and also
// records it on the test. Go's `-v` output buffers t.Log until the test returns,
// so a long pass would print its lines only at the end; writing the line to
// stderr first makes it appear the moment the step starts, so the operator sees
// the sweep and calibration advance rather than a silent stretch. The t.Log call
// keeps the same line attached to the test output for a failure report.
func logProgress(t *testing.T) Progress {
	t.Helper()
	return func(line string) {
		fmt.Fprintln(os.Stderr, line)
		t.Log(line)
	}
}

// sweepCards lists the corpus the sweep runs over: the Linux Matters page and
// the four test pages, matching the five committed fixtures.
var sweepCards = []string{
	"linuxmatters.png",
	"testpage-01-grayscale.png",
	"testpage-02-basic.png",
	"testpage-03-cmyk.png",
	"testpage-04-color.png",
}

// thresholdsPath returns the committed thresholds gate file.
func thresholdsPath() string { return filepath.Join("..", "..", "testdata", "thresholds.json") }

// calibrationPath returns the committed calibration file path.
func calibrationPath() string {
	return filepath.Join("..", "..", "testdata", "calibration.json")
}

// updateBaseline reports whether UPDATE_BASELINE is set, mirroring UPDATE_GOLDEN.
func updateBaseline() bool {
	v := os.Getenv("UPDATE_BASELINE")
	return v != "" && v != "0"
}

// updateCalibration reports whether the UPDATE_CALIBRATION environment flag is
// set, mirroring the UPDATE_GOLDEN convention.
func updateCalibration() bool {
	v := os.Getenv("UPDATE_CALIBRATION")
	return v != "" && v != "0"
}

// pageName turns a fixture filename into its page name, stripping the extension.
func pageName(card string) string {
	return card[:len(card)-len(filepath.Ext(card))]
}

// loadSweepInputs decodes the corpus PNGs into sweep inputs.
func loadSweepInputs(t *testing.T) []SweepInput {
	t.Helper()
	inputs := make([]SweepInput, 0, len(sweepCards))
	for _, card := range sweepCards {
		img := loadRGBA(t, filepath.Join("..", "..", "testdata", card))
		inputs = append(inputs, SweepInput{Page: pageName(card), Src: img})
	}
	return inputs
}

// sweepConfig builds the pinned 120x40 sweep config writing into dir.
func sweepConfig(dir string) SweepConfig {
	return SweepConfig{Viewport: Viewport{Cols: CalibCols, Rows: CalibRows}, OutDir: dir, NyalaCommit: "test"}
}

// loadCalibInputs decodes the four calibration test pages into RGBA inputs.
func loadCalibInputs(t *testing.T) []CalibInput {
	t.Helper()
	inputs := make([]CalibInput, 0, len(CalibCards))
	for _, card := range CalibCards {
		img := loadRGBA(t, filepath.Join("..", "..", "testdata", card))
		inputs = append(inputs, CalibInput{Card: card, Src: img})
	}
	return inputs
}

// newRasteriser builds a Route A rasteriser or fails the test.
func newRasteriser(t *testing.T) *cellraster.Rasteriser {
	t.Helper()
	r, err := cellraster.NewRasteriser()
	if err != nil {
		t.Fatalf("NewRasteriser: %v", err)
	}
	return r
}

// loadCommittedCalibration reads the committed calibration file, returning the
// zero value when it is absent.
func loadCommittedCalibration(t *testing.T) Calibration {
	t.Helper()
	b, err := os.ReadFile(calibrationPath())
	if os.IsNotExist(err) {
		return Calibration{}
	}
	if err != nil {
		t.Fatalf("read calibration: %v", err)
	}
	var c Calibration
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal calibration: %v", err)
	}
	return c
}
