package viewer

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"runtime"
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

func (e *pipeEngine) SetBackend(Backend)          {}
func (e *pipeEngine) SetCellPx(int, int)          {}
func (e *pipeEngine) Resize(int, int)             {}
func (e *pipeEngine) Navigate(string) error       { return nil }
func (e *pipeEngine) Frames() <-chan render.Frame { return e.frames }
func (e *pipeEngine) SendKey(string)              {}
func (e *pipeEngine) SendText(string)             {}
func (e *pipeEngine) SendPaste(string) bool       { return false }
func (e *pipeEngine) SendNav(NavAction)           {}
func (e *pipeEngine) CurrentURL() string          { return "" }
func (e *pipeEngine) CurrentTitle() string        { return "" }
func (e *pipeEngine) OpenURL(string)              {}
func (e *pipeEngine) BrowserVersion() string      { return "" }
func (e *pipeEngine) Find(string)                 {}
func (e *pipeEngine) FindNext()                   {}
func (e *pipeEngine) FindPrev()                   {}
func (e *pipeEngine) FindClear()                  {}
func (e *pipeEngine) FindState() FindState        { return FindState{} }
func (e *pipeEngine) Selection() (string, bool)   { return "", false }
func (e *pipeEngine) SendMouse(MouseEvent)        {}
func (e *pipeEngine) Close() error                { return nil }

// shmPipeEngine records the optional Kitty shm decode mode while exposing the
// same scripted frame channel as pipeEngine.
type shmPipeEngine struct {
	pipeEngine
	kittyDecode bool
	keyC        chan string
}

func (e *shmPipeEngine) SetKittyDecode(on bool) { e.kittyDecode = on }

func (e *shmPipeEngine) SendKey(code string) {
	if e.keyC != nil {
		e.keyC <- code
	}
}

func assertNoKittyDirectTransmit(t *testing.T, out string) {
	t.Helper()
	for _, cmd := range kittyCommands(out) {
		if hasOpt(cmd, "f=100") && hasOpt(cmd, "t=d") {
			t.Fatalf("SHM pipeline emitted a direct PNG transmit: %q", cmd)
		}
	}
}

func assertKittySHMObjectsUnlinked(t *testing.T, shm *testSHMWriter) {
	t.Helper()
	for _, name := range shm.names {
		path := "/dev/shm/" + strings.TrimPrefix(name, "/")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SHM object %s survived stream cleanup (stat err %v)", name, err)
		}
	}
}

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

