package viewer

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDecodeSGRMouse pins the SGR mouse decode table: buttons and finals,
// modifier bitmasks translated to CDP bits, wheel directions, motion
// recognition, low coordinate clamping, and malformed bodies dropped.
func TestDecodeSGRMouse(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   MouseEvent
		motion bool
		ok     bool
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
		{name: "drag motion recognised and dropped", body: "<32;5;5M", motion: true, ok: true},
		{name: "buttonless motion recognised and dropped", body: "<35;5;5m", motion: true, ok: true},
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
			got, motion, ok := decodeSGRMouse([]byte(tc.body))
			if ok != tc.ok || motion != tc.motion {
				t.Fatalf("decodeSGRMouse(%q) = (motion %t, ok %t), want (motion %t, ok %t)",
					tc.body, motion, ok, tc.motion, tc.ok)
			}
			if ok && !motion && got != tc.want {
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
// replies, motion reports, and plain keys survive interleaving on the one
// stdin parser: each event stays typed, the motion report is consumed
// silently, and no report leaks bytes as keys.
func TestNextEventMouseAPCInterleave(t *testing.T) {
	in := "\x1b_Gi=1;OK\x1b\\" + "\x1b[<0;3;2M" + "\x1b[<35;9;9M" + "\x1b[<65;4;4M" + "q"
	events := collectEvents(t, in)
	if len(events) != 4 {
		t.Fatalf("events = %d (%+v), want 4: apc, press, wheel, key", len(events), events)
	}
	if events[0].apc != "i=1;OK" {
		t.Errorf("event 0 apc = %q, want %q", events[0].apc, "i=1;OK")
	}
	press := events[1].mouse
	if press == nil || press.Action != MouseActionPress || press.Col != 2 || press.Row != 1 {
		t.Errorf("event 1 = %+v, want left press at cell (2, 1)", events[1])
	}
	wheel := events[2].mouse
	if wheel == nil || wheel.Action != MouseActionWheel || wheel.WheelDY != 1 {
		t.Errorf("event 2 = %+v, want wheel down", events[2])
	}
	if events[3].b != 'q' || events[3].mouse != nil {
		t.Errorf("event 3 = %+v, want plain key 'q'", events[3])
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
		if got := exitTerminal(24, drawn); !strings.Contains(got, disableMouseReporting) {
			t.Errorf("exitTerminal(24, %t) = %q, want it to contain %q", drawn, got, disableMouseReporting)
		}
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
