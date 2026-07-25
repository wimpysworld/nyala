package viewer

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDecodeSGRMouse pins the SGR mouse decode table: buttons and finals,
// modifier bitmasks translated to CDP bits, wheel directions, drag motion with
// its held button and modifiers, low coordinate clamping, and malformed bodies
// dropped.
func TestDecodeSGRMouse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want MouseEvent
		ok   bool
	}{
		{
			name: "left press",
			body: "<0;12;5M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 11, Row: 4},
			ok:   true,
		},
		{
			name: "left release",
			body: "<0;12;5m",
			want: MouseEvent{Action: MouseActionRelease, Button: MouseButtonLeft, Col: 11, Row: 4},
			ok:   true,
		},
		{
			name: "middle press",
			body: "<1;1;1M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonMiddle},
			ok:   true,
		},
		{
			name: "right press",
			body: "<2;80;24M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonRight, Col: 79, Row: 23},
			ok:   true,
		},
		{
			name: "shift press",
			body: "<4;2;2M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Mod: cdpModShift, Col: 1, Row: 1},
			ok:   true,
		},
		{
			name: "alt press",
			body: "<8;2;2M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Mod: cdpModAlt, Col: 1, Row: 1},
			ok:   true,
		},
		{
			name: "ctrl press",
			body: "<16;2;2M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Mod: cdpModCtrl, Col: 1, Row: 1},
			ok:   true,
		},
		{
			name: "ctrl shift right release",
			body: "<22;3;3m",
			want: MouseEvent{Action: MouseActionRelease, Button: MouseButtonRight, Mod: cdpModCtrl | cdpModShift, Col: 2, Row: 2},
			ok:   true,
		},
		{
			name: "all modifiers middle press",
			body: "<29;2;2M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonMiddle, Mod: cdpModAlt | cdpModCtrl | cdpModShift, Col: 1, Row: 1},
			ok:   true,
		},
		{
			name: "wheel up",
			body: "<64;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDY: -1, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel down",
			body: "<65;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDY: 1, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel left",
			body: "<66;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDX: -1, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel right",
			body: "<67;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDX: 1, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel up with ctrl",
			body: "<80;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDY: -1, Mod: cdpModCtrl, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel left with shift keeps the modifier",
			body: "<70;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDX: -1, Mod: cdpModShift, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel right with shift keeps the modifier",
			body: "<71;10;10M",
			want: MouseEvent{Action: MouseActionWheel, WheelDX: 1, Mod: cdpModShift, Col: 9, Row: 9},
			ok:   true,
		},
		{
			name: "wheel right with all modifiers",
			body: "<95;2;3M",
			want: MouseEvent{
				Action: MouseActionWheel, WheelDX: 1,
				Mod: cdpModAlt | cdpModCtrl | cdpModShift, Col: 1, Row: 2,
			},
			ok: true,
		},
		{name: "wheel left release dropped", body: "<66;10;10m"},
		{name: "wheel right release dropped", body: "<67;10;10m"},
		{
			name: "left drag motion",
			body: "<32;5;5M",
			want: MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 4, Row: 4},
			ok:   true,
		},
		{
			name: "middle drag motion",
			body: "<33;5;5M",
			want: MouseEvent{Action: MouseActionMove, Button: MouseButtonMiddle, Col: 4, Row: 4},
			ok:   true,
		},
		{
			name: "right drag motion",
			body: "<34;5;5M",
			want: MouseEvent{Action: MouseActionMove, Button: MouseButtonRight, Col: 4, Row: 4},
			ok:   true,
		},
		{
			name: "shift left drag motion keeps the modifier",
			body: "<36;5;5M",
			want: MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Mod: cdpModShift, Col: 4, Row: 4},
			ok:   true,
		},
		{
			name: "all modifiers right drag motion",
			body: "<62;7;3M",
			want: MouseEvent{
				Action: MouseActionMove, Button: MouseButtonRight,
				Mod: cdpModAlt | cdpModCtrl | cdpModShift, Col: 6, Row: 2,
			},
			ok: true,
		},
		{
			name: "drag motion with the release final still moves",
			body: "<32;5;5m",
			want: MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 4, Row: 4},
			ok:   true,
		},
		{name: "buttonless motion dropped", body: "<35;5;5m"},
		{name: "wheel motion dropped", body: "<96;5;5M"},
		{name: "side button motion dropped", body: "<160;5;5M"},
		{
			name: "zero coordinates clamp",
			body: "<0;0;0M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft},
			ok:   true,
		},
		{
			name: "negative coordinates clamp",
			body: "<0;-3;-9M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft},
			ok:   true,
		},
		{name: "wheel release dropped", body: "<64;10;10m"},
		{name: "buttonless press dropped", body: "<3;1;1M"},
		{
			name: "back button press",
			body: "<128;1;1M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonBack},
			ok:   true,
		},
		{
			name: "forward button press",
			body: "<129;1;1M",
			want: MouseEvent{Action: MouseActionPress, Button: MouseButtonForward},
			ok:   true,
		},
		{
			name: "back button release",
			body: "<130;1;1m",
			want: MouseEvent{Action: MouseActionRelease, Button: MouseButtonBack},
			ok:   true,
		},
		{
			name: "forward button release",
			body: "<131;1;1m",
			want: MouseEvent{Action: MouseActionRelease, Button: MouseButtonForward},
			ok:   true,
		},
		{name: "negative button dropped", body: "<-1;1;1M"},
		{name: "two parameters dropped", body: "<0;12M"},
		{name: "four parameters dropped", body: "<0;1;2;3M"},
		{name: "non-numeric dropped", body: "<a;1;1M"},
		{name: "empty dropped", body: "<M"},
		{name: "wrong final dropped", body: "<0;1;1~"},
		{name: "not sgr dropped", body: "0;1;1M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeSGRMouse([]byte(tc.body))
			if ok != tc.ok {
				t.Fatalf("decodeSGRMouse(%q) ok = %t, want %t", tc.body, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("decodeSGRMouse(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

// collectEvents drains a whole input string through the shared parser and
// returns the emitted events, so a test observes exactly what the input loop
// would receive: no dropped sequence may leak bytes as key events.
func collectEvents(t *testing.T, in string) []keyEvent {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(in))
	var events []keyEvent
	for {
		b, err := br.ReadByte()
		if err != nil {
			return events
		}
		if ev, emit := nextEvent(br, b); emit {
			events = append(events, ev)
		}
	}
}

// TestNextEventMouseAPCInterleave asserts SGR mouse reports, Kitty APC
// replies, drag motion, and plain keys survive interleaving on the one stdin
// parser: each event stays typed, a buttonless motion report is consumed
// silently, and no report leaks bytes as keys.
func TestNextEventMouseAPCInterleave(t *testing.T) {
	in := "\x1b_Gi=1;OK\x1b\\" + "\x1b[<0;3;2M" + "\x1b[<32;4;2M" + "\x1b[<35;9;9M" +
		"\x1b[<0;4;2m" + "\x1b[<65;4;4M" + "q"
	events := collectEvents(t, in)
	if len(events) != 6 {
		t.Fatalf("events = %d (%+v), want 6: apc, press, move, release, wheel, key", len(events), events)
	}
	if events[0].apc != "i=1;OK" {
		t.Errorf("event 0 apc = %q, want %q", events[0].apc, "i=1;OK")
	}
	press := events[1].mouse
	if press == nil || press.Action != MouseActionPress || press.Col != 2 || press.Row != 1 {
		t.Errorf("event 1 = %+v, want left press at cell (2, 1)", events[1])
	}
	move := events[2].mouse
	if move == nil || move.Action != MouseActionMove || move.Button != MouseButtonLeft ||
		move.Col != 3 || move.Row != 1 {
		t.Errorf("event 2 = %+v, want left drag motion at cell (3, 1)", events[2])
	}
	release := events[3].mouse
	if release == nil || release.Action != MouseActionRelease || release.Col != 3 {
		t.Errorf("event 3 = %+v, want left release at cell (3, 1)", events[3])
	}
	wheel := events[4].mouse
	if wheel == nil || wheel.Action != MouseActionWheel || wheel.WheelDY != 1 {
		t.Errorf("event 4 = %+v, want wheel down", events[4])
	}
	if events[5].b != 'q' || events[5].mouse != nil {
		t.Errorf("event 5 = %+v, want plain key 'q'", events[5])
	}
}

// TestNextEventMalformedMouseDropped asserts a malformed SGR mouse report is
// consumed whole, like any unrecognised CSI, and the following byte still
// arrives as a key event.
func TestNextEventMalformedMouseDropped(t *testing.T) {
	events := collectEvents(t, "\x1b[<0;12M"+"\x1b[<;1;1M"+"q")
	if len(events) != 1 || events[0].b != 'q' {
		t.Fatalf("events = %+v, want only the trailing 'q' key", events)
	}
}

// TestMouseReportingEscapes pins the mode pair Stream enables (1002
// button-event tracking, 1006 SGR encoding) and asserts every teardown emits
// the matching disables, drawn or not, so a leaked mode cannot spray reports
// into the shell.
func TestMouseReportingEscapes(t *testing.T) {
	if enableMouseReporting != "\x1b[?1002h\x1b[?1006h" {
		t.Errorf("enableMouseReporting = %q, want ?1002h?1006h", enableMouseReporting)
	}
	if disableMouseReporting != "\x1b[?1002l\x1b[?1006l" {
		t.Errorf("disableMouseReporting = %q, want ?1002l?1006l", disableMouseReporting)
	}
	for _, drawn := range []bool{true, false} {
		if got := exitTerminal(24, drawn, false); !strings.Contains(got, disableMouseReporting) {
			t.Errorf("exitTerminal(24, %t) = %q, want it to contain %q", drawn, got, disableMouseReporting)
		}
	}
}

// dragSession builds a synchronous-dispatch session on an 80x24 grid (bar row
// 23) that records every forwarded mouse event, so a drag test asserts exactly
// what reached the engine seam.
func dragSession(t *testing.T) (*streamSession, *[]MouseEvent) {
	t.Helper()
	st := NewState(80, 24)
	var got []MouseEvent
	s := dispatchSession(streamTestEngine{
		sendMouse: func(ev MouseEvent) { got = append(got, ev) },
	}, &st)
	s.version = "dev"
	return s, &got
}

// pageDrag builds the events of one left drag across the page region: the
// press, the moves, and the release, all carrying mod.
func pageDrag(mod int, cells ...[2]int) []MouseEvent {
	evs := make([]MouseEvent, 0, len(cells))
	for i, c := range cells {
		action := MouseActionMove
		switch i {
		case 0:
			action = MouseActionPress
		case len(cells) - 1:
			action = MouseActionRelease
		}
		evs = append(evs, MouseEvent{
			Action: action, Button: MouseButtonLeft, Mod: mod, Col: c[0], Row: c[1],
		})
	}
	return evs
}

// TestHandleMouseForwardsDrag asserts a press-move-release gesture on the page
// reaches the engine whole and in order, with the held button and the modifier
// bits preserved on every motion report, so a shift-extended drag-selection
// keeps its modifier all the way to CDP.
func TestHandleMouseForwardsDrag(t *testing.T) {
	s, got := dragSession(t)
	drag := pageDrag(cdpModShift, [2]int{4, 4}, [2]int{6, 5}, [2]int{9, 7}, [2]int{12, 9})

	for _, ev := range drag {
		if quit := s.handleMouse(ev); quit {
			t.Fatalf("%+v requested quit", ev)
		}
	}

	if len(*got) != len(drag) {
		t.Fatalf("forwarded %d events, want the whole gesture (%d)", len(*got), len(drag))
	}
	for i, want := range drag {
		if (*got)[i] != want {
			t.Fatalf("forwarded event %d = %+v, want %+v", i, (*got)[i], want)
		}
	}
	if s.dragOnPage {
		t.Fatal("release left the pointer captured")
	}
}

// TestHandleMouseDragCrossesBarRow asserts the press captures the pointer for
// the page: motion and the release over the bar row still reach the page rather
// than hit-testing the bar, so a drag that runs off the bottom of the page does
// not open the URL editor or press a bar button mid-gesture.
func TestHandleMouseDragCrossesBarRow(t *testing.T) {
	s, got := dragSession(t)
	barRow := s.st.Rows - 1
	// Draw the bar once, so its spans exist and a bar-row hit would resolve.
	s.repaintBar()

	for _, ev := range pageDrag(0, [2]int{10, 10}, [2]int{20, barRow}, [2]int{30, barRow}) {
		s.handleMouse(ev)
	}

	if len(*got) != 3 {
		t.Fatalf("forwarded %d events, want the press, the bar-row move, and the release", len(*got))
	}
	if (*got)[1].Action != MouseActionMove || (*got)[1].Row != barRow {
		t.Fatalf("bar-row move = %+v, want a forwarded move on the bar row", (*got)[1])
	}
	if (*got)[2].Action != MouseActionRelease {
		t.Fatalf("last forwarded event = %+v, want the release", (*got)[2])
	}
	if s.barMode != barModeChrome {
		t.Fatalf("the drag opened a bar mode: %d", s.barMode)
	}
}

// TestHandleMouseDragFromBarRowStaysOnBar asserts the mirror rule: a press on
// the bar row captures the pointer for the bar, so its motion never injects
// page input, and the release clears the press feedback.
func TestHandleMouseDragFromBarRowStaysOnBar(t *testing.T) {
	s, got := dragSession(t)
	barRow := s.st.Rows - 1
	s.repaintBar()
	seg, ok := s.barLay.segment(barSegmentBack)
	if !ok {
		t.Fatal("the bar has no back button to press")
	}

	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: seg.col - 1, Row: barRow})
	s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 20, Row: 10})
	s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 30, Row: 12})
	s.handleMouse(MouseEvent{Action: MouseActionRelease, Button: MouseButtonLeft, Col: 30, Row: 12})

	if len(*got) != 0 {
		t.Fatalf("a bar-row drag reached the page: %+v", *got)
	}
	if s.pressedSet {
		t.Fatal("the release left the bar button pressed")
	}
	if s.dragOnPage {
		t.Fatal("a bar-row press captured the pointer for the page")
	}
}