// TestStreamWriterMeasuresTmuxSixelWriteDuration pins the seam the adaptive tmux
// pacing gate feeds on: a tmux Sixel item returns its write duration on free even
// with the fps meter off (timed false), while a plain untimed item pays no clock
// read and returns none.
func TestStreamWriterMeasuresTmuxSixelWriteDuration(t *testing.T) {
	const delay = 20 * time.Millisecond
	tty := &slowWriter{delay: delay}
	w := newStreamWriter(tty)

	buf := bytes.NewBufferString("frame")
	w.send(writeItem{buf: buf, tmuxSixel: true})
	fi := <-w.free
	if !fi.tmuxSixel {
		t.Fatal("free item lost the tmux Sixel tag")
	}
	if fi.writeDur < delay {
		t.Fatalf("writeDur = %v, want at least the %v tty delay", fi.writeDur, delay)
	}
	if fi.writeMS != 0 {
		t.Fatalf("untimed item recorded writeMS = %v for the fps meter, want 0", fi.writeMS)
	}

	w.send(writeItem{buf: buf})
	fi = <-w.free
	if fi.writeDur != 0 || fi.tmuxSixel {
		t.Fatalf("plain item returned writeDur = %v, tmuxSixel = %v, want zero and false", fi.writeDur, fi.tmuxSixel)
	}
	w.stop()
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

// TestStreamWriterDrainsFrameBeforeTerminalExit pins teardown ordering across the
// writer and Stream seams. The queued frame, including its synchronised-update
// close, must drain before the shared terminal cleanup. Once cleanup lands, the
// stopped writer must emit no trailing bytes.
func TestStreamWriterDrainsFrameBeforeTerminalExit(t *testing.T) {
	tty := &slowWriter{delay: time.Millisecond}
	w := newStreamWriter(tty)

	frame := &bytes.Buffer{}
	frame.WriteString("FRAME")
	frame.WriteString(endSyncUpdate)
	w.send(writeItem{buf: frame})
	w.stop()

	cleanup := exitTerminal(24, true, false)
	if _, err := tty.Write([]byte(cleanup)); err != nil {
		t.Fatalf("write terminal cleanup: %v", err)
	}
	want := "FRAME" + endSyncUpdate + cleanup
	if got := tty.String(); got != want {
		t.Fatalf("teardown order = %q, want %q", got, want)
	}

	tty.markClosed()
	time.Sleep(5 * time.Millisecond)
	if got := tty.String(); got != want {
		t.Fatalf("writer emitted bytes after terminal cleanup: got %q, want %q", got, want)
	}
	if tty.wroteAfterClose() {
		t.Fatal("writer wrote the terminal after cleanup")
	}
}

// TestAssembleSixelUsesNonScrollingDisplayMode pins the live DECSDM placement
// wrap. A cursor restore after the DCS is too late: a scrolling full-height
// Sixel has already shifted the viewport before that restore is processed.
func TestAssembleSixelUsesNonScrollingDisplayMode(t *testing.T) {
	st := NewState(1, 1)
	st.Backend = BackendSixel
	s := &streamSession{st: &st}
	frame := render.Frame{Img: solidImageSize(8, 16, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})}

	var assembled bytes.Buffer
	if _, err := s.assemble(&assembled, frame); err != nil {
		t.Fatalf("assemble Sixel frame: %v", err)
	}
	out := assembled.Bytes()
	if bytes.Count(out, []byte("\x1bP")) != 1 || bytes.Count(out, []byte("\x1b\\")) != 1 {
		t.Fatalf("assembled frame does not contain exactly one Sixel DCS: %q", out)
	}
	wantPrefix := beginSyncUpdate + clearScreen + beginSixelDisplayMode + "\x1bP"
	if !bytes.HasPrefix(out, []byte(wantPrefix)) {
		t.Fatalf("Sixel frame does not enter display mode before its DCS: %q", out)
	}
	dcsStart := bytes.Index(out, []byte("\x1bP"))
	dcsEndOffset := bytes.Index(out[dcsStart:], []byte("\x1b\\"))
	if dcsEndOffset < 0 {
		t.Fatalf("Sixel DCS has no terminator: %q", out)
	}
	dcsEnd := dcsStart + dcsEndOffset + len("\x1b\\")
	if len(out) < dcsEnd+len(endSixelDisplayMode) || string(out[dcsEnd:dcsEnd+len(endSixelDisplayMode)]) != endSixelDisplayMode {
		t.Fatalf("Sixel DCS is not followed by the DECSDM reset: %q", out)
	}
	if !bytes.HasSuffix(out, []byte(endSixelDisplayMode+endSyncUpdate)) {
		t.Fatalf("Sixel display-mode reset is not immediately before the synchronised-update close: %q", out)
	}
	if bytes.ContainsAny(out, "\r\n") {
		t.Fatalf("assembled Sixel frame contains a newline: %q", out)
	}
	if bytes.Contains(out, []byte("\x1b7")) || bytes.Contains(out, []byte("\x1b8")) {
		t.Fatalf("assembled Sixel frame relies on cursor save/restore: %q", out)
	}
}

// TestAssembleFootSixelOmitsOnlySyncUpdates compares the normal and Foot Sixel
// production bytes. Removing the two DEC 2026 markers from the normal frame must
// produce the Foot frame exactly, preserving cursor placement, DECSDM, the Sixel
// DCS, and every payload byte.
func TestAssembleFootSixelOmitsOnlySyncUpdates(t *testing.T) {
	frame := render.Frame{Img: solidImageSize(8, 16, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})}

	normalState := NewState(1, 1)
	normalState.Backend = BackendSixel
	var normal bytes.Buffer
	if _, err := (&streamSession{st: &normalState}).assemble(&normal, frame); err != nil {
		t.Fatalf("assemble normal Sixel frame: %v", err)
	}

	footState := NewState(1, 1)
	footState.Backend = BackendSixel
	footState.setTerminalCompatibility("foot(1.27.0)", nil)
	var foot bytes.Buffer
	if _, err := (&streamSession{st: &footState}).assemble(&foot, frame); err != nil {
		t.Fatalf("assemble Foot Sixel frame: %v", err)
	}

	want := bytes.ReplaceAll(normal.Bytes(), []byte(beginSyncUpdate), nil)
	want = bytes.ReplaceAll(want, []byte(endSyncUpdate), nil)
	if !bytes.Equal(foot.Bytes(), want) {
		t.Fatalf("Foot Sixel frame changed beyond DEC 2026 markers: got %q, want %q", foot.Bytes(), want)
	}
	if bytes.Contains(foot.Bytes(), []byte(beginSyncUpdate)) || bytes.Contains(foot.Bytes(), []byte(endSyncUpdate)) {
		t.Fatalf("Foot Sixel frame retains DEC 2026 markers: %q", foot.Bytes())
	}
	if !bytes.Contains(foot.Bytes(), []byte(beginSixelDisplayMode+"\x1bP")) ||
		!bytes.HasSuffix(foot.Bytes(), []byte(endSixelDisplayMode)) {
		t.Fatalf("Foot Sixel frame lost DECSDM placement: %q", foot.Bytes())
	}
}

