package viewer

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clipSession returns a browser-free session whose engine reports selection as
// the page's selected text, plus the buffer standing in for the terminal. It has
// no key worker, so a copy reads and writes inline and the assertions read the
// bytes the session emitted.
func clipSession(t *testing.T, selection string) (*streamSession, *bytes.Buffer) {
	t.Helper()
	tty := &bytes.Buffer{}
	st := NewState(80, 24)
	s := &streamSession{
		eng: streamTestEngine{selection: func() string { return selection }},
		st:  &st,
		tty: tty,
	}
	return s, tty
}

// wroteClipboard reports whether buf carries an OSC 52 clipboard write.
func wroteClipboard(buf *bytes.Buffer) bool {
	return bytes.Contains(buf.Bytes(), []byte(osc52Prefix))
}

// clipboardPayload returns the text one OSC 52 write in buf carries, decoded
// from its base64 payload, so a test asserts what the terminal was really given.
func clipboardPayload(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	_, rest, found := strings.Cut(buf.String(), osc52Prefix)
	if !found {
		t.Fatalf("output carries no clipboard write: %q", buf.String())
	}
	encoded, _, found := strings.Cut(rest, osc52Terminator)
	if !found {
		t.Fatalf("clipboard write is unterminated: %q", rest)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("clipboard payload %q is not base64: %v", encoded, err)
	}
	return string(raw)
}

// TestOSC52ClipboardEncodesPayload pins the sequence one copy emits: the OSC 52
// introducer with the CLIPBOARD target, the text as base64 (so no byte of the
// page's text can reach the terminal parser as an escape), and the BEL
// terminator. Multi-byte text and text carrying its own control bytes both
// round-trip, because the payload is encoded rather than escaped.
func TestOSC52ClipboardEncodesPayload(t *testing.T) {
	for _, text := range []string{"alpha", "ünïcøde ✓", "line\nbreak\ttab", "\x1b]52;c;evil\a"} {
		got := osc52Clipboard(text)
		if !bytes.HasPrefix(got, []byte("\x1b]52;c;")) {
			t.Fatalf("sequence for %q = %q, want the OSC 52 CLIPBOARD prefix", text, got)
		}
		if !bytes.HasSuffix(got, []byte("\a")) {
			t.Fatalf("sequence for %q = %q, want a BEL terminator", text, got)
		}
		payload := got[len("\x1b]52;c;") : len(got)-1]
		raw, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil {
			t.Fatalf("payload for %q is not base64: %v", text, err)
		}
		if string(raw) != text {
			t.Fatalf("decoded payload = %q, want %q", raw, text)
		}
		// The framing is the only escape in the sequence: the payload alphabet
		// cannot carry an ESC, so a hostile page cannot smuggle one through a copy.
		if bytes.Count(got, []byte{0x1b}) != 1 {
			t.Fatalf("sequence for %q carries %d escapes, want only the introducer", text, bytes.Count(got, []byte{0x1b}))
		}
	}
}

// TestOSC52ClipboardBound pins the write bound at its boundary: nothing to copy
// and a selection exactly at clipboardMaxBytes are the two ends that must work,
// and one byte over is refused rather than truncated, so no caller can emit a
// sequence past the limit a terminal is known to parse. It also checks the bound
// against the arithmetic it comes from: the framed base64 sequence must fit the
// 100,000-byte de-facto limit.
func TestOSC52ClipboardBound(t *testing.T) {
	if got := osc52Clipboard(""); got != nil {
		t.Fatalf("empty text emitted %q, want no write", got)
	}

	atBound := strings.Repeat("a", clipboardMaxBytes)
	seq := osc52Clipboard(atBound)
	if seq == nil {
		t.Fatalf("a selection of exactly %d bytes was refused, want a write", clipboardMaxBytes)
	}
	if len(seq) > 100000 {
		t.Fatalf("the sequence at the bound is %d bytes, want at most the 100000-byte OSC 52 limit", len(seq))
	}

	if got := osc52Clipboard(atBound + "a"); got != nil {
		t.Fatalf("a selection one byte over the bound emitted %d bytes, want a refusal", len(got))
	}
}

// TestClipboardSequencesAreWriteOnly scans the package sources for OSC 52
// literals and pins two invariants the copy path depends on: the only target is
// `c`, CLIPBOARD, never `p`, the X11 PRIMARY selection the terminal's own
// Shift-drag owns, and no sequence carries the `?` query form, which is the only
// form that reads the clipboard back. One literal exists, so there is a single
// producer to audit.
func TestClipboardSequencesAreWriteOnly(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	const introducer = `]52;`
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for rest := string(src); ; {
			_, after, ok := strings.Cut(rest, introducer)
			if !ok {
				break
			}
			found++
			if !strings.HasPrefix(after, "c;") {
				t.Fatalf("%s has an OSC 52 sequence targeting %q, want the CLIPBOARD target `c`", name, after[:min(len(after), 4)])
			}
			if strings.HasPrefix(after, "c;?") {
				t.Fatalf("%s has an OSC 52 clipboard read, which Nyala never sends", name)
			}
			rest = after
		}
	}
	if found != 1 {
		t.Fatalf("found %d OSC 52 literals in the package, want the single write prefix", found)
	}
}

