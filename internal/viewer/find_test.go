package viewer

import (
	"testing"
	"time"
)

// findRecordingEngine records the forwarded search operations and keys on one
// channel, so a keySender test can assert search ordering against typing.
type findRecordingEngine struct {
	*fakeEngine
	got   chan string
	state FindState
}

func (e *findRecordingEngine) SendKey(code string) { e.got <- "key:" + code }

func (e *findRecordingEngine) Find(query string) { e.got <- "find:" + query }

func (e *findRecordingEngine) FindNext() { e.got <- "next" }

func (e *findRecordingEngine) FindPrev() { e.got <- "prev" }

func (e *findRecordingEngine) FindClear() { e.got <- "clear" }

func (e *findRecordingEngine) FindState() FindState { return e.state }

// TestKeySenderOrdersFindWithKeys asserts the shared worker forwards every
// search operation off the input loop and in submission order, so a search, its
// steps, and page keys reach the engine as the user produced them and a wedged
// browser search never blocks quit or draw.
func TestKeySenderOrdersFindWithKeys(t *testing.T) {
	eng := &findRecordingEngine{fakeEngine: &fakeEngine{}, got: make(chan string, 6)}
	ks := newKeySender(eng)
	t.Cleanup(ks.close)

	ks.sendFind("needle")
	ks.sendFindNext()
	ks.send("ArrowDown")
	ks.sendFindPrev()
	ks.sendFindClear()

	want := []string{"find:needle", "next", "key:ArrowDown", "prev", "clear"}
	deadline := time.After(2 * time.Second)
	for i, w := range want {
		select {
		case got := <-eng.got:
			if got != w {
				t.Fatalf("forwarded event %d = %q, want %q", i, got, w)
			}
		case <-deadline:
			t.Fatalf("keySender forwarded %d events, want %d", i, len(want))
		}
	}
}

// TestFindStateReadsWithoutDispatch asserts the state read is a plain snapshot:
// it forwards nothing to the engine, so a repaint can read the counts as often
// as it likes while a search is still in flight.
func TestFindStateReadsWithoutDispatch(t *testing.T) {
	eng := &findRecordingEngine{
		fakeEngine: &fakeEngine{},
		got:        make(chan string, 1),
		state:      FindState{Query: "needle", Total: 4, Current: 2, Pending: true},
	}

	got := eng.FindState()

	if got.Total != 4 || got.Current != 2 || !got.Pending {
		t.Fatalf("find state = %+v, want the second of four matches with a search in flight", got)
	}
	select {
	case ev := <-eng.got:
		t.Fatalf("reading the search state forwarded %q to the engine", ev)
	default:
	}
}