// TestFootIdentityScopesSyncUpdateAssembly drives the same identity setter as
// startup detection through real frame assembly. Under one Foot XTVERSION reply,
// Sixel omits DEC 2026 while Block and Kitty retain both fence markers.
func TestFootIdentityScopesSyncUpdateAssembly(t *testing.T) {
	colour := color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff}
	cases := []struct {
		name      string
		backend   Backend
		frame     render.Frame
		wantFence bool
	}{
		{"Sixel", BackendSixel, render.Frame{Img: solidImageSize(8, 16, colour)}, false},
		{"Block", BackendBlock, render.Frame{Img: solidImageSize(2, 2, colour)}, true},
		{"Kitty", BackendKitty, render.Frame{EncodedPNG: encodedPNG(t, solidImageSize(8, 8, colour))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(1, 1)
			st.setTerminalCompatibility("foot(1.27.0)", nil)
			st.Backend = tc.backend
			if tc.backend == BackendBlock {
				st.SetDiff(&block.GridDiff{})
			}

			var out bytes.Buffer
			if _, err := (&streamSession{st: &st}).assemble(&out, tc.frame); err != nil {
				t.Fatalf("assemble Foot %s frame: %v", tc.backend, err)
			}
			hasOpen := bytes.HasPrefix(out.Bytes(), []byte(beginSyncUpdate))
			hasClose := bytes.HasSuffix(out.Bytes(), []byte(endSyncUpdate))
			if hasOpen != tc.wantFence || hasClose != tc.wantFence {
				t.Fatalf("Foot %s DEC 2026 fence: open=%v close=%v, want both %v; output %q",
					tc.backend, hasOpen, hasClose, tc.wantFence, out.Bytes())
			}
		})
	}
}

// TestEmitControlFlushesAssembledFirst pins the cycle/resize sequencing: a control
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

