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

// tabCycleEngine drives the streamSession end to end for the backend-cycle
// test. It records every SetBackend name and, on each SetBackend, pushes one
// fresh frame so the drawing loop repaints with the newly selected backend,
// mimicking the real engine's restartStream re-emit.
type tabCycleEngine struct {
	frames   chan render.Frame
	kittyPNG string

	mu    sync.Mutex
	names []string
}

func (e *tabCycleEngine) SetBackend(backend Backend) {
	e.mu.Lock()
	e.names = append(e.names, backend.String())
	e.mu.Unlock()
	// The real engine restarts the screencast with the selected backend's frame
	// representation. Mirror that so the loop repaints through the production seam.
	if backend == BackendKitty {
		e.frames <- render.Frame{EncodedPNG: e.kittyPNG}
		return
	}
	e.frames <- render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
}

func (e *tabCycleEngine) backends() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.names...)
}

func (e *tabCycleEngine) Frames() <-chan render.Frame { return e.frames }
func (e *tabCycleEngine) SetCellPx(int, int)          {}
func (e *tabCycleEngine) Resize(int, int)             {}
func (e *tabCycleEngine) Navigate(string) error       { return nil }
func (e *tabCycleEngine) SendKey(string)              {}
func (e *tabCycleEngine) SendText(string)             {}
func (e *tabCycleEngine) SendPaste(string) bool       { return false }
func (e *tabCycleEngine) SendNav(NavAction)           {}
func (e *tabCycleEngine) CurrentURL() string          { return "" }
func (e *tabCycleEngine) CurrentTitle() string        { return "" }
func (e *tabCycleEngine) OpenURL(string)              {}
func (e *tabCycleEngine) BrowserVersion() string      { return "" }
func (e *tabCycleEngine) Find(string)                 {}
func (e *tabCycleEngine) FindNext()                   {}
func (e *tabCycleEngine) FindPrev()                   {}
func (e *tabCycleEngine) FindClear()                  {}
func (e *tabCycleEngine) FindState() FindState        { return FindState{} }
func (e *tabCycleEngine) Selection() (string, bool)   { return "", false }
func (e *tabCycleEngine) SendMouse(MouseEvent)        {}
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

// TestStreamAltRCyclesBackend drives the real streamSession.loop through an
// Alt+R press and asserts the on-screen backend actually flips. It starts on
// kitty, presses Alt+R, and checks that the engine received a SetBackend for
// the next backend and that the state advanced to block. This locks the whole
// backend-cycle wiring through streamSession.handleKey, including the engine
// restart and repaint at the new backend.
func TestStreamAltRCyclesBackend(t *testing.T) {
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

	st := NewState(8, 4)
	eng.kittyPNG = encodedPNG(t, image.NewRGBA(image.Rect(0, 0, 16, 16)))
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

	// Draw one kitty frame first, so the pre-cycle output carries a kitty escape.
	eng.frames <- render.Frame{EncodedPNG: eng.kittyPNG}
	if !waitFor(2*time.Second, func() bool { return strings.Contains(out.String(), "\x1b_G") }) {
		t.Fatalf("no kitty graphics escape before Alt+R; output = %q", out.String())
	}

	// Press Alt+R: it must cycle the state to block and submit a SetBackend.
	keys <- keyEvent{act: actionCycleBackend}

	if !waitFor(2*time.Second, func() bool {
		for _, n := range eng.backends() {
			if n == "block" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("Alt+R did not submit SetBackend(block); got %v", eng.backends())
	}
	if st.Backend != BackendBlock {
		t.Fatalf("after Alt+R, state backend = %d, want BackendBlock", st.Backend)
	}

	// Quit the loop and stop the reader.
	keys <- keyEvent{b: keyQuitCtrlQ}
	<-loopDone
	close(done)
	_ = pw.Close()
	<-drained
	_ = pr.Close()
}

func TestBackendCycleUsesActiveFrameRepresentation(t *testing.T) {
	st := NewState(8, 4)
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	st.SetDiff(&block.GridDiff{})
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	pngData := encodedPNG(t, img)

	st.CycleBackend()
	if st.Backend != BackendBlock {
		t.Fatalf("backend after first cycle = %d, want Block", st.Backend)
	}
	var blockOut bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{EncodedPNG: pngData}, &blockOut); err != nil {
		t.Fatalf("render queued Kitty PNG through Block: %v", err)
	}
	if blockOut.Len() == 0 {
		t.Fatal("queued Kitty PNG produced no Block output")
	}

	st.CycleBackend()
	if st.Backend != BackendKitty {
		t.Fatalf("backend after second cycle = %d, want Kitty", st.Backend)
	}
	var stale bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: img}, &stale); err != nil {
		t.Fatalf("render stale pixel frame through Kitty: %v", err)
	}
	if stale.Len() != 0 {
		t.Fatalf("stale pixel frame wrote %d bytes through Kitty, want 0", stale.Len())
	}
	var kittyOut bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{EncodedPNG: pngData}, &kittyOut); err != nil {
		t.Fatalf("render restarted direct PNG through Kitty: %v", err)
	}
	if !strings.Contains(kittyOut.String(), "f=100") || !strings.Contains(kittyOut.String(), "i=1") {
		t.Fatalf("restarted Kitty output lacks direct PNG under id 1: %q", kittyOut.String())
	}
}

