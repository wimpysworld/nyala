package viewer

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

// signalSession returns a browser-free session on an 80x24 terminal with a
// known launch URL, so a test can tell a signal from the URL the row falls back
// to. It has no key worker, so a copy or paste reports inline.
func signalSession(t *testing.T, eng Engine) *streamSession {
	t.Helper()
	st := NewState(80, 24)
	s := &streamSession{eng: eng, st: &st, tty: &discardWriter{}, launchURL: "https://launch.example/"}
	s.firstDrawn.Store(true)
	return s
}

// discardWriter stands in for the terminal where a test asserts on session
// state rather than on the bytes written.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestSignalTextTellsOutcomesApart pins the three lines a reader has to tell
// apart: a completed copy carries the amount it wrote, the refusal names the
// bound it broke rather than reading like a copy, and the discarded paste says
// the page took nothing. Each leads with its outcome, so a narrow row that
// truncates the tail still distinguishes them.
func TestSignalTextTellsOutcomesApart(t *testing.T) {
	copied := signalText(signalCopied, 132)
	refused := signalText(signalCopyRefused, 0)
	discarded := signalText(signalPasteDiscarded, 0)

	if !strings.Contains(copied, "132") {
		t.Fatalf("copy signal %q does not carry the amount copied", copied)
	}
	if !strings.Contains(refused, strconv.Itoa(clipboardMaxBytes)) {
		t.Fatalf("refusal %q does not name the %d-byte bound", refused, clipboardMaxBytes)
	}
	texts := []string{copied, refused, discarded}
	for i, a := range texts {
		for j, b := range texts {
			if i == j {
				continue
			}
			if a == b {
				t.Fatalf("signals %d and %d read the same: %q", i, j, a)
			}
			// The first cells are what a phone-sized terminal keeps.
			if a[:8] == b[:8] {
				t.Fatalf("signals %q and %q share their first cells, so a truncated row cannot tell them apart", a, b)
			}
		}
	}
}

// TestRenderBarSignalKeepsRowWidthAndAnchor pins the layout invariant in every
// signal state and at every width: the row measures exactly cols cells, the
// signal replaces the URL text rather than claiming a column of its own, and
// the anchored menu button does not move. The narrow widths cover a phone-sized
// terminal, including the fallback row too narrow for the chrome.
func TestRenderBarSignalKeepsRowWidthAndAnchor(t *testing.T) {
	signals := []string{
		signalText(signalCopied, 132),
		signalText(signalCopyRefused, 0),
		signalText(signalPasteDiscarded, 0),
	}
	for _, cols := range []int{120, 80, 40, 24, 16, 8, 1} {
		base := barContent{indicator: "⚃", url: "https://example.org/page"}
		plain, without := renderBar(cols, base)
		if w := runewidth.StringWidth(plain); w != cols {
			t.Fatalf("plain row width at %d cols = %d", cols, w)
		}
		for _, sig := range signals {
			withSignal := base
			withSignal.signal = sig
			content, with := renderBar(cols, withSignal)
			if w := runewidth.StringWidth(content); w != cols {
				t.Fatalf("row width at %d cols with signal %q = %d, want %d", cols, sig, w, cols)
			}
			if strings.ContainsRune(content, '\x1b') {
				t.Fatalf("signal row at %d cols carries an escape: %q", cols, content)
			}
			menuWith, okWith := with.segment(barSegmentMenu)
			menuWithout, okWithout := without.segment(barSegmentMenu)
			if okWith != okWithout || menuWith != menuWithout {
				t.Fatalf("the menu moved at %d cols with signal %q: %+v vs %+v", cols, sig, menuWith, menuWithout)
			}
			urlWith, _ := with.segment(barSegmentURL)
			urlWithout, _ := without.segment(barSegmentURL)
			if urlWith != urlWithout {
				t.Fatalf("the URL region changed at %d cols with signal %q: %+v vs %+v", cols, sig, urlWith, urlWithout)
			}
			// The signal takes the URL region, so a row wide enough to hold it
			// shows the signal and not the URL.
			if urlWith.width >= runewidth.StringWidth(sig) && !strings.Contains(content, sig) {
				t.Fatalf("row at %d cols missing the signal %q: %q", cols, sig, content)
			}
			if urlWith.width >= runewidth.StringWidth(sig) && strings.Contains(content, "example.org") {
				t.Fatalf("row at %d cols shows the URL beside the signal: %q", cols, content)
			}
		}
	}
}

