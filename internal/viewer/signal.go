package viewer

import (
	"fmt"
	"time"
)

// barSignalKind identifies one input outcome the bar reports. Three outcomes
// are otherwise silent and read exactly like success: a copy writes the
// terminal clipboard with nothing on screen to say so, a selection over the
// clipboard bound is refused rather than truncated, and a paste that reached no
// page field is dropped by Chromium without a word. Space arbitration has the
// same shape and needs no signal, because the page visibly scrolls or types.
type barSignalKind int

const (
	// signalCopied is a completed copy, carrying the bytes written.
	signalCopied barSignalKind = iota
	// signalCopyRefused is a selection over the clipboard bound: nothing was
	// written and the previous clipboard still stands.
	signalCopyRefused
	// signalPasteDiscarded is a paste the page dropped because no field held
	// focus.
	signalPasteDiscarded
)

// barSignalHold is how long one signal holds the bar's URL region before the
// row falls back to the URL. It is long enough to read a short line and short
// enough that the URL is never gone for long. The consumer loop wakes on the
// expiry, so an idle page that pushes no frames clears the signal as promptly
// as a busy one, and no key press is needed to dismiss it.
const barSignalHold = 2 * time.Second

// signalText renders one signal for the bar row. n is the byte count a
// completed copy wrote and is ignored for the other kinds. Each line leads with
// the outcome, so a narrow terminal that truncates the tail still tells the
// three apart, and the refusal names the bound it broke.
func signalText(kind barSignalKind, n int) string {
	switch kind {
	case signalCopyRefused:
		return fmt.Sprintf("Too large to copy: over %d bytes", clipboardMaxBytes)
	case signalPasteDiscarded:
		return "Paste discarded: no page field focused"
	default:
		return fmt.Sprintf("Copied %d bytes", n)
	}
}

// inputOutcome is one input result the serialised worker hands back to the
// consumer loop, which owns the terminal writes: kind is what happened, and
// text carries the selection a completed copy has to write. The worker never
// writes the terminal itself, so the clipboard sequence and the bar signal both
// go out through the one ordered control path.
type inputOutcome struct {
	kind barSignalKind
	text string
}

// copyOutcome maps one selection read to the outcome the bar reports: a
// completed copy with its text, a refusal for a selection over the bound, and
// no outcome at all when the page held no selection or the read did not land,
// which are the cases where the clipboard was never going to change.
func copyOutcome(text string, over bool) (inputOutcome, bool) {
	switch {
	case over:
		return inputOutcome{kind: signalCopyRefused}, true
	case text != "":
		return inputOutcome{kind: signalCopied, text: text}, true
	default:
		return inputOutcome{}, false
	}
}

// applyOutcome writes one input outcome: the clipboard sequence for a completed
// copy, then the bar signal for whatever happened. It runs on the consumer
// loop, the goroutine that owns the terminal writes.
func (s *streamSession) applyOutcome(o inputOutcome) {
	if o.kind == signalCopied {
		s.writeClipboard(o.text)
	}
	s.showSignal(o.kind, len(o.text))
}

// showSignal puts one signal on the chrome bar row and repaints it there. The
// signal takes the URL region, the one flexible segment, so the buttons and the
// right-anchored menu keep their columns and the row still measures exactly
// cols cells. It blocks nothing: the row keeps its hit spans and the loop keeps
// dispatching keys, mouse events, and frames while the signal shows.
func (s *streamSession) showSignal(kind barSignalKind, n int) {
	s.signal = signalText(kind, n)
	s.signalUntil = time.Now().Add(barSignalHold)
	s.repaintBar()
}

// liveSignal returns the signal the chrome row draws at now, clearing one whose
// hold has elapsed. The loop's expiry wake normally clears it first; this is the
// draw-side guard, so a repaint can never show a signal past its hold. Producer
// goroutine only, like the rest of the bar state.
func (s *streamSession) liveSignal(now time.Time) string {
	if s.signal != "" && !now.Before(s.signalUntil) {
		s.signal = ""
	}
	return s.signal
}

// clearSignal drops the signal and repaints the bar, the consumer loop's
// expiry wake. A session with no signal repaints nothing.
func (s *streamSession) clearSignal() {
	if s.signal == "" {
		return
	}
	s.signal = ""
	s.repaintBar()
}