// TestSelectionAloneWritesNoClipboard pins the split the whole feature rests on:
// a drag that selects page text writes nothing to the clipboard, and only the
// explicit copy does. A selection that copied itself would overwrite the user's
// clipboard every time they highlighted a word.
func TestSelectionAloneWritesNoClipboard(t *testing.T) {
	s, tty := clipSession(t, "selected words")

	// One whole drag gesture over the page region: press, motion, release.
	s.handleMouse(MouseEvent{Action: MouseActionPress, Col: 4, Row: 6})
	s.handleMouse(MouseEvent{Action: MouseActionMove, Col: 12, Row: 6})
	s.handleMouse(MouseEvent{Action: MouseActionRelease, Col: 12, Row: 6})

	if wroteClipboard(tty) {
		t.Fatalf("a drag selection wrote the clipboard: %q", tty.String())
	}

	s.handleKey(keyEvent{act: actionCopySelection})

	if got := clipboardPayload(t, tty); got != "selected words" {
		t.Fatalf("copied text = %q, want the page selection", got)
	}
}

// TestCopyWithNoSelectionWritesNothing pins the empty case: a copy with nothing
// selected, an over-long selection, and a read that did not land all report no
// text, and none of them touches the clipboard, so a stray Alt+W cannot clear
// what the user copied earlier.
func TestCopyWithNoSelectionWritesNothing(t *testing.T) {
	s, tty := clipSession(t, "")

	s.handleKey(keyEvent{act: actionCopySelection})

	if wroteClipboard(tty) {
		t.Fatalf("a copy with no selection wrote %q, want no write", tty.String())
	}
}

// TestCopyBindingAndMenuCommand pins the two ways in: the Alt+W chord decodes to
// the copy action, and the menu's Copy command runs the same copy and closes the
// menu, so the one page action a reader cannot guess is still reachable from the
// row.
func TestCopyBindingAndMenuCommand(t *testing.T) {
	if act, ok := altKeyAction('w'); !ok || act != actionCopySelection {
		t.Fatalf("Alt+W decoded to action %d (ok %v), want the copy action", act, ok)
	}

	s, tty := clipSession(t, "menu copy")
	s.handleKey(keyEvent{act: actionOpenMenu})
	s.menuSel = menuIndex(t, s.fpsBadgeShown(), "Copy")
	if quit := s.handleKey(keyEvent{b: keyEnter}); quit {
		t.Fatal("the Copy command quit the session")
	}

	if got := clipboardPayload(t, tty); got != "menu copy" {
		t.Fatalf("menu copy wrote %q, want the page selection", got)
	}
	if s.barMode != barModeChrome {
		t.Fatalf("bar mode = %d after the Copy command, want chrome", s.barMode)
	}
}

// chunkWriter records each Write call separately, so a test can tell one whole
// sequence from bytes spliced into another write.
type chunkWriter struct {
	mu     sync.Mutex
	chunks []string
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chunks = append(w.chunks, string(p))
	return len(p), nil
}

func (w *chunkWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.chunks...)
}

// TestClipboardWriteSerialisesWithFrames pins the ordering rule for the escape
// sequence: it rides the one writer goroutine like any control write, so a copy
// taken while frames are streaming flushes the assembled frame first and then
// goes out as one whole write. Splicing it into a frame's bytes is the failure
// this guards: the terminal would read the payload as page output and paint it
// over the render.
func TestClipboardWriteSerialisesWithFrames(t *testing.T) {
	tty := &chunkWriter{}
	w := newStreamWriter(tty)
	st := NewState(80, 24)
	s := &streamSession{writer: w, tty: tty, st: &st}

	frame := &bytes.Buffer{}
	frame.WriteString("FRAME")
	s.assembled = frame

	s.writeClipboard("copied")
	w.stop()

	want := string(osc52Clipboard("copied"))
	got := tty.all()
	if len(got) != 2 || got[0] != "FRAME" || got[1] != want {
		t.Fatalf("terminal writes = %q, want the frame then the whole clipboard sequence", got)
	}
	if s.assembled != nil {
		t.Fatal("the clipboard write left the assembled frame unsent")
	}
}

// TestKeySenderPostsCopyOffTheInputLoop pins the copy path in production shape:
// the page read runs on the serialised worker, in order with keys, and the text
// comes back on the channel the consumer loop writes from, so the escape
// sequence is never written by the worker goroutine. A read with nothing selected
// posts nothing.
func TestKeySenderPostsCopyOffTheInputLoop(t *testing.T) {
	eng := &selectionEngine{fakeEngine: &fakeEngine{}, text: "worker copy", keys: make(chan string, 1)}
	ks := newKeySender(eng)
	t.Cleanup(ks.close)
	clip := make(chan string, 1)

	ks.sendCopy(clip)
	select {
	case got := <-clip:
		if got != "worker copy" {
			t.Fatalf("worker posted %q, want the page selection", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker posted no copied text")
	}

	eng.setText("")
	ks.sendCopy(clip)
	// Order the empty read behind a later key: once the key has landed the copy
	// has run, so an empty channel proves nothing was posted rather than racing.
	ks.send("ArrowDown")
	select {
	case got := <-eng.keys:
		if got != "ArrowDown" {
			t.Fatalf("worker forwarded %q, want the queued key", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker forwarded no key after the copy")
	}
	select {
	case got := <-clip:
		t.Fatalf("a copy with no selection posted %q, want nothing", got)
	default:
	}
}

// selectionEngine reports a page selection and records forwarded keys, so a
// worker test can order a copy against a key.
type selectionEngine struct {
	*fakeEngine
	mu   sync.Mutex
	text string
	keys chan string
}

func (e *selectionEngine) setText(text string) {
	e.mu.Lock()
	e.text = text
	e.mu.Unlock()
}

func (e *selectionEngine) Selection() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.text
}

func (e *selectionEngine) SendKey(code string) { e.keys <- code }