// TestResizeResetInvalidatesAssembledKittySHMTransmit pins the resize control
// order where reset unlinks an assembled frame's object before emitControl
// flushes its t=s escape. The resulting terminal failure is correlated with
// the invalidated frame and does not arm or settle another pacing gate.
func TestResizeResetInvalidatesAssembledKittySHMTransmit(t *testing.T) {
	tty := &slowWriter{}
	w := newStreamWriter(tty)
	st := shmState()
	st.kittyID = 1
	s := &streamSession{st: &st, writer: w, tty: tty}

	assembled := &bytes.Buffer{}
	assembled.WriteString("SHMFRAME")
	s.assembled = assembled
	s.assembledSHM = kittySHMFrame{id: 1, generation: s.kittySHMGeneration}

	var ctl bytes.Buffer
	if err := s.resetKitty(&ctl); err != nil {
		t.Fatalf("resize Kitty reset: %v", err)
	}
	ctl.WriteString("RESIZE")
	if err := s.emitControl(ctl.Bytes()); err != nil {
		t.Fatalf("emit resize control: %v", err)
	}
	w.stop()

	out := tty.String()
	frameAt := strings.Index(out, "SHMFRAME")
	resetAt := strings.Index(out, "a=d")
	if frameAt < 0 || resetAt < 0 || frameAt >= resetAt {
		t.Fatalf("resize output order = %q, want assembled SHM frame before reset", out)
	}
	if s.kittySHMGated() {
		t.Fatal("resize-invalidated assembled transmit armed the pacing gate")
	}
	if err := s.handleKittyResponse("i=1;ENOENT:failed to open shared memory object"); err != nil {
		t.Fatalf("resize-invalidated failure returned error: %v", err)
	}
	if s.kittySHMGated() {
		t.Fatal("resize-invalidated reply changed the pacing gate")
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

	st := NewState(4, 2)
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
	keys <- keyEvent{b: keyQuitCtrlQ}

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

// TestStreamLoopTeardownAfterCycleAndWinch drives the real loop through a kitty draw,
// a backend cycle away from kitty (which routes the kitty reset through the writer), a resize
// signal, and a quit, and asserts the loop returns without deadlock and the reset
// reached the terminal. It guards the teardown and control-write sequencing on the
// live path.
func TestStreamLoopTeardownAfterCycleAndWinch(t *testing.T) {
	out := &syncBuf{}
	eng := &tabCycleEngine{frames: make(chan render.Frame, 4)}

	st := NewState(8, 4)
	eng.kittyPNG = encodedPNG(t, image.NewRGBA(image.Rect(0, 0, 16, 16)))
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

	// Draw one kitty frame so a kitty image is placed (kittyID != 0), so the cycle away
	// actually emits a reset.
	eng.frames <- render.Frame{EncodedPNG: eng.kittyPNG}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "\x1b_G") }) {
		t.Fatalf("no kitty graphics escape before the cycle; output = %q", out.String())
	}

	// Cycle away from kitty: the reset (a=d delete-image) must route through the writer.
	keys <- keyEvent{act: actionCycleBackend}
	if !waitFor(3*time.Second, func() bool {
		for _, n := range eng.backends() {
			if n == "block" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("Alt+R did not switch backend; got %v", eng.backends())
	}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "a=d") }) {
		t.Fatalf("kitty reset (a=d) did not reach the terminal after the cycle; output = %q", out.String())
	}

	// A resize signal must not deadlock even when GetSize fails on the unset fd.
	winch <- syscall.SIGWINCH
	keys <- keyEvent{b: keyQuitCtrlQ}

	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("loop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return after cycle, winch, and quit (deadlock)")
	}
	close(done)
}

// solidImageFrame builds a small solid-colour block-backend frame whose colour
// tags its identity, for the drop-policy burst.
func solidImageFrame(id uint8) render.Frame {
	return render.Frame{Img: solidImageSize(8, 8, color.RGBA{R: id, G: id, B: id, A: 0xff})}
}

// errWriter is a tty stand-in whose Write returns err on and after the failAt call,
// so a test drives a real write error through the writer goroutine and asserts loop
// surfaces it. Its counter is guarded, so the writer goroutine may touch it under
// -race.
type errWriter struct {
	mu     sync.Mutex
	writes int
	failAt int
	err    error
}

func (w *errWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes >= w.failAt {
		return 0, w.err
	}
	return len(p), nil
}

// captureErrWriter records the bytes a failed terminal write attempted, then
// returns its configured error.
type captureErrWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	err    error
	writes int
	failAt int
	wrote  chan int
}

func (w *captureErrWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.buf.Write(p)
	w.writes++
	writes := w.writes
	fail := w.failAt == 0 || writes >= w.failAt
	w.mu.Unlock()
	if w.wrote != nil {
		w.wrote <- writes
	}
	if fail {
		return 0, w.err
	}
	return len(p), nil
}

func (w *captureErrWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// blockingErrWriter records one write, then lets the test change producer
// state before releasing the write with an error.
type blockingErrWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	err     error
	started chan struct{}
	release chan struct{}
}

func (w *blockingErrWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.buf.Write(p)
	w.mu.Unlock()
	close(w.started)
	<-w.release
	return 0, w.err
}