// TestSignalExpiresOnItsOwn pins the hold: the bar shows the signal in place of
// the URL while it holds, and the row falls back to the URL once the hold
// elapses, with no key press and no frame needed. The draw-side read clears it,
// so a repaint can never show a signal past its hold even if the loop's expiry
// wake is late.
func TestSignalExpiresOnItsOwn(t *testing.T) {
	s := signalSession(t, streamTestEngine{})

	s.showSignal(signalCopied, 42)
	want := signalText(signalCopied, 42)
	bar := string(s.barBytes())
	if !strings.Contains(bar, want) {
		t.Fatalf("bar %q missing the signal %q", bar, want)
	}
	if strings.Contains(bar, "launch.example") {
		t.Fatalf("bar %q still shows the URL under the signal", bar)
	}

	if got := s.liveSignal(s.signalUntil.Add(-time.Millisecond)); got != want {
		t.Fatalf("signal cleared early: %q", got)
	}
	if got := s.liveSignal(s.signalUntil); got != "" {
		t.Fatalf("signal %q survived its hold", got)
	}
	bar = string(s.barBytes())
	if !strings.Contains(bar, "launch.example") {
		t.Fatalf("expired bar %q did not fall back to the URL", bar)
	}

	// The loop's expiry wake clears the same state and repaints.
	s.showSignal(signalPasteDiscarded, 0)
	s.clearSignal()
	if s.signal != "" {
		t.Fatalf("clearSignal left %q", s.signal)
	}
}

// TestCopySignalsCompletedAndRefused pins the two copy outcomes at the session
// seam: a completed copy writes the clipboard and reports the bytes it wrote,
// and an over-bound selection writes nothing and says why, so the refusal can
// no longer pass for success.
func TestCopySignalsCompletedAndRefused(t *testing.T) {
	s, tty := clipSession(t, "selected words")
	s.handleKey(keyEvent{act: actionCopySelection})
	if !wroteClipboard(tty) {
		t.Fatal("a copy with a selection wrote no clipboard sequence")
	}
	if want := signalText(signalCopied, len("selected words")); s.signal != want {
		t.Fatalf("copy signal = %q, want %q", s.signal, want)
	}

	over, overTTY := clipSessionOver(t, "", true)
	over.handleKey(keyEvent{act: actionCopySelection})
	if wroteClipboard(overTTY) {
		t.Fatalf("an over-bound copy wrote the clipboard: %q", overTTY.String())
	}
	if want := signalText(signalCopyRefused, 0); over.signal != want {
		t.Fatalf("refused-copy signal = %q, want %q", over.signal, want)
	}

	// Nothing selected stays silent: the clipboard was never going to change,
	// and the engine cannot tell an empty selection from a dropped read.
	empty, _ := clipSession(t, "")
	empty.handleKey(keyEvent{act: actionCopySelection})
	if empty.signal != "" {
		t.Fatalf("a copy with no selection signalled %q, want silence", empty.signal)
	}
}

