package rowband

import "testing"

// TestRunTilesRows proves Run partitions [0, rows) into disjoint whole-row bands
// that cover every row exactly once, across even and uneven splits, a single-row
// grid, a sub-threshold grid (serial fallback), and a zero-row grid. Each band
// writes only its own disjoint rows, so the shared counter needs no lock and the
// test stays race-clean; a gap or overlap surfaces as a row counted other than
// once.
func TestRunTilesRows(t *testing.T) {
	pool := NewSized(8)
	defer pool.Close()

	cases := []struct{ rows, minRows int }{
		{160, 16}, // even split into 8 bands
		{344, 16}, // uneven remainder across 8 bands
		{40, 4},   // compose grid, even
		{86, 4},   // compose grid, uneven
		{3, 4},    // below the minimum: serial fallback
		{1, 4},    // single row: serial fallback
		{0, 4},    // zero rows: no work
	}
	for _, c := range cases {
		covered := make([]int, c.rows)
		pool.Run(c.rows, c.minRows, func(row0, row1 int) {
			for i := row0; i < row1; i++ {
				covered[i]++
			}
		})
		for i, n := range covered {
			if n != 1 {
				t.Fatalf("rows=%d minRows=%d: row %d covered %d times, want 1", c.rows, c.minRows, i, n)
			}
		}
	}
}

// TestRunReuse proves one pool tiles correctly across repeated calls, since a live
// path reuses a single pool every frame; a stale WaitGroup would surface as a hang
// or a miscount here.
func TestRunReuse(t *testing.T) {
	pool := NewSized(4)
	defer pool.Close()
	for range 50 {
		covered := make([]int, 100)
		pool.Run(100, 8, func(row0, row1 int) {
			for i := row0; i < row1; i++ {
				covered[i]++
			}
		})
		for i, n := range covered {
			if n != 1 {
				t.Fatalf("row %d covered %d times, want 1", i, n)
			}
		}
	}
}