func (w *blockingErrWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// shortWriter is a tty stand-in whose Write returns one byte short with a nil error,
// the io.Writer contract the production *os.File never trips. It verifies that
// the writer treats n < len(p) as a failure.
type shortWriter struct{}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

// feedFrames pushes identical frames into ch until stop closes, so a loop test keeps
// the producer assembling while it waits for a write error to surface.
func feedFrames(ch chan<- render.Frame, stop <-chan struct{}) {
	for {
		select {
		case ch <- solidImageFrame(1):
		case <-stop:
			return
		}
	}
}

// TestStreamLoopReturnsWriteError drives the real loop against a tty that errors on
// its third write and asserts loop returns that exact error. The writer goroutine
// exits with it: loop's teardown defer calls w.stop, which joins the goroutine, so a
// returned loop proves no writer leak.
func TestStreamLoopReturnsWriteError(t *testing.T) {
	wantErr := errors.New("tty write failed")
	tty := &errWriter{failAt: 3, err: wantErr}
	frames := make(chan render.Frame)
	eng := &pipeEngine{frames: frames}

	st := NewState(4, 2)
	st.Backend = BackendBlock
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	s := &streamSession{eng: eng, st: &st, tty: tty, diff: diff}

	keys := make(chan keyEvent)
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, keys) }()

	stop := make(chan struct{})
	go feedFrames(frames, stop)
	defer close(stop)

	select {
	case err := <-loopDone:
		if !errors.Is(err, wantErr) {
			t.Fatalf("loop returned %v, want unchanged %v", err, wantErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not surface the write error")
	}
}

// TestStreamLoopKittySHMAssemblyErrorReleasesOnce asserts a synchronous SHM
// assembly failure closes and unlinks its writer, returns the source error,
// releases the transferred plane once, and never uses retained direct PNG.
func TestStreamLoopKittySHMAssemblyErrorReleasesOnce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Kitty SHM cleanup test requires Linux /dev/shm")
	}
	wantErr := errors.New("forced SHM object close failure")
	shm := newTestSHMWriter()
	shm.objectCloseErr = wantErr
	frames := make(chan render.Frame, 1)
	eng := &shmPipeEngine{pipeEngine: pipeEngine{frames: frames}}
	st := shmState()
	st.kittySHM = shm
	defer st.closeKittySHM()
	syncEngineKittyDecode(eng, &st)
	out := &syncBuf{}
	s := &streamSession{eng: eng, st: &st, tty: out}

	released := 0
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frames <- render.Frame{Img: img, EncodedPNG: encodedPNG(t, img), Release: func() { released++ }}
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, make(chan keyEvent)) }()

	var err error
	select {
	case err = <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return after the Kitty SHM assembly error")
	}

	if !errors.Is(err, wantErr) {
		t.Fatalf("loop returned %v, want %v", err, wantErr)
	}
	if released != 1 {
		t.Fatalf("failed assembled plane released %d times, want 1", released)
	}
	gotCleanup := exitTerminal(st.Rows, s.firstDrawn.Load(), false)
	wantCleanup := exitTerminal(st.Rows, false, false)
	if gotCleanup != wantCleanup {
		t.Fatalf("failed first-frame teardown = %q, want pre-frame cleanup %q", gotCleanup, wantCleanup)
	}
	if !st.kittySHMLive() || !eng.kittyDecode {
		t.Fatal("assembly failure changed SHM transport or engine decode state")
	}
	if !shm.closed || st.kittySHM != nil {
		t.Fatal("assembly failure did not transfer SHM cleanup ownership")
	}
	assertKittySHMObjectsUnlinked(t, shm)
	assertNoKittyDirectTransmit(t, out.String())
}

// TestStreamLoopKittySHMWriteErrorStopsWithoutDirect asserts the writer error
// channel remains the terminal write failure path after an SHM transmit. The
// stream owner retains cleanup responsibility and the decode mode stays on.
func TestStreamLoopKittySHMWriteErrorStopsWithoutDirect(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Kitty SHM cleanup test requires Linux /dev/shm")
	}
	wantErr := errors.New("forced terminal write failure")
	out := &captureErrWriter{err: wantErr}
	shm := newTestSHMWriter()
	frames := make(chan render.Frame, 1)
	eng := &shmPipeEngine{pipeEngine: pipeEngine{frames: frames}}
	st := shmState()
	st.kittySHM = shm
	defer st.closeKittySHM()
	syncEngineKittyDecode(eng, &st)
	s := &streamSession{eng: eng, st: &st, tty: out}

	released := 0
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frames <- render.Frame{Img: img, EncodedPNG: encodedPNG(t, img), Release: func() { released++ }}
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, make(chan keyEvent)) }()

	var err error
	select {
	case err = <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return after the Kitty SHM write error")
	}

	if !errors.Is(err, wantErr) {
		t.Fatalf("loop returned %v, want %v", err, wantErr)
	}
	if got, want := err.Error(), "write Kitty shm frame: "+wantErr.Error(); got != want {
		t.Fatalf("loop error = %q, want %q", got, want)
	}
	if released != 1 {
		t.Fatalf("written plane released %d times, want 1", released)
	}
	if !st.kittySHMLive() || !eng.kittyDecode {
		t.Fatal("terminal write failure changed SHM transport or engine decode state")
	}
	if shm.closed || st.kittySHM == nil {
		t.Fatal("loop took SHM cleanup ownership from the stream owner")
	}
	assertNoKittyDirectTransmit(t, out.String())
	st.closeKittySHM()
	if !shm.closed || st.kittySHM != nil {
		t.Fatal("stream-owner cleanup did not close the SHM writer")
	}
	assertKittySHMObjectsUnlinked(t, shm)
}

