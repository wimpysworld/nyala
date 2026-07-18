package blockinterop

import (
	"image/color"
	"testing"
)

func TestDecodeTrueColour(t *testing.T) {
	payload := []byte("\x1b[1;1H\x1b[38;2;1;2;3;48;2;4;5;6mA\x1b[38;2;7;8;9;48;2;10;11;12mB")
	grid, err := Decode(payload, 2, 1)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []struct {
		text string
		fg   color.RGBA
		bg   color.RGBA
	}{
		{"A", color.RGBA{1, 2, 3, 0xff}, color.RGBA{4, 5, 6, 0xff}},
		{"B", color.RGBA{7, 8, 9, 0xff}, color.RGBA{10, 11, 12, 0xff}},
	}
	for col, expected := range want {
		cell := grid.At(col, 0)
		if cell.Text != expected.text || cell.FG != expected.fg || cell.BG != expected.bg {
			t.Errorf("cell %d = %+v, want text %q fg %v bg %v", col, *cell, expected.text, expected.fg, expected.bg)
		}
	}
}

func TestDecodeWideCell(t *testing.T) {
	payload := []byte("\x1b[1;1H\x1b[38;2;1;2;3;48;2;4;5;6m世")
	grid, err := Decode(payload, 2, 1)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := grid.At(0, 0).Text; got != "世" {
		t.Fatalf("lead Text = %q, want 世", got)
	}
	trail := grid.At(1, 0)
	if !trail.Wide {
		t.Fatal("trailing cell Wide = false, want true")
	}
	if trail.BG != grid.At(0, 0).BG {
		t.Errorf("trailing BG = %v, want lead BG %v", trail.BG, grid.At(0, 0).BG)
	}
}

func TestDecodeRejectsInvalidGrid(t *testing.T) {
	if _, err := Decode(nil, 0, 1); err == nil {
		t.Fatal("Decode with zero columns returned nil error")
	}
}
