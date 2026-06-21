package harness

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/txtar"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// goldenArchiveComment is the leading comment stored at the top of every golden
// txtar archive. It records the format so the file reads on its own.
const goldenArchiveComment = `Nyala Stage 0 golden block grid.
One txtar file per page. The "grid" file holds the deterministic block cell grid
as one line per cell: "col,row glyph fgHex bgHex textFlag". Colours are RRGGBB
hex, textFlag is t for a text cell and b for a bitmap cell. Regenerate with
UPDATE_GOLDEN=1 go test ./internal/harness/...
`

// gridFileName is the name of the single file inside each golden archive that
// holds the encoded cell grid.
const gridFileName = "grid"

// GoldenCell is one cell of a golden grid: the block cell plus the text flag the
// classifier assigned. The flag is separate because block.Cell carries no
// classification of its own.
type GoldenCell struct {
	block.Cell      // glyph, fg, bg
	Text       bool // true for a text cell, false for a bitmap cell
}

// EncodeGrid serialises a grid and its per-cell text flags to the deterministic
// golden archive bytes for one page. flags must be nil or len Cols*Rows in
// row-major order; a nil slice classifies every cell as a bitmap cell. The
// output is byte-identical for identical inputs.
func EncodeGrid(page string, g block.Grid, flags []bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "cols %d rows %d\n", g.Cols, g.Rows)
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			c := g.At(col, row)
			text := false
			if i := row*g.Cols + col; flags != nil && i < len(flags) {
				text = flags[i]
			}
			fmt.Fprintf(
				&b, "%d,%d %s %s %s %s\n",
				col, row,
				glyphField(c.Glyph),
				hexRGB(c.FG.R, c.FG.G, c.FG.B),
				hexRGB(c.BG.R, c.BG.G, c.BG.B),
				flagField(text),
			)
		}
	}
	ar := &txtar.Archive{
		Comment: []byte(goldenArchiveComment),
		Files:   []txtar.File{{Name: gridFileName, Data: []byte(b.String())}},
	}
	return txtar.Format(ar)
}

// CompareGolden compares the encoded grid for page against the committed archive
// at path. It returns ok true when they match byte for byte. When update is
// true it writes the encoded bytes to path only if they differ (write-if-
// changed), creating parent directories as needed, and reports ok true once the
// file matches. A missing file under update is treated as changed and written.
func CompareGolden(path, page string, g block.Grid, flags []bool, update bool) (ok bool, wrote bool, err error) {
	want := EncodeGrid(page, g, flags)
	got, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if bytes.Equal(got, want) {
			return true, false, nil
		}
	case os.IsNotExist(readErr):
		// Missing golden: a match is impossible; fall through to update logic.
	default:
		return false, false, readErr
	}

	if !update {
		return false, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, false, err
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// glyphField encodes a glyph rune as its uppercase hex code point prefixed with
// U+, so the grid stays single-line and diffs cleanly regardless of the rune's
// own width.
func glyphField(r rune) string {
	return "U+" + strings.ToUpper(strconv.FormatInt(int64(r), 16))
}

// hexRGB encodes three channel bytes as a six-digit uppercase RRGGBB hex string.
// Channel order is RGB, matching the RGBA cell colours.
func hexRGB(r, g, b uint8) string {
	return fmt.Sprintf("%02X%02X%02X", r, g, b)
}

// flagField encodes the text flag as "t" for a text cell or "b" for a bitmap
// cell.
func flagField(text bool) string {
	if text {
		return "t"
	}
	return "b"
}