// TestStreamLoopControlWriteErrorWithKittySHMAwaiting keeps an SHM
// acknowledgement outstanding while a later bar repaint fails. The control
// write must keep its unwrapped terminal error despite the unrelated SHM gate.
func TestStreamLoopControlWriteErrorWithKittySHMAwaiting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Kitty SHM cleanup test requires Linux /dev/shm")
	}
	wantErr := errors.New("forced control write failure")
	out := &captureErrWriter{err: wantErr, failAt: 2, wrote: make(chan int, 2)}
	shm := newTestSHMWriter()
	frames := make(chan render.Frame, 1)
	eng := &shmPipeEngine{pipeEngine: pipeEngine{frames: frames}}
	st := shmState()
	st.kittySHM = shm
	defer st.closeKittySHM()
	syncEngineKittyDecode(eng, &st)
	outcomes := make(chan inputOutcome, 1)
	s := &streamSession{eng: eng, st: &st, tty: out, outcomes: outcomes}

	released := 0
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frames <- render.Frame{Img: img, EncodedPNG: encodedPNG(t, img), Release: func() { released++ }}
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, make(chan keyEvent)) }()

	select {
	case writes := <-out.wrote:
		if writes != 1 {
			t.Fatalf("first completed write = %d, want SHM frame write 1", writes)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SHM frame write did not complete")
	}
	outcomes <- inputOutcome{kind: signalCopyRefused}
	select {
	case writes := <-out.wrote:
		if writes != 2 {
			t.Fatalf("failed write = %d, want control write 2", writes)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control write did not fail")
	}

	select {
	case err := <-loopDone:
		if !errors.Is(err, wantErr) {
			t.Fatalf("loop returned %v, want %v", err, wantErr)
		}
		if got := err.Error(); got != wantErr.Error() {
			t.Fatalf("loop error = %q, want unwrapped control error %q", got, wantErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return the control write error")
	}
	if released != 1 {
		t.Fatalf("control failure released the SHM plane %d times, want 1", released)
	}
	if !st.kittySHMLive() || !eng.kittyDecode {
		t.Fatal("control failure changed SHM transport or engine decode state")
	}
	if shm.closed || st.kittySHM == nil {
		t.Fatal("control failure took SHM cleanup ownership from the stream owner")
	}
	assertNoKittyDirectTransmit(t, out.String())
	st.closeKittySHM()
	if !shm.closed || st.kittySHM != nil {
		t.Fatal("stream-owner cleanup did not close the SHM writer")
	}
	assertKittySHMObjectsUnlinked(t, shm)
}

// TestStreamLoopKittySHMWriteErrorAfterAcknowledgement clears the pacing state
// while the SHM terminal write is blocked. The eventual failure must retain the
// failed frame's SHM context and release its ownership exactly once.
func TestStreamLoopKittySHMWriteErrorAfterAcknowledgement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Kitty SHM cleanup test requires Linux /dev/shm")
	}
	wantErr := errors.New("forced delayed SHM write failure")
	out := &blockingErrWriter{
		err: wantErr, started: make(chan struct{}), release: make(chan struct{}),
	}
	shm := newTestSHMWriter()
	frames := make(chan render.Frame, 1)
	eng := &shmPipeEngine{
		pipeEngine: pipeEngine{frames: frames},
		keyC:       make(chan string, 1),
	}
	st := shmState()
	st.kittySHM = shm
	defer st.closeKittySHM()
	syncEngineKittyDecode(eng, &st)
	s := &streamSession{eng: eng, st: &st, tty: out}
	keys := make(chan keyEvent, 2)
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, keys) }()

	released := 0
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frames <- render.Frame{Img: img, EncodedPNG: encodedPNG(t, img), Release: func() { released++ }}
	select {
	case <-out.started:
	case <-time.After(3 * time.Second):
		t.Fatal("SHM frame write did not start")
	}
	keys <- keyEvent{apc: "i=1;OK"}
	keys <- keyEvent{scroll: ScrollDown}
	select {
	case <-eng.keyC:
		// The loop handles keys in channel order, so this proves the preceding
		// acknowledgement cleared the SHM pacing state before the write failed.
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not process the event after the SHM acknowledgement")
	}
	close(out.release)

	select {
	case err := <-loopDone:
		if !errors.Is(err, wantErr) {
			t.Fatalf("loop returned %v, want %v", err, wantErr)
		}
		if got, want := err.Error(), "write Kitty shm frame: "+wantErr.Error(); got != want {
			t.Fatalf("loop error = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return the delayed SHM write error")
	}
	if released != 1 {
		t.Fatalf("delayed write failure released the SHM plane %d times, want 1", released)
	}
	if !st.kittySHMLive() || !eng.kittyDecode {
		t.Fatal("delayed write failure changed SHM transport or engine decode state")
	}
	if shm.closed || st.kittySHM == nil {
		t.Fatal("delayed write failure took SHM cleanup ownership from the stream owner")
	}
	assertNoKittyDirectTransmit(t, out.String())
	st.closeKittySHM()
	if !shm.closed || st.kittySHM != nil {
		t.Fatal("stream-owner cleanup did not close the SHM writer")
	}
	assertKittySHMObjectsUnlinked(t, shm)
}

