package viewer

import (
	"bytes"
	"image"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// tabCycleEngine drives the streamSession end to end for the Tab-cycle test. It
// records every SetBackend name and, on each SetBackend, pushes one fresh frame
// so the drawing loop repaints with the newly selected backend, mimicking the
// real engine's restartStream re-emit.
type tabCycleEngine struct {
	frames chan render.Frame

	mu    sync.Mutex
	names []string
}

func (e *tabCycleEngine) SetBackend(name string) {
	e.mu.Lock()
	e.names = append(e.names, name)
	e.mu.Unlock()
	// The real engine restarts the screencast on a backend switch and emits a
	// fresh frame sized for the new backend. Mirror that so the loop repaints.
	e.frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
}

func (e *tabCycleEngine) backends() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.names...)
}

func (e *tabCycleEngine) Frames() <-chan render.Frame { return e.frames }
func (e *tabCycleEngine) SetScheme(string)            {}
func (e *tabCycleEngine) SetCellPx(int, int)          {}
func (e *tabCycleEngine) SetLayoutWidth(int)          {}
func (e *tabCycleEngine) Resize(int, int)             {}
func (e *tabCycleEngine) Navigate(string) error       { return nil }
func (e *tabCycleEngine) SendKey(string)              {}
func (e *tabCycleEngine) SendScroll(int)              {}
func (e *tabCycleEngine) Close() error                { return nil }

// syncBuf is a concurrency-safe sink for the pipe reader goroutine.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls fn until it returns true or the deadline passes.
func waitFor(d time.Duration, fn func() bool) bool {
	deadline := time.After(d)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return true
		}
		select {
		case <-deadline:
			return fn()
		case <-tick.C:
		}
	}
}

// TestStreamTabCyclesBackend drives the real streamSession.loop through a Tab
// press and asserts the on-screen backend actually flips. It starts on kitty,
// presses Tab, and checks that the engine received a SetBackend for the next
// backend and that the state advanced to block. This locks the whole Tab-cycle
// wiring the unit-level State tests do not cover: the streamSession.handleKey
// path that submits the engine restart and repaints at the new backend.
func TestStreamTabCyclesBackend(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	out := &syncBuf{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(out, pr)
	}()

	eng := &tabCycleEngine{frames: make(chan render.Frame, 4)}

	st := NewState(8, 4, 16, 16)
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	done := make(chan struct{})
	ctl := newControl(eng, done)

	s := &streamSession{eng: eng, st: &st, tty: pw, fd: pw.Fd(), ctl: ctl, diff: diff}

	winch := make(chan os.Signal, 1)
	keys := make(chan keyEvent, 2)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = s.loop(winch, keys)
	}()

	// Draw one kitty frame first, so the pre-Tab output carries a kitty escape.
	eng.frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	if !waitFor(2*time.Second, func() bool { return strings.Contains(out.String(), "\x1b_G") }) {
		t.Fatalf("no kitty graphics escape before Tab; output = %q", out.String())
	}

	// Press Tab: it must cycle the state to block and submit a SetBackend.
	keys <- keyEvent{b: keyTab}

	if !waitFor(2*time.Second, func() bool {
		for _, n := range eng.backends() {
			if n == "block" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("Tab did not submit SetBackend(block); got %v", eng.backends())
	}
	if st.Backend != BackendBlock {
		t.Fatalf("after Tab, state backend = %d, want BackendBlock", st.Backend)
	}

	// Quit the loop and stop the reader.
	keys <- keyEvent{b: 'q'}
	<-loopDone
	close(done)
	_ = pw.Close()
	<-drained
	_ = pr.Close()
}
