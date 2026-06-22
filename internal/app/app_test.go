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
	if cli.Color != "auto" {
		t.Errorf("Color = %q, want %q by default", cli.Color, "auto")
	}
	if cli.Image != "" {
		t.Errorf("Image = %q, want empty by default", cli.Image)
	}
}

func TestParseViewerImage(t *testing.T) {
	cli := parse(t, "page.png")
	if cli.Image == "" {
		t.Errorf("Image is empty, want the positional path")
	}
}

func TestParseColorDefault(t *testing.T) {
	cli := parse(t)
	if cli.Color != "auto" {
		t.Errorf("Color = %q, want %q by default", cli.Color, "auto")
	}
}

func TestParseColorOverrides(t *testing.T) {
	for _, mode := range []string{"truecolor", "256", "none"} {
		cli := parse(t, "--color", mode)
		if cli.Color != mode {
			t.Errorf("--color %s: Color = %q, want %q", mode, cli.Color, mode)
		}
	}
}

func TestParseColorInvalid(t *testing.T) {
	var cli CLI
	parser, err := kong.New(
		&cli,
		kong.Name("nyala"),
		kong.Vars{"version": "test"},
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--color", "bogus"}); err == nil {
		t.Errorf("--color bogus: want a parse error, got nil")
	}
}