// TestStreamLoopKittySHMRejectionStopsWithoutDirect asserts an active terminal
// rejection exits the loop, preserves SHM decode state, and leaves object
// cleanup to the stream owner without emitting retained direct PNG.
func TestStreamLoopKittySHMRejectionStopsWithoutDirect(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Kitty SHM cleanup test requires Linux /dev/shm")
	}
	out := &syncBuf{}
	shm := newTestSHMWriter()
	frames := make(chan render.Frame, 1)
	eng := &shmPipeEngine{pipeEngine: pipeEngine{frames: frames}}
	st := shmState()
	st.kittySHM = shm
	defer st.closeKittySHM()
	syncEngineKittyDecode(eng, &st)
	s := &streamSession{eng: eng, st: &st, tty: out}
	keys := make(chan keyEvent, 1)
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, keys) }()

	released := 0
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frames <- render.Frame{Img: img, EncodedPNG: encodedPNG(t, img), Release: func() { released++ }}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "t=s") }) {
		t.Fatalf("SHM transmit did not reach terminal: %q", out.String())
	}
	keys <- keyEvent{apc: "i=1;EBADF:could not open shared memory"}

	select {
	case err := <-loopDone:
		if err == nil || !strings.Contains(err.Error(), "terminal rejected shm transmit") {
			t.Fatalf("loop returned %v, want active SHM rejection", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return the SHM rejection")
	}
	if released != 1 {
		t.Fatalf("rejected plane released %d times, want 1", released)
	}
	if !st.kittySHMLive() || !eng.kittyDecode {
		t.Fatal("terminal rejection changed SHM transport or engine decode state")
	}
	if shm.closed || st.kittySHM == nil {
		t.Fatal("loop took SHM cleanup ownership from the stream owner")
	}
	assertNoKittyDirectTransmit(t, out.String())
	st.closeKittySHM()
	if !shm.closed || st.kittySHM != nil {
		t.Fatal("stream-owner cleanup did not close the SHM writer")
	}
	assertKittySHMObjectsUnlinked(t, shm)
}

// TestStreamLoopDropsStaleFramesWithFPSArmed repeats the drop-policy burst with the
// fps meter armed, so assemble stamps the meter and the free-case recordPaint runs on
// the producer goroutine concurrently with the writer's buffer frees. Under -race it
// pins that the meter stays single-goroutine and no pooled buffer aliases.
func TestStreamLoopDropsStaleFramesWithFPSArmed(t *testing.T) {
	tty := &slowWriter{delay: 3 * time.Millisecond}
	frames := make(chan render.Frame)
	eng := &pipeEngine{frames: frames}

	st := NewState(4, 2)
	st.Backend = BackendBlock
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	s := &streamSession{eng: eng, st: &st, tty: tty, diff: diff, fps: newFPSMeter([]string{"NYALA_FPS_TIMING=1"})}
	if s.fps == nil {
		t.Fatal("fps meter did not arm")
	}

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
	keys <- keyEvent{b: keyQuitCtrlQ}

	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not quit with the fps meter armed")
	}
	if tty.count() == 0 {
		t.Fatal("writer presented no frames at all")
	}
}

