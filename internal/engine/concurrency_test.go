package engine

import (
	"sync"
	"testing"
)

// TestEngineFieldsRaceFree drives the viewer-facing methods that read and write
// the session and geometry fields (Resize, SetBackend, SendKey, SendScroll,
// SetCellPx) concurrently with a goroutine that churns the same fields under the
// engine mutex. It keeps ctx and
// queue nil so no chromedp I/O fires, so the test stays browser-free; run under
// -race it asserts the mutex guards every shared field. A regression that dropped
// a lock would trip the race detector here.
func TestEngineFieldsRaceFree(t *testing.T) {
	t.Parallel()
	e := New("")
	e.SetCellPx(8, 16)

	const iterations = 500
	var wg sync.WaitGroup

	// Viewer goroutine: resize, switch backend, and send input.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iterations {
			e.Resize(80+i%40, 24+i%20)
			if i%2 == 0 {
				e.SetBackend("block")
			} else {
				e.SetBackend("sixel")
			}
			e.SendKey(Key{Code: "ArrowDown"})
			e.SendScroll(40)
			e.SetCellPx(8+i%4, 16+i%4)
		}
	}()

	// Supervisor stand-in: churn the locked session fields as a rebuild would,
	// without launching a browser.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iterations {
			e.mu.Lock()
			e.scrollX, e.scrollY = i, i*2
			e.cols, e.rows = 100, 40
			e.ctx = nil
			e.mu.Unlock()
		}
	}()

	wg.Wait()
}
