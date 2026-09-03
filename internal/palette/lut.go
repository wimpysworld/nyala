package palette

import (
	"image/color"
	"runtime"
	"sync"
)

// NewLUT builds a fresh RGB555 lookup table for the palette, based at register
// zero.
func NewLUT(palette color.Palette) []uint8 {
	return BuildLUT(nil, palette, 0)
}

// BuildLUT fills lut with the nearest palette register for every RGB555 key,
// reusing the given slice when it is exactly the right size. It returns the
// table it wrote. Each entry is base plus the position of the nearest palette
// colour, so the table holds absolute terminal register numbers and its
// consumer adds nothing; base+len(palette) must not exceed 256. The build fans
// across WorkerCount goroutines; the split changes the work per goroutine,
// never the result.
func BuildLUT(lut []uint8, palette color.Palette, base int) []uint8 {
	var pr, pg, pb [256]int32
	for i, colour := range palette {
		r, g, b, _ := colour.RGBA()
		pr[i], pg[i], pb[i] = int32(r>>8), int32(g>>8), int32(b>>8)
	}
	if len(lut) != 1<<15 || cap(lut) != 1<<15 {
		lut = make([]uint8, 1<<15)
	}
	workers := WorkerCount()
	var wg sync.WaitGroup
	for worker := range workers {
		lo, hi := len(lut)*worker/workers, len(lut)*(worker+1)/workers
		wg.Go(func() {
			for i := lo; i < hi; i++ {
				r := int32(i>>10&31<<3 | 4) //nolint:gosec // A 15-bit LUT index bounds this to 252.
				g := int32(i>>5&31<<3 | 4)  //nolint:gosec // A 15-bit LUT index bounds this to 252.
				b := int32(i&31<<3 | 4)     //nolint:gosec // A 15-bit LUT index bounds this to 252.
				best, bestDistance := 0, int32(1<<31-1)
				for j := range palette {
					dr, dg, db := r-pr[j], g-pg[j], b-pb[j]
					if distance := dr*dr + dg*dg + db*db; distance < bestDistance {
						best, bestDistance = j, distance
					}
				}
				lut[i] = uint8(base + best) //nolint:gosec // base+len(palette) is bounded by 256.
			}
		})
	}
	wg.Wait()
	return lut
}

// WorkerCount returns the shared goroutine budget for palette and backend
// parallelism: at most four, and never more than GOMAXPROCS.
func WorkerCount() int { return min(4, runtime.GOMAXPROCS(0)) }
