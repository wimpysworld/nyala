package engine

import (
	"strconv"
	"strings"
	"testing"
)

// TestBuildSelectionScriptSubstitutesCap pins the one substitution the selection
// script takes: the read cap replaces its placeholder, so the page refuses an
// oversized selection instead of shipping it over the wire.
func TestBuildSelectionScriptSubstitutesCap(t *testing.T) {
	got := buildSelectionScript()

	if strings.Contains(got, selectionMaxPlaceholder) {
		t.Fatal("the selection script kept its cap placeholder")
	}
	if !strings.Contains(got, strconv.Itoa(selectionMaxBytes)) {
		t.Fatalf("the selection script does not carry the %d-byte cap", selectionMaxBytes)
	}
}

// TestSelectionScriptOnlyReadsTheSelection pins the read as page-only: the script
// touches no clipboard API, so a copy can never pull the system clipboard through
// the page, and it writes nothing back into the document.
func TestSelectionScriptOnlyReadsTheSelection(t *testing.T) {
	for _, banned := range []string{"navigator.clipboard", "clipboardData", "execCommand", "document.write"} {
		if strings.Contains(selectionSource, banned) {
			t.Fatalf("the selection script references %q, which a selection read must not touch", banned)
		}
	}
	if !strings.Contains(selectionSource, "window.getSelection()") {
		t.Fatal("the selection script does not read the DOM selection")
	}
}