// TestStreamLoopControlFrameInterleaveUnderSlowTTY drives the real loop through a
// kitty frame burst, a backend cycle away from kitty (which routes the kitty reset through the
// writer amid the burst), more frames, and a quit, all against a slow tty. It asserts
// the loop quits promptly (the two blocking control sends never wedge under
// backpressure) and the reset lands after the kitty graphics escape, so a control
// write stays frame-before-control ordered and never splits a frame. Run under -race.
func TestStreamLoopControlFrameInterleaveUnderSlowTTY(t *testing.T) {
	out := &slowWriter{delay: 500 * time.Microsecond}
	eng := &tabCycleEngine{frames: make(chan render.Frame, 8)}

	st := NewState(8, 4)
	eng.kittyPNG = encodedPNG(t, image.NewRGBA(image.Rect(0, 0, 16, 16)))
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	diff := &block.GridDiff{}
	st.SetDiff(diff)

	done := make(chan struct{})
	ctl := newControl(eng, done)
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: ctl, diff: diff}

	winch := make(chan os.Signal, 1)
	keys := make(chan keyEvent, 3)
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(winch, keys) }()

	// Burst of kitty frames, then wait for a kitty graphics escape to reach the tty.
	for range 6 {
		eng.frames <- render.Frame{EncodedPNG: eng.kittyPNG}
	}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "\x1b_G") }) {
		t.Fatalf("no kitty graphics escape before the cycle; output = %q", out.String())
	}

	// Cycle away from kitty: the reset (a=d delete-image) routes through the writer amid
	// the burst and must land after the already-sent frames.
	keys <- keyEvent{act: actionCycleBackend}
	if !waitFor(3*time.Second, func() bool { return strings.Contains(out.String(), "a=d") }) {
		t.Fatalf("kitty reset (a=d) did not reach the terminal after the cycle; output = %q", out.String())
	}

	// A few more frames after the control write, then quit.
	for range 4 {
		eng.frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	}
	keys <- keyEvent{b: keyQuitCtrlQ}

	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("loop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not quit under a slow tty with an interleaved control write")
	}
	close(done)

	got := out.String()
	if g, r := strings.Index(got, "\x1b_G"), strings.Index(got, "a=d"); g < 0 || r < 0 || g > r {
		t.Fatalf("control write out of order: graphics escape at %d, reset at %d (want escape first)", g, r)
	}
}

// TestStreamWriterShortWriteFails verifies at the writer seam that a tty
// returning n < len(p) with a nil error must surface io.ErrShortWrite on errc and
// still free the pooled buffer, so a torn frame never passes silently.
func TestStreamWriterShortWriteFails(t *testing.T) {
	w := newStreamWriter(&shortWriter{})
	b := &bytes.Buffer{}
	b.WriteString("FRAME")
	w.send(writeItem{buf: b})

	select {
	case err := <-w.errc:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("errc = %v, want io.ErrShortWrite", err)
		}
	case <-time.After(time.Second):
		t.Fatal("short write not reported on errc")
	}
	select {
	case <-w.free:
	case <-time.After(time.Second):
		t.Fatal("writer did not free the buffer after a short write")
	}
	w.stop()
}

// TestStreamLoopReturnsShortWriteError drives the real loop against a tty that
// always short-writes and asserts loop returns io.ErrShortWrite, preventing a torn
// frame from passing as a successful write.
func TestStreamLoopReturnsShortWriteError(t *testing.T) {
	frames := make(chan render.Frame)
	eng := &pipeEngine{frames: frames}

	st := NewState(4, 2)
	st.Backend = BackendBlock
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	s := &streamSession{eng: eng, st: &st, tty: &shortWriter{}, diff: diff}

	keys := make(chan keyEvent)
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, keys) }()

	stop := make(chan struct{})
	go feedFrames(frames, stop)
	defer close(stop)

	select {
	case err := <-loopDone:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("loop returned %v, want io.ErrShortWrite", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not surface the short-write error")
	}
}