// TestHandleMouseMotionNeedsCapture asserts motion with no press in flight is
// dropped, and that a release for a button never pressed leaves no capture
// behind for a later stray move to ride on. Mode 1002 reports motion only
// during a drag, so a bare move means the state is not ours to forward.
func TestHandleMouseMotionNeedsCapture(t *testing.T) {
	s, got := dragSession(t)

	s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 5, Row: 5})
	if len(*got) != 0 {
		t.Fatalf("uncaptured motion reached the page: %+v", *got)
	}

	// A release with no matching press clears state rather than arming it.
	s.handleMouse(MouseEvent{Action: MouseActionRelease, Button: MouseButtonLeft, Col: 5, Row: 5})
	if s.dragOnPage {
		t.Fatal("a release for an unpressed button captured the pointer")
	}
	before := len(*got)
	s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 6, Row: 6})
	if len(*got) != before {
		t.Fatalf("motion after a stray release reached the page: %+v", (*got)[before:])
	}
}

// TestHandleMouseBarModeDropsCapture asserts a bar mode taking over input drops
// the pointer capture: the mode swallows the release that would otherwise clear
// it, so a page press, a Ctrl+L, and a release must not leave the capture armed
// once the mode closes. Mode 1002 reports no buttonless motion today, so a stale
// capture is latent rather than visible; it would forward motion to the page the
// moment any-event tracking arrives.
func TestHandleMouseBarModeDropsCapture(t *testing.T) {
	s, got := dragSession(t)

	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 10, Row: 10})
	if !s.dragOnPage {
		t.Fatal("a page press did not capture the pointer")
	}
	if s.handleKey(keyEvent{b: keyCtrlL}); s.barMode != barModeEdit {
		t.Fatalf("Ctrl+L did not open the URL editor: mode %d", s.barMode)
	}
	// The editor owns this release, so the page never sees it.
	s.handleMouse(MouseEvent{Action: MouseActionRelease, Button: MouseButtonLeft, Col: 12, Row: 11})
	if s.dragOnPage {
		t.Fatal("a release taken by a bar mode left the pointer captured")
	}

	s.handleKey(keyEvent{b: keyEsc})
	if s.barMode != barModeChrome {
		t.Fatalf("Esc did not close the editor: mode %d", s.barMode)
	}
	before := len(*got)
	s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 14, Row: 12})
	if len(*got) != before {
		t.Fatalf("motion on a stale capture reached the page: %+v", (*got)[before:])
	}
}

