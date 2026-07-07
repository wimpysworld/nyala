package viewer

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// slowWriter is an in-memory tty stand-in that optionally sleeps on each Write, so
// a test can make the writer goroutine lag and exercise the producer's drop policy.
// It flags any Write that lands after markClosed, which pins the no-write-after-stop
// teardown invariant, and counts writes for the drop-policy assertion. All fields
// are guarded, so the producer and writer goroutines may touch it concurrently
// under -race.
type slowWriter struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	delay      time.Duration
	closed     bool
	afterClose bool
	writes     int
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.afterClose = true
	}
	w.writes++
	return w.buf.Write(p)
}

func (w *slowWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *slowWriter) markClosed() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

func (w *slowWriter) wroteAfterClose() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.afterClose
}

func (w *slowWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// pipeEngine is a minimal Engine that hands a scripted frames channel to the loop
// and no-ops every control call, so a test drives streamSession.loop end to end
// without a browser.
type pipeEngine struct {
	frames chan render.Frame
}

func (e *pipeEngine) EnableCapture(io.Writer) CaptureSession { return nil }
func (e *pipeEngine) SetCaptureSettleHold(bool)              {}
func (e *pipeEngine) SetBackend(string)                      {}
func (e *pipeEngine) SetScheme(string)                       {}
func (e *pipeEngine) SetCellPx(int, int)                     {}
func (e *pipeEngine) SetLayoutWidth(int)                     {}
func (e *pipeEngine) Resize(int, int)                        {}
func (e *pipeEngine) Navigate(string) error                  { return nil }
func (e *pipeEngine) Frames() <-chan render.Frame            { return e.frames }
func (e *pipeEngine) SendKey(string)                         {}
func (e *pipeEngine) SendScroll(int)                         {}
func (e *pipeEngine) Close() error                           { return nil }

// TestStreamWriterOrdersItemsUnderSlowTTY drives the two-buffer ping-pong against a
// slow in-memory tty and asserts the writer emits every frame in submission order
// with none lost. A single writer draining a cap-1 ready channel gives strict order
// regardless of the encode/write overlap, and returned buffers are reused only after
// the writer is done with them, so -race sees no buffer race.
func TestStreamWriterOrdersItemsUnderSlowTTY(t *testing.T) {
	const n = 60
	tty := &slowWriter{delay: 50 * time.Microsecond}
	w := newStreamWriter(tty)

	avail := []*bytes.Buffer{{}, {}}
	for i := range n {
		var buf *bytes.Buffer
		if len(avail) > 0 {
			buf = avail[len(avail)-1]
			avail = avail[:len(avail)-1]
		} else {
			buf = (<-w.free).buf
		}
		buf.Reset()
		fmt.Fprintf(buf, "<%d>", i)
		w.send(writeItem{buf: buf})
	}
	w.stop()

	var want bytes.Buffer
	for i := range n {
		fmt.Fprintf(&want, "<%d>", i)
	}
	if got := tty.String(); got != want.String() {
		t.Fatalf("writer output out of order or incomplete:\n got %q\nwant %q", got, want.String())
	}
}

// TestStreamWriterStopDrainsQueuedItems asserts stop drains the item already handed
// off before the writer exits, closes done, and never writes after it returns, so a
// teardown loses no in-flight frame and leaves no goroutine writing the terminal.
func TestStreamWriterStopDrainsQueuedItems(t *testing.T) {
	tty := &slowWriter{delay: time.Millisecond}
	w := newStreamWriter(tty)

	b0, b1 := &bytes.Buffer{}, &bytes.Buffer{}
	b0.WriteString("A")
	b1.WriteString("B")
	w.send(writeItem{buf: b0})
	w.send(writeItem{buf: b1})
	w.stop()

	tty.markClosed()
	time.Sleep(5 * time.Millisecond)

	if got := tty.String(); got != "AB" {
		t.Fatalf("stop did not drain queued items: got %q, want %q", got, "AB")
	}
	if tty.wroteAfterClose() {
		t.Fatal("writer wrote the terminal after stop returned")
	}
	select {
	case <-w.done:
	default:
		t.Fatal("writer done channel not closed after stop")
	}
}

// TestEmitControlFlushesAssembledFirst pins the Tab/resize sequencing: a control
// write (a resize clear or a kitty reset) flushes the in-flight assembled frame
// before its own bytes, so the two never interleave and the frame lands first.
func TestEmitControlFlushesAssembledFirst(t *testing.T) {
	tty := &slowWriter{}
	w := newStreamWriter(tty)
	s := &streamSession{writer: w, tty: tty}

	ab := &bytes.Buffer{}
	ab.WriteString("FRAME")
	s.assembled = ab
	if err := s.emitControl([]byte("CTRL")); err != nil {
		t.Fatalf("emitControl: %v", err)
	}
	w.stop()

	if got := tty.String(); got != "FRAMECTRL" {
		t.Fatalf("emitControl order = %q, want FRAMECTRL (assembled frame before control)", got)
	}
	if s.assembled != nil {
		t.Fatal("emitControl left the assembled buffer set")
	}
}

// TestStreamLoopDropsStaleFramesUnderSlowWriter asserts the producer never blocks
// its ingest on a slow writer: it keeps draining Frames() and holds only the newest,
// so a burst of frames drops the stale ones (latest-wins) rather than queueing
// unboundedly, and the loop still quits promptly.
func TestStreamLoopDropsStaleFramesUnderSlowWriter(t *testing.T) {
	tty := &slowWriter{delay: 3 * time.Millisecond}
	frames := make(chan render.Frame)
	eng := &pipeEngine{frames: frames}

	st := NewState(4, 2, 8, 8)
	st.Backend = BackendBlock
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	s := &streamSession{eng: eng, st: &st, tty: tty, diff: diff}

	keys := make(chan keyEvent, 1)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = s.loop(nil, keys)
	}()

	const pushed = 40
	for i := range pushed {
		frames <- solidImageFrame(uint8(i))
	}
	keys <- keyEvent{b: 'q'}

	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not quit: the producer blocked on the slow writer")
	}

	if got := tty.count(); got >= pushed {
		t.Fatalf("writer presented %d of %d frames; the drop policy did not shed stale frames", got, pushed)
	}
	if tty.count() == 0 {
		t.Fatal("writer presented no frames at all")
	}
}

