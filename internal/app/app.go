// Package app holds the Nyala command-line surface and its dispatch. It owns one
// CLI struct, parsed by kong in cmd/nyala, and routes a parsed run to the viewer
// or the diagnostics sweep. The dispatch is thin: it parses flags, loads the
// fixtures and the gate files, and calls internal/viewer or internal/harness; it
// holds no rendering or harness logic of its own.
package app

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/harness"
	"github.com/wimpysworld/nyala/internal/viewer"
)

// Message returns the Nyala welcome line shown when the binary runs with no
// diagnostics flag and no viewer target.
func Message() string {
	return "Welcome to Nyala."
}

// DefaultFixtures lists the four committed testdata pages the sweep runs when
// --fixture is not given, in report order.
var DefaultFixtures = []string{
	"linuxmatters.png",
	"testcard-01-greyscale.png",
	"testcard-02-colour.png",
	"testcard-03-images.png",
}

const (
	// DefaultGrid is the pinned sweep grid, 120 columns by 40 rows.
	DefaultGrid = "120x40"
	// DefaultOut is the directory the sweep writes its reports and artefacts into.
	DefaultOut = "./diagnostics"
)

// CLI is the parsed Nyala command line. cmd/nyala binds it to kong; the fields
// carry the diagnostics flags and the optional viewer target. Diagnostics and
// the viewer are mutually exclusive: with --diagnostics the run is the sweep,
// otherwise the run is the viewer over the positional PNG.
type CLI struct {
	Version kong.VersionFlag `help:"Show version."`

	Diagnostics  bool     `help:"Run the diagnostics sweep over the fixtures instead of the viewer."`
	Fixture      []string `help:"Fixture PNGs under testdata/ to sweep; defaults to all four." placeholder:"FILE"`
	Grid         string   `help:"Sweep cell grid, COLSxROWS." default:"120x40"`
	Out          string   `help:"Directory for the sweep reports and artefacts." default:"./diagnostics"`
	UpdateGolden bool     `help:"Rewrite golden archives that changed, mirroring UPDATE_GOLDEN." name:"update-golden"`

	AllRenderers bool `help:"Force every render backend into the viewer cycle, skipping terminal capability detection." name:"all-renderers"`

	Color string `help:"Viewer colour mode: auto, truecolor, 256, or none." default:"auto" enum:"auto,truecolor,256,none"`

	Image string `arg:"" optional:"" help:"PNG to view when not running diagnostics."`
}

// Run dispatches a parsed CLI. With Diagnostics set it runs the sweep over the
// chosen fixtures at the chosen grid, writing into Out, and returns a non-nil
// error when the sweep breaches its thresholds. Otherwise it runs the viewer
// over the positional image on the given tty. testdataDir is the directory the
// fixtures and the baseline and thresholds files live in.
func Run(cli CLI, testdataDir string, tty *os.File) error {
	if cli.Diagnostics {
		return runDiagnostics(cli, testdataDir)
	}
	if cli.Image == "" {
		fmt.Println(Message())
		return nil
	}
	return viewer.Run(cli.Image, tty, cli.AllRenderers, cli.Color)
}

// runDiagnostics loads the fixtures and the gate files, runs the sweep, and maps
// any breach to a non-nil error so the caller exits non-zero.
func runDiagnostics(cli CLI, testdataDir string) error {
	cols, rows, err := ParseGrid(cli.Grid)
	if err != nil {
		return err
	}

	if cli.UpdateGolden {
		// The golden helpers read UPDATE_GOLDEN from the environment, so the flag
		// mirrors the env var rather than adding a parallel switch.
		_ = os.Setenv("UPDATE_GOLDEN", "1")
	}

	fixtures := cli.Fixture
	if len(fixtures) == 0 {
		fixtures = DefaultFixtures
	}

	inputs := make([]harness.SweepInput, 0, len(fixtures))
	for _, f := range fixtures {
		img, derr := decodePNG(filepath.Join(testdataDir, f))
		if derr != nil {
			return fmt.Errorf("app: load fixture %s: %w", f, derr)
		}
		inputs = append(inputs, harness.SweepInput{Page: pageName(f), Src: img})
	}

	rast, err := capture.NewRasteriser()
	if err != nil {
		return fmt.Errorf("app: rasteriser: %w", err)
	}

	baseline, err := harness.LoadBaseline(filepath.Join(testdataDir, "baseline.json"))
	if err != nil {
		return err
	}
	thresholds, err := harness.LoadThresholds(filepath.Join(testdataDir, "thresholds.json"))
	if err != nil {
		return err
	}

	cfg := harness.SweepConfig{Cols: cols, Rows: rows, OutDir: cli.Out}
	res, err := harness.RunSweep(rast, cfg, inputs, baseline, thresholds)
	if err != nil {
		return err
	}

	if len(res.Breaches) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "diagnostics: %d threshold breach(es)\n", len(res.Breaches))
		for _, br := range res.Breaches {
			fmt.Fprintf(&b, "  %s\n", br.String())
		}
		return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
	return nil
}

// ParseGrid parses a COLSxROWS grid string into its two positive dimensions. It
// rejects a missing separator, a non-numeric field, or a non-positive size.
func ParseGrid(s string) (cols, rows int, err error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("app: grid %q: want COLSxROWS", s)
	}
	cols, err = strconv.Atoi(parts[0])
	if err != nil || cols <= 0 {
		return 0, 0, fmt.Errorf("app: grid %q: cols must be a positive integer", s)
	}
	rows, err = strconv.Atoi(parts[1])
	if err != nil || rows <= 0 {
		return 0, 0, fmt.Errorf("app: grid %q: rows must be a positive integer", s)
	}
	return cols, rows, nil
}

// pageName turns a fixture filename into its sweep page key, stripping the
// directory and the extension.
func pageName(card string) string {
	base := filepath.Base(card)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// decodePNG reads a PNG file and returns it as an *image.RGBA in RGBA byte
// order, converting from the decoded image type when needed.
func decodePNG(path string) (*image.RGBA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba, nil
	}
	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)
	return rgba, nil
}
