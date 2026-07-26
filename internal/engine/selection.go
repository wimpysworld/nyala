package engine

import (
	_ "embed"
	"strconv"
	"strings"

	"github.com/chromedp/chromedp"
)

const selectionMaxPlaceholder = "__NYALA_SELECTION_MAX__"

//go:embed selection.js
var selectionSource string

// selectionMaxBytes caps the selection one read carries back from the page. It
// is the largest payload the terminal clipboard write accepts (the viewer's
// clipboardMaxBytes), so an oversized selection is refused at the source and
// never crosses the CDP wire for a write that would refuse it anyway.
const selectionMaxBytes = 74994

// selectionResult is the page script's return: the selected text, and Over when
// the page held a selection longer than the cap. Over is separate from an empty
// text so a refused selection is not read as no selection at all.
type selectionResult struct {
	Text string `json:"text"`
	Over bool   `json:"over"`
}

// buildSelectionScript returns the page script for one selection read, with the
// cap substituted for its placeholder.
func buildSelectionScript() string {
	return strings.Replace(selectionSource, selectionMaxPlaceholder, strconv.Itoa(selectionMaxBytes), 1)
}

// Selection returns the text the page currently has selected, for an explicit
// copy, and whether that selection was over selectionMaxBytes. It reads the DOM
// selection, and a focused text control's own selection where the document
// cannot see one, through the same bounded dispatch as keys and mouse events, so
// a wedged browser drops the read rather than parking the caller.
//
// An over-bound selection returns no text and over true: it is refused rather
// than truncated, and the caller reports the refusal instead of a copy that
// never happened. Empty text with over false means the page holds no selection
// or the dispatch did not land; both mean there is nothing to copy.
//
// It is a read of the page, not of the terminal. Nyala never reads the terminal
// clipboard, so no page can pull the user's clipboard contents out through this
// seam, and selecting text writes nothing on its own: only an explicit copy
// calls this.
func (e *ChromiumEngine) Selection() (text string, over bool) {
	ctx := e.inputCtx()
	if ctx == nil {
		return "", false
	}
	var res selectionResult
	if err := e.runInput(ctx, chromedp.Evaluate(buildSelectionScript(), &res)); err != nil {
		return "", false
	}
	// The page cap counts UTF-16 units, so multi-byte text can pass it and still
	// exceed the byte bound the clipboard sequence is framed by. Re-check here,
	// where the length is bytes.
	if res.Over || len(res.Text) > selectionMaxBytes {
		return "", true
	}
	return res.Text, false
}