// TestHandleMouseDragDoesNotDisturbClickOrWheel asserts the routing regressions
// this issue could cause: a wheel on the page still forwards, a wheel on the
// bar row is still dropped, and a wheel taken mid-drag neither ends the capture
// nor stops the drag reaching the page.
func TestHandleMouseDragDoesNotDisturbClickOrWheel(t *testing.T) {
	s, got := dragSession(t)
	barRow := s.st.Rows - 1
	s.repaintBar()

	s.handleMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Col: 3, Row: 4})
	if len(*got) != 1 || (*got)[0].Action != MouseActionWheel {
		t.Fatalf("page wheel = %+v, want one forwarded wheel", *got)
	}
	s.handleMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Col: 3, Row: barRow})
	if len(*got) != 1 {
		t.Fatalf("a bar-row wheel forwarded: %+v", *got)
	}

	s.handleMouse(MouseEvent{Action: MouseActionPress, Button: MouseButtonLeft, Col: 8, Row: 8})
	s.handleMouse(MouseEvent{Action: MouseActionWheel, WheelDY: -1, Col: 8, Row: 8})
	if !s.dragOnPage {
		t.Fatal("a wheel mid-drag dropped the pointer capture")
	}
	s.handleMouse(MouseEvent{Action: MouseActionMove, Button: MouseButtonLeft, Col: 9, Row: 9})
	last := (*got)[len(*got)-1]
	if last.Action != MouseActionMove || last.Col != 9 {
		t.Fatalf("post-wheel drag move = %+v, want the forwarded move at column 9", last)
	}
}