// TestStreamLoopTeardownAfterTabAndWinch drives the real loop through a kitty draw,
// a Tab away from kitty (which routes the kitty reset through the writer), a resize
// signal, and a quit, and asserts the loop returns without deadlock and the reset
// reached the terminal. It guards the teardown and control-write sequencing on the
// live path.
func TestStreamLoopTeardownAfterTabAndWinch(t *testing.T) {
	out := &syncBuf{}
	eng := &tabCycleEngine{frames: make(chan render.Frame, 4)}

	st := NewState(8, 4, 16, 16)
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	done := make(chan struct{})
	ctl := newControl(eng, done)
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl, diff: diff}

	winch := make(chan os.Signal, 1)
	keys := make(chan keyEvent, 3)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- s.loop(winch, keys)
	}()

	// Draw one kitty frame so a kitty image is placed (kittyID != 0), so the Tab away
	// actually emits a reset.
	eng.frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "\x1b_G") }) {
		t.Fatalf("no kitty graphics escape before Tab; output = %q", out.String())
	}

	// Tab away from kitty: the reset (a=d delete-image) must route through the writer.
	keys <- keyEvent{b: keyTab}
	if !waitFor(3*time.Second, func() bool {
		for _, n := range eng.backends() {
			if n == "block" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("Tab did not switch backend; got %v", eng.backends())
	}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "a=d") }) {
		t.Fatalf("kitty reset (a=d) did not reach the terminal after Tab; output = %q", out.String())
	}

	// A resize signal must not deadlock even when GetSize fails on the unset fd.
	winch <- syscall.SIGWINCH
	keys <- keyEvent{b: 'q'}

	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("loop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return after Tab, winch, and quit (deadlock)")
	}
	close(done)
}

// solidImageFrame builds a small solid-colour block-backend frame whose colour
// tags its identity, for the drop-policy burst.
func solidImageFrame(id uint8) render.Frame {
	return render.Frame{Img: solidImageSize(8, 8, color.RGBA{R: id, G: id, B: id, A: 0xff})}
}
