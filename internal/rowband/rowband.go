// Package rowband runs a per-row band function across a persistent set of worker
// goroutines. It generalises the block-render row-band parallelism: a caller
// splits its work by whole destination rows, and the pool fans those bands over
// workers sized from GOMAXPROCS-1, read once at build. The pool is built once and
// reused every call, so a hot path spawns no goroutine and allocates nothing when
// the caller supplies a persistent band closure. It is driven from one goroutine
// (the caller reuses one WaitGroup barrier), so calls never overlap.
package rowband

import (
	"runtime"
	"sync"
)

// Pool fans a row-band function over persistent workers. Split the work into
// whole-row bands, hand each band to a worker, and barrier every band before Run
// returns. A band writes only its own disjoint rows and reads shared inputs
// read-only, so bands never race and the joined result is byte-identical to the
// serial whole-range call. Build it with New and release it with Close.
type Pool struct {
	maxBands int            // upper band count, sized from GOMAXPROCS at build
	jobs     chan band      // buffered to maxBands so the driver never blocks on send
	wg       sync.WaitGroup // reused per call; barrier before the driver returns
}

// band is one whole-row band of work: call fn over rows [row0, row1). wg is the
// call's barrier the worker signals when the band is done.
type band struct {
	fn         func(row0, row1 int)
	row0, row1 int
	wg         *sync.WaitGroup
}

// New builds a pool sized from runtime.GOMAXPROCS(0), read once here. It runs at
// most GOMAXPROCS-1 bands, leaving one core for the caller and the shared
// headless-Chromium load, and starts that many persistent workers. A single-core
// box gets one worker, so the pool degrades to serial. Call Close to stop the
// workers.
func New() *Pool {
	return NewSized(runtime.GOMAXPROCS(0) - 1)
}

// NewSized builds a pool with persistent worker goroutines, clamped to at least
// one. It is the shared constructor; New sizes workers from GOMAXPROCS, and tests
// pass an explicit count to force the multi-band path.
func NewSized(workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	p := &Pool{maxBands: workers, jobs: make(chan band, workers)}
	for range workers {
		go p.worker()
	}
	return p
}

// worker runs one persistent goroutine: it calls each band's function and signals
// the band's barrier. It exits when Close shuts the jobs channel.
func (p *Pool) worker() {
	for b := range p.jobs {
		b.fn(b.row0, b.row1)
		b.wg.Done()
	}
}

// Close stops the pool's workers. After Close the pool must not be used again. A
// process-lifetime pool never calls Close; tests call it to release the goroutines.
func (p *Pool) Close() {
	close(p.jobs)
}

// Run splits rows into whole-row bands, each covering at least minRows rows, and
// calls fn once per band across the workers, blocking until every band finishes.
// fn must write only rows in the [row0, row1) range it is handed and read all
// other state read-only, so disjoint bands never race. A grid too small to split
// (fewer than 2*minRows rows, including a single row or a zero-row grid) runs fn
// once serially on the calling goroutine and pays no fan-out overhead. The reused
// WaitGroup barriers every band before Run returns, so the work is complete on
// return and the pool is free for the next call.
func (p *Pool) Run(rows, minRows int, fn func(row0, row1 int)) {
	n := p.bands(rows, minRows)
	if n <= 1 {
		fn(0, rows)
		return
	}
	p.wg.Add(n)
	base, rem := rows/n, rows%n
	y := 0
	for i := range n {
		h := base
		if i < rem {
			h++
		}
		p.jobs <- band{fn: fn, row0: y, row1: y + h, wg: &p.wg}
		y += h
	}
	p.wg.Wait()
}

// bands returns how many whole-row bands to split rows into: the pool's worker
// count, capped so each band covers at least minRows rows. It returns at least
// one, so a grid too small to split runs serially. minRows below one is treated
// as one, so a caller cannot force an unbounded band count.
func (p *Pool) bands(rows, minRows int) int {
	if minRows < 1 {
		minRows = 1
	}
	n := p.maxBands
	if m := rows / minRows; m < n {
		n = m
	}
	if n < 1 {
		return 1
	}
	return n
}
