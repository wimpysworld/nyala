package app

import (
	"testing"

	"github.com/alecthomas/kong"
)

// parse runs kong over args against a fresh CLI, the same wiring cmd/nyala uses.
func parse(t *testing.T, args ...string) CLI {
	t.Helper()
	var cli CLI
	parser, err := kong.New(
		&cli,
		kong.Name("nyala"),
		kong.Vars{"version": "test"},
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cli
}

func TestParseDefaults(t *testing.T) {
	cli := parse(t)
	if cli.Diagnostics {
		t.Errorf("Diagnostics = true, want false by default")
	}
	if cli.Grid != DefaultGrid {
		t.Errorf("Grid = %q, want %q", cli.Grid, DefaultGrid)
	}
	if cli.Out != DefaultOut {
		t.Errorf("Out = %q, want %q", cli.Out, DefaultOut)
	}
	if cli.UpdateGolden {
		t.Errorf("UpdateGolden = true, want false by default")
	}
	if len(cli.Fixture) != 0 {
		t.Errorf("Fixture = %v, want empty by default", cli.Fixture)
	}
	if cli.Image != "" {
		t.Errorf("Image = %q, want empty by default", cli.Image)
	}
}

func TestParseDiagnostics(t *testing.T) {
	cli := parse(t, "--diagnostics")
	if !cli.Diagnostics {
		t.Errorf("Diagnostics = false, want true")
	}
}

func TestParseFlags(t *testing.T) {
	cli := parse(
		t,
		"--diagnostics",
		"--fixture", "testcard-01-greyscale.png",
		"--fixture", "testcard-02-colour.png",
		"--grid", "80x24",
		"--out", "/tmp/diag-out",
		"--update-golden",
	)
	if !cli.Diagnostics {
		t.Errorf("Diagnostics = false, want true")
	}
	wantFix := []string{"testcard-01-greyscale.png", "testcard-02-colour.png"}
	if len(cli.Fixture) != len(wantFix) {
		t.Fatalf("Fixture = %v, want %v", cli.Fixture, wantFix)
	}
	for i, f := range wantFix {
		if cli.Fixture[i] != f {
			t.Errorf("Fixture[%d] = %q, want %q", i, cli.Fixture[i], f)
		}
	}
	if cli.Grid != "80x24" {
		t.Errorf("Grid = %q, want %q", cli.Grid, "80x24")
	}
	if cli.Out != "/tmp/diag-out" {
		t.Errorf("Out = %q, want %q", cli.Out, "/tmp/diag-out")
	}
	if !cli.UpdateGolden {
		t.Errorf("UpdateGolden = false, want true")
	}
}

func TestParseViewerImage(t *testing.T) {
	cli := parse(t, "page.png")
	if cli.Diagnostics {
		t.Errorf("Diagnostics = true, want false for a viewer run")
	}
	if cli.Image == "" {
		t.Errorf("Image is empty, want the positional path")
	}
}

func TestParseGrid(t *testing.T) {
	cols, rows, err := ParseGrid("120x40")
	if err != nil {
		t.Fatalf("ParseGrid(120x40): %v", err)
	}
	if cols != 120 || rows != 40 {
		t.Errorf("ParseGrid(120x40) = %d,%d, want 120,40", cols, rows)
	}

	for _, bad := range []string{"", "120", "120x", "x40", "0x40", "120x0", "axb", "12.0x40"} {
		if _, _, err := ParseGrid(bad); err == nil {
			t.Errorf("ParseGrid(%q): want error, got nil", bad)
		}
	}
}

func TestMessage(t *testing.T) {
	if Message() == "" {
		t.Errorf("Message() is empty")
	}
}