// mouseRecordingEngine records forwarded keys and mouse events on one channel
// so a keySender test can assert cross-type ordering.
type mouseRecordingEngine struct {
	*fakeEngine
	got chan string
}

func (e *mouseRecordingEngine) SendKey(code string) { e.got <- "key:" + code }

func (e *mouseRecordingEngine) SendMouse(ev MouseEvent) {
	e.got <- fmt.Sprintf("mouse:%d@%d,%d", ev.Action, ev.Col, ev.Row)
}

// TestKeySenderOrdersMouseWithKeys asserts the shared worker forwards mouse
// events and scroll keys in submission order, so a wheel burst and a click
// dispatch as the user produced them.
func TestKeySenderOrdersMouseWithKeys(t *testing.T) {
	eng := &mouseRecordingEngine{fakeEngine: &fakeEngine{}, got: make(chan string, 4)}
	ks := newKeySender(eng)
	t.Cleanup(ks.close)

	ks.send("ArrowDown")
	ks.sendMouse(MouseEvent{Action: MouseActionWheel, WheelDY: 1, Col: 3, Row: 4})
	ks.sendMouse(MouseEvent{Action: MouseActionPress, Col: 3, Row: 4})
	ks.send("ArrowUp")

	want := []string{
		"key:ArrowDown",
		fmt.Sprintf("mouse:%d@3,4", MouseActionWheel),
		fmt.Sprintf("mouse:%d@3,4", MouseActionPress),
		"key:ArrowUp",
	}
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