// TestFPSBlockToKittyClearResetsSGRBeforeErase pins the byte order that keeps
// the first Kitty frame visible after a Block-to-Kitty cycle with the FPS
// overlay armed. The Block differ leaves the last cell's SGR background
// active, and terminals erase with the current background (background colour
// erase), so an unreset ESC[2J fills every cell with a non-default
// background. The overlay places the page image at kittyFPSPageZ, below
// non-default backgrounds, so those cells occlude every frame after the
// switch: a black screen on Kitty and Ghostty. The switch clear must reset
// the SGR before it erases.
func TestFPSBlockToKittyClearResetsSGRBeforeErase(t *testing.T) {
	st := NewState(8, 4)
	st.SetEnabled([]Backend{BackendBlock, BackendKitty})
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	// Draw one Block frame so the differ emits SGR colours, leaving a
	// non-default background active, as after any live Block frame.
	var blockOut bytes.Buffer
	if err := st.RenderStreamFrame(render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}, &blockOut); err != nil {
		t.Fatalf("seed Block frame: %v", err)
	}
	if !strings.Contains(blockOut.String(), "48;2;") {
		t.Fatalf("seed Block frame emitted no background SGR: %q", blockOut.String())
	}

	out := &syncBuf{}
	eng := &pipeEngine{frames: make(chan render.Frame, 1)}
	done := make(chan struct{})
	defer close(done)
	ctl := newControl(eng, done)
	writer := newStreamWriter(out)
	s := &streamSession{
		eng: eng, st: &st, tty: out, ctl: ctl, diff: diff, writer: writer,
		fps: &fpsMeter{window: fpsWindow, start: time.Now()},
	}
	s.firstDrawn.Store(true)
	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendKitty {
		t.Fatalf("backend after Alt+R = %v, want Kitty", st.Backend)
	}
	writer.stop()
	s.writer = nil

	got := out.String()
	clearAt := strings.Index(got, clearScreen)
	if clearAt < 0 {
		t.Fatalf("Block-to-Kitty switch emitted no clear: %q", got)
	}
	if !strings.HasSuffix(got[:clearAt], "\x1b[0m") {
		t.Fatalf("Block-to-Kitty clear erases with the Block SGR background still active: %q", got)
	}
}

func TestFPSBlockToKittyClearsBeforeFrameAndResetsDiff(t *testing.T) {
	st := NewState(8, 4)
	st.SetEnabled([]Backend{BackendBlock, BackendKitty})
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	blockFrame := render.Frame{Img: image.NewRGBA(image.Rect(0, 0, 16, 16))}
	if err := st.RenderStreamFrame(blockFrame, io.Discard); err != nil {
		t.Fatalf("seed Block diff: %v", err)
	}
	var unchanged bytes.Buffer
	if err := st.RenderStreamFrame(blockFrame, &unchanged); err != nil {
		t.Fatalf("render unchanged Block frame: %v", err)
	}
	if unchanged.Len() != 0 {
		t.Fatalf("unchanged Block frame wrote %d bytes, want 0", unchanged.Len())
	}

	out := &syncBuf{}
	eng := &pipeEngine{frames: make(chan render.Frame, 1)}
	done := make(chan struct{})
	ctl := newControl(eng, done)
	writer := newStreamWriter(out)
	s := &streamSession{
		eng: eng, st: &st, tty: out, ctl: ctl, diff: diff, writer: writer,
		fps: &fpsMeter{window: fpsWindow, start: time.Now()},
	}
	s.firstDrawn.Store(true)
	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendKitty {
		t.Fatalf("backend after Alt+R = %v, want Kitty", st.Backend)
	}

	kittyFrame := render.Frame{EncodedPNG: encodedPNG(t, image.NewRGBA(image.Rect(0, 0, 16, 16)))}
	var assembled bytes.Buffer
	if _, err := s.assemble(&assembled, kittyFrame); err != nil {
		t.Fatalf("assemble Kitty frame after clear: %v", err)
	}
	if !strings.Contains(assembled.String(), "z=-1073741825") {
		t.Fatal("Kitty frame after Block clear lacks FPS z-index")
	}
	writer.send(writeItem{buf: &assembled})
	writer.stop()
	s.writer = nil
	got := out.String()
	clearAt, kittyAt := strings.Index(got, clearScreen), strings.Index(got, "\x1b_G")
	if clearAt < 0 || kittyAt < 0 || clearAt > kittyAt {
		t.Fatalf("Block-to-Kitty order clear=%d Kitty=%d", clearAt, kittyAt)
	}

	st.Backend = BackendBlock
	var repainted bytes.Buffer
	if err := st.RenderStreamFrame(blockFrame, &repainted); err != nil {
		t.Fatalf("render Block frame after reset: %v", err)
	}
	if repainted.Len() == 0 {
		t.Fatal("Block diff was not reset before entering FPS Kitty")
	}
	close(done)
}