// TestDiscardedPasteSignals pins the paste outcome: a paste the page dropped
// for want of a focused field reports itself, and a paste that landed says
// nothing, because the page shows the text.
func TestDiscardedPasteSignals(t *testing.T) {
	landed := signalSession(t, streamTestEngine{})
	landed.handleKey(keyEvent{paste: "hello"})
	if landed.signal != "" {
		t.Fatalf("a paste that landed signalled %q, want silence", landed.signal)
	}

	lost := signalSession(t, streamTestEngine{pasteDiscarded: true})
	lost.handleKey(keyEvent{paste: "hello"})
	want := signalText(signalPasteDiscarded, 0)
	if lost.signal != want {
		t.Fatalf("discarded-paste signal = %q, want %q", lost.signal, want)
	}
	bar := string(lost.barBytes())
	if !strings.Contains(bar, want) {
		t.Fatalf("bar %q missing the discarded-paste signal", bar)
	}
}

// TestSignalDoesNotBlockInput pins that a showing signal is not a mode: keys
// still reach the page and a bar-row click still opens the menu while it holds.
func TestSignalDoesNotBlockInput(t *testing.T) {
	var keys []string
	s := signalSession(t, streamTestEngine{sendKey: func(code string) { keys = append(keys, code) }})
	s.showSignal(signalCopied, 10)

	s.handleKey(keyEvent{scroll: ScrollDown})
	if len(keys) != 1 || keys[0] != "ArrowDown" {
		t.Fatalf("keys forwarded while a signal showed = %v, want the scroll key", keys)
	}

	menu, ok := s.barLay.segment(barSegmentMenu)
	if !ok {
		t.Fatal("the signal row laid out no menu button")
	}
	s.handleMouse(MouseEvent{Action: MouseActionPress, Col: menu.col - 1, Row: s.st.Rows - 1})
	if s.barMode != barModeMenu {
		t.Fatalf("bar mode = %d after clicking ☰ under a signal, want menu", s.barMode)
	}
}

// TestKeySenderPostsSignalsOffTheInputLoop pins the production path for the two
// outcomes the worker discovers: a refused over-bound copy and a discarded
// paste both come back on the outcome channel, so the consumer loop owns every
// terminal write. A paste that landed posts nothing.
func TestKeySenderPostsSignalsOffTheInputLoop(t *testing.T) {
	eng := &selectionEngine{fakeEngine: &fakeEngine{}, keys: make(chan string, 1)}
	eng.setOver(true)
	ks := newKeySender(eng)
	t.Cleanup(ks.close)
	out := make(chan inputOutcome, 1)

	ks.sendCopy(out)
	select {
	case got := <-out:
		if got.kind != signalCopyRefused || got.text != "" {
			t.Fatalf("worker posted %+v, want the refusal with no text", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker posted no outcome for an over-bound copy")
	}

	lost := &pasteEngine{fakeEngine: &fakeEngine{}, discarded: true, keys: make(chan string, 1)}
	lostKS := newKeySender(lost)
	t.Cleanup(lostKS.close)
	lostKS.sendPaste("text", out)
	select {
	case got := <-out:
		if got.kind != signalPasteDiscarded {
			t.Fatalf("worker posted %+v, want the discarded paste", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker posted no outcome for a discarded paste")
	}

	kept := &pasteEngine{fakeEngine: &fakeEngine{}, keys: make(chan string, 1)}
	keptKS := newKeySender(kept)
	t.Cleanup(keptKS.close)
	keptKS.sendPaste("text", out)
	// Order the silent paste behind a later key: once the key has landed the
	// paste has run, so an empty channel proves nothing was posted.
	keptKS.send("ArrowDown")
	select {
	case got := <-kept.keys:
		if got != "ArrowDown" {
			t.Fatalf("worker forwarded %q, want the queued key", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker forwarded no key after the paste")
	}
	select {
	case got := <-out:
		t.Fatalf("a paste that landed posted %+v, want nothing", got)
	default:
	}
}

// pasteEngine reports whether a paste found a field and records forwarded keys,
// so a worker test can order a paste against a key.
type pasteEngine struct {
	*fakeEngine
	discarded bool
	keys      chan string
}

func (e *pasteEngine) SendPaste(string) bool { return e.discarded }

func (e *pasteEngine) SendKey(code string) { e.keys <- code }
