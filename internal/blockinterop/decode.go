// Package blockinterop decodes Block terminal streams for offline compatibility
// checks. Production code does not import this package.
package blockinterop

import (
	"fmt"
	"image/color"

	"github.com/charmbracelet/x/vt"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// Decode decodes a complete Block terminal byte stream through a VT emulator
// and returns the visible cell state as a block.Grid. The decoded grid is a
// visual model: every drawable grapheme is stored in Cell.Text because a
// terminal stream does not retain whether the producer sourced it from Text or
// Glyph. Wide-cell placeholders are restored as Cell.Wide. The result can be
// compared directly with the grid returned by the production compose seam.
func Decode(payload []byte, cols, rows int) (block.Grid, error) {
	if cols <= 0 || rows <= 0 {
		return block.Grid{}, fmt.Errorf("blockinterop: invalid grid %dx%d", cols, rows)
	}

	emulator := vt.NewEmulator(cols, rows)
	if _, err := emulator.Write([]byte("\x1b[?7l")); err != nil {
		return block.Grid{}, fmt.Errorf("blockinterop: terminal setup: %w", err)
	}
	if _, err := emulator.Write(payload); err != nil {
		return block.Grid{}, fmt.Errorf("blockinterop: decode stream: %w", err)
	}

	grid := block.Grid{Cols: cols, Rows: rows, Cells: make([]block.Cell, cols*rows)}
	for row := range rows {
		for col := range cols {
			terminalCell := emulator.CellAt(col, row)
			if terminalCell == nil {
				return block.Grid{}, fmt.Errorf("blockinterop: missing cell (%d,%d)", col, row)
			}
			if terminalCell.Width == 0 {
				if col == 0 {
					return block.Grid{}, fmt.Errorf("blockinterop: orphan wide cell at (%d,%d)", col, row)
				}
				lead := grid.At(col-1, row)
				*grid.At(col, row) = block.Cell{FG: lead.FG, BG: lead.BG, Wide: true}
				continue
			}

			fg, err := terminalRGBA(terminalCell.Style.Fg)
			if err != nil {
				return block.Grid{}, fmt.Errorf("blockinterop: cell (%d,%d) foreground: %w", col, row, err)
			}
			bg, err := terminalRGBA(terminalCell.Style.Bg)
			if err != nil {
				return block.Grid{}, fmt.Errorf("blockinterop: cell (%d,%d) background: %w", col, row, err)
			}
			*grid.At(col, row) = block.Cell{Text: terminalCell.Content, FG: fg, BG: bg}
		}
	}
	return grid, nil
}

func terminalRGBA(c color.Color) (color.RGBA, error) {
	if c == nil {
		return color.RGBA{}, fmt.Errorf("missing colour")
	}
	rgba := color.RGBAModel.Convert(c).(color.RGBA)
	rgba.A = 0xff
	return rgba, nil
}
