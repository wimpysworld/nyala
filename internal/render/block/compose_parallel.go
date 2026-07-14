package block

import (
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/rowband"
)

// composeBandMinRows is the smallest grid-row count one compose band covers. The
// pool never splits below it, so a band always carries at least this many rows and
// the fan-out stays fat: a small grid runs fewer, larger bands, or falls back to
// serial when it holds fewer than this many rows.
const composeBandMinRows = 4

// composeBand binarises grid rows [row0, row1) of g from the frame image,
// dispatching on the renderer's Scheme. It is the per-band body the parallel
// compose hands one worker: it writes only the cells in its own row range and
// reads the shared source image read-only, so disjoint bands never race and the
// join is byte-identical to the serial compose. It folds no overlay; the overlay
// runs once after the band-join.
func (br *Renderer) composeBand(g *Grid, f render.Frame, row0, row1 int) {
	switch br.Scheme {
	case SchemeHalfBlock:
		br.composeHalfBlockRows(g, f, row0, row1)
	case SchemeSextant:
		br.composeSextantRows(g, f, row0, row1)
	default:
		br.composeQuadrantRows(g, f, row0, row1)
	}
}

// ParallelCompose composes a Frame onto a grid across a rowband.Pool by splitting
// the binarise pass into whole grid-row bands, then folds the text overlay on once
// after the band-join. It is the parallel twin of Renderer.ComposeInto and
// byte-identical to it: the binarise is per-cell independent, so a disjoint row
// band writes the same cells the serial pass writes, and the overlay stays
// sequential (first-writer-wins by DOM order) after every band completes. It holds
// one band closure built once by NewParallelCompose, so a reused driver allocates
// nothing per frame. Drive it from a single goroutine, so the pool's WaitGroup
// never overlaps calls.
type ParallelCompose struct {
	br   Renderer             // current frame's renderer, held by value so the band takes no per-frame heap
	g    *Grid                // current frame's destination grid; the band writes it
	f    render.Frame         // current frame; the band reads its image
	band func(row0, row1 int) // persistent closure over br/g/f; built once
}

// NewParallelCompose builds a compose driver with its band closure built once.
// Reuse one driver across frames, driving it from a single goroutine, so no frame
// allocates a closure and the pool's WaitGroup never overlaps calls.
func NewParallelCompose() *ParallelCompose {
	pc := &ParallelCompose{}
	pc.band = func(row0, row1 int) { pc.br.composeBand(pc.g, pc.f, row0, row1) }
	return pc
}

// ComposeInto composes f onto dst using br's scheme, splitting the binarise pass
// across pool by whole grid-row bands, then folds f's text overlay on. It reuses
// dst's Cells backing array (via reuseGrid) and returns dst. The output is
// byte-identical to Renderer.ComposeInto: the parallel binarise writes the same
// cells, and the overlay fold is unchanged. A nil pool or a grid too small to split
// binarises serially on the calling goroutine.
func (pc *ParallelCompose) ComposeInto(br *Renderer, dst *Grid, f render.Frame, pool *rowband.Pool) *Grid {
	f = rgbaFrame(f)
	reuseGrid(dst, br.Cols, br.Rows)
	pc.br = *br
	pc.g = dst
	pc.f = f
	if pool == nil {
		pc.band(0, dst.Rows)
	} else {
		pool.Run(dst.Rows, composeBandMinRows, pc.band)
	}
	applyOverlay(dst, f, br.Scheme)
	return dst
}
