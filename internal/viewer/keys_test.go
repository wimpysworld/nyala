package viewer

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestDecodeCSI asserts the CSI body decoder maps the arrow finals A/B/C/D,
// the PageUp/PageDown bodies 5~/6~, and both encodings each of Home (H, 1~)
// and End (F, 4~) to their scroll keys, and rejects an unrecognised body.
func TestDecodeCSI(t *testing.T) {
	cases := []struct {
		body string
		want ScrollKey
		ok   bool
	}{
		{"A", ScrollUp, true},
		{"B", ScrollDown, true},
		{"C", ScrollRight, true},
		{"D", ScrollLeft, true},
		{"5~", ScrollPageUp, true},
		{"6~", ScrollPageDown, true},
		{"H", ScrollHome, true},
		{"1~", ScrollHome, true},
		{"7~", ScrollHome, true},
		{"F", ScrollEnd, true},
		{"4~", ScrollEnd, true},
		{"8~", ScrollEnd, true},
		{"99~", ScrollNone, false},
		{"", ScrollNone, false},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			got, ok := decodeCSI([]byte(c.body))
			if ok != c.ok || got != c.want {
				t.Fatalf("decodeCSI(%q) = (%d, %v), want (%d, %v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestDecodeCSIAction asserts the CSI body decoder maps the backtab (Z, how
// Shift+Tab arrives), F5 (15~), and the Alt-modified cursor keys (Alt+Left
// 1;3D, Alt+Right 1;3C, Alt+Home 1;3H and 1;3~) to their commands, and
// rejects an unrecognised body.
func TestDecodeCSIAction(t *testing.T) {
	cases := []struct {
		body string
		want keyAction
		ok   bool
	}{
		{"Z", actionFocusPrev, true},
		{"15~", actionReload, true},
		{"3~", actionDelete, true},
		{"1;3D", actionBack, true},
		{"1;3C", actionForward, true},
		{"1;3H", actionOpenHome, true},
		{"1;3~", actionOpenHome, true},
		{"99~", actionNone, false},
		{"", actionNone, false},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			got, ok := decodeCSIAction([]byte(c.body))
			if ok != c.ok || got != c.want {
				t.Fatalf("decodeCSIAction(%q) = (%d, %v), want (%d, %v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestByteKeyAction asserts the single-byte command map: Ctrl+Q quits (with
// Ctrl+C as the interrupt fallback), Ctrl+R reloads, Tab and Enter forward to
// the page, both Backspace encodings edit the focused field, Esc forwards to
// the page, and plain printable bytes, including q and Space, map to no
// command because they are page text, not a binding.
func TestByteKeyAction(t *testing.T) {
	cases := []struct {
		name string
		b    byte
		want keyAction
	}{
		{"ctrl-q", keyQuitCtrlQ, actionQuit},
		{"ctrl-c", keyQuitCtrlC, actionQuit},
		{"ctrl-r", keyCtrlR, actionReload},
		{"ctrl-l", keyCtrlL, actionEditURL},
		{"ctrl-f", keyCtrlF, actionOpenFind},
		{"tab", keyTab, actionFocusNext},
		{"enter", keyEnter, actionActivate},
		{"backspace", keyBackspace, actionBackspace},
		{"del", keyDel, actionBackspace},
		{"esc", keyEsc, actionEscape},
		{"plain q", 'q', actionNone},
		{"plain Q", 'Q', actionNone},
		{"space", ' ', actionNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := byteKeyAction(c.b); got != c.want {
				t.Fatalf("byteKeyAction(%#x) = %d, want %d", c.b, got, c.want)
			}
		})
	}
}

// TestPrintableByte asserts the page-text byte range: Space through tilde are
// text, while the control bytes below Space, DEL, and the UTF-8 lead and
// continuation bytes are not, so they never reach the page as typed text.
func TestPrintableByte(t *testing.T) {
	for _, b := range []byte{' ', '!', '0', 'a', 'Z', '~'} {
		if !printableByte(b) {
			t.Fatalf("printableByte(%#x) = false, want true", b)
		}
	}
	for _, b := range []byte{0x00, keyBackspace, keyTab, keyEnter, keyEsc, keyDel, 0xc3, 0xa9} {
		if printableByte(b) {
			t.Fatalf("printableByte(%#x) = true, want false", b)
		}
	}
}

// TestNextEventCSI asserts the reader resolves whole CSI escape sequences into
// scroll events: each arrow, PageUp/PageDown, Home, and End sequence yields
// one scroll keyEvent, a plain byte yields a byte event, an unrecognised CSI
// sequence is dropped, and a bare ESC passes through as a byte event. It reads
// from a bufio.Reader the way readKeys does, so it pins the multi-byte parse.
func TestNextEventCSI(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantScroll ScrollKey
		wantByte   byte
		wantEmit   bool
	}{
		{"up arrow", "\x1b[A", ScrollUp, 0, true},
		{"down arrow", "\x1b[B", ScrollDown, 0, true},
		{"right arrow", "\x1b[C", ScrollRight, 0, true},
		{"left arrow", "\x1b[D", ScrollLeft, 0, true},
		{"page up", "\x1b[5~", ScrollPageUp, 0, true},
		{"page down", "\x1b[6~", ScrollPageDown, 0, true},
		{"home", "\x1b[H", ScrollHome, 0, true},
		{"home vt", "\x1b[1~", ScrollHome, 0, true},
		{"end", "\x1b[F", ScrollEnd, 0, true},
		{"end vt", "\x1b[4~", ScrollEnd, 0, true},
		{"plain byte", "q", ScrollNone, 'q', true},
		{"tab byte", "\t", ScrollNone, keyTab, true},
		{"enter byte", "\r", ScrollNone, keyEnter, true},
		{"ctrl-q byte", "\x11", ScrollNone, keyQuitCtrlQ, true},
		{"ctrl-r byte", "\x12", ScrollNone, keyCtrlR, true},
		{"unknown csi dropped", "\x1b[99~", ScrollNone, 0, false},
		{"bare esc passthrough", "\x1bq", ScrollNone, keyEsc, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if emit != c.wantEmit {
				t.Fatalf("nextEvent emit = %v, want %v", emit, c.wantEmit)
			}
			if !emit {
				return
			}
			if ev.scroll != c.wantScroll {
				t.Fatalf("nextEvent scroll = %d, want %d", ev.scroll, c.wantScroll)
			}
			if ev.scroll == ScrollNone && ev.b != c.wantByte {
				t.Fatalf("nextEvent byte = %q, want %q", ev.b, c.wantByte)
			}
		})
	}
}

// TestNextEventCSIBodyBounded asserts the CSI reader drops a body that never
// reaches a final byte within maxCSIBody, matching the paste and APC caps, so
// garbage input cannot grow the buffer without limit.
func TestNextEventCSIBodyBounded(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\x1b[" + strings.Repeat("1;", maxCSIBody) + "A"))
	first, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	if _, emit := nextEvent(br, first); emit {
		t.Fatal("over-long CSI body emitted an event")
	}
}

// TestNextEventActions asserts the reader resolves the command escape
// sequences into action events: the Alt chords (ESC+letter, Shift carried by
// case), the backtab, F5, and the Alt-modified cursor keys. A trailing byte
// after each sequence pins that the parse consumes exactly the sequence.
func TestNextEventActions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want keyAction
	}{
		{"alt-r", "\x1br", actionCycleBackend},
		{"alt-shift-r", "\x1bR", actionCycleBackendReverse},
		{"alt-b", "\x1bb", actionCycleBlocks},
		{"alt-shift-b", "\x1bB", actionCycleBlocksReverse},
		{"alt-c", "\x1bc", actionCycleColour},
		{"alt-shift-c", "\x1bC", actionCycleColourReverse},
		{"alt-f", "\x1bf", actionOpenMenu},
		{"backtab", "\x1b[Z", actionFocusPrev},
		{"delete", "\x1b[3~", actionDelete},
		{"f5", "\x1b[15~", actionReload},
		{"alt-left", "\x1b[1;3D", actionBack},
		{"alt-right", "\x1b[1;3C", actionForward},
		{"alt-home", "\x1b[1;3H", actionOpenHome},
		{"alt-home vt", "\x1b[1;3~", actionOpenHome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in + "x"))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if !emit {
				t.Fatalf("nextEvent dropped %q", c.in)
			}
			if ev.act != c.want {
				t.Fatalf("nextEvent act = %d, want %d", ev.act, c.want)
			}
			rest, err := br.ReadByte()
			if err != nil || rest != 'x' {
				t.Fatalf("trailing byte after %q = %q, %v; want 'x'", c.in, rest, err)
			}
		})
	}
}

// TestReadKeysKeepsQuitKeysAfterDownArrow asserts a Down-arrow CSI sequence
// does not swallow later quit keys from the same input stream.
func TestReadKeysKeepsQuitKeysAfterDownArrow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want byte
	}{
		{"ctrl-q", "\x1b[B\x11", keyQuitCtrlQ},
		{"ctrl-c", "\x1b[B\x03", keyQuitCtrlC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan struct{})
			defer close(done)
			keys := readKeys(strings.NewReader(c.in), done)

			first, ok := <-keys
			if !ok {
				t.Fatalf("first event missing, want Down arrow")
			}
			if first.scroll != ScrollDown {
				t.Fatalf("first event scroll = %d, want ScrollDown", first.scroll)
			}

			second, ok := <-keys
			if !ok {
				t.Fatalf("second event missing, want %q", c.want)
			}
			if second.scroll != ScrollNone || second.b != c.want {
				t.Fatalf("second event = %+v, want byte %q", second, c.want)
			}
		})
	}
}

// TestNextEventUTF8Runes asserts the reader assembles multi-byte UTF-8 into
// whole rune events (two, three, and four byte forms), leaves ASCII as byte
// events, and drops stray continuation bytes and malformed sequences without
// swallowing the byte that follows them.
func TestNextEventUTF8Runes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want rune
	}{
		{"two byte", "é", 'é'},
		{"three byte", "→", '→'},
		{"four byte", "\U0001f9a1", '\U0001f9a1'},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in + "x"))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if !emit {
				t.Fatalf("nextEvent dropped %q", c.in)
			}
			if ev.r != c.want {
				t.Fatalf("nextEvent rune = %q, want %q", ev.r, c.want)
			}
			rest, err := br.ReadByte()
			if err != nil || rest != 'x' {
				t.Fatalf("trailing byte after %q = %q, %v; want 'x'", c.in, rest, err)
			}
		})
	}

	t.Run("stray continuation dropped", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\x80x"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("stray continuation byte emitted an event")
		}
		if rest, err := br.ReadByte(); err != nil || rest != 'x' {
			t.Fatalf("byte after stray continuation = %q, %v; want 'x'", rest, err)
		}
	})
	t.Run("cut sequence unreads the cutter", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\xc3x"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("cut UTF-8 sequence emitted an event")
		}
		if rest, err := br.ReadByte(); err != nil || rest != 'x' {
			t.Fatalf("byte after cut sequence = %q, %v; want 'x'", rest, err)
		}
	})
}

// TestNextEventLoneEsc asserts the buffered-byte lone-Esc detection: an ESC
// with nothing buffered behind it (a bare Esc key press arrives alone in its
// read burst) emits an ESC byte event immediately, while an ESC with a
// buffered chord letter behind it stays an Alt chord.
func TestNextEventLoneEsc(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\x1b"))
	first, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	ev, emit := nextEvent(br, first)
	if !emit || ev.b != keyEsc || ev.act != actionNone {
		t.Fatalf("lone Esc = (%+v, %v), want an ESC byte event", ev, emit)
	}

	br = bufio.NewReader(strings.NewReader("\x1br"))
	first, _ = br.ReadByte()
	ev, emit = nextEvent(br, first)
	if !emit || ev.act != actionCycleBackend {
		t.Fatalf("buffered chord = (%+v, %v), want the Alt+R action", ev, emit)
	}
}

// TestNextEventBracketedPaste asserts a bracketed paste (CSI 200~ to
// CSI 201~) arrives as one paste event carrying the payload whole, including
// newlines and embedded escape bytes, and that the byte after the terminator
// still arrives as a key. An empty paste is dropped.
func TestNextEventBracketedPaste(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "\x1b[200~https://example.org/\x1b[201~", "https://example.org/"},
		{"multi-line kept whole", "\x1b[200~one\ntwo\rthree\x1b[201~", "one\ntwo\rthree"},
		{"embedded escape", "\x1b[200~a\x1b[Zb\x1b[201~", "a\x1b[Zb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(c.in + "x"))
			first, err := br.ReadByte()
			if err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			ev, emit := nextEvent(br, first)
			if !emit {
				t.Fatalf("nextEvent dropped the paste %q", c.in)
			}
			if ev.paste != c.want {
				t.Fatalf("paste = %q, want %q", ev.paste, c.want)
			}
			rest, err := br.ReadByte()
			if err != nil || rest != 'x' {
				t.Fatalf("trailing byte after paste = %q, %v; want 'x'", rest, err)
			}
		})
	}

	t.Run("empty paste dropped", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\x1b[200~\x1b[201~x"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("empty paste emitted an event")
		}
		if rest, err := br.ReadByte(); err != nil || rest != 'x' {
			t.Fatalf("byte after empty paste = %q, %v; want 'x'", rest, err)
		}
	})
	t.Run("unterminated paste dropped", func(t *testing.T) {
		br := bufio.NewReader(strings.NewReader("\x1b[200~orphan"))
		first, _ := br.ReadByte()
		if _, emit := nextEvent(br, first); emit {
			t.Fatal("unterminated paste emitted an event")
		}
	})
}

// TestDecodeKittyKey pins the kitty keyboard-protocol decode table: the reports
// Nyala consumes map to the same actions the legacy parser produces for the same
// physical keys, the modifier bits ride along in CDP currency, and anything
// unbound or malformed is refused so it is dropped rather than leaked.
func TestDecodeKittyKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		want keyAction
		mod  int
		ok   bool
	}{
		{"escape", "27u", actionEscape, 0, true},
		{"escape with a modifier still escapes", "27;5u", actionEscape, cdpModCtrl, true},
		{"enter", "13u", actionActivate, 0, true},
		{"shift enter", "13;2u", actionActivate, cdpModShift, true},
		{"tab", "9u", actionFocusNext, 0, true},
		{"shift tab", "9;2u", actionFocusPrev, cdpModShift, true},
		{"backspace", "127u", actionBackspace, 0, true},
		{"ctrl q quits", "113;5u", actionQuit, cdpModCtrl, true},
		{"ctrl c quits", "99;5u", actionQuit, cdpModCtrl, true},
		{"ctrl r reloads", "114;5u", actionReload, cdpModCtrl, true},
		{"ctrl l edits the URL", "108;5u", actionEditURL, cdpModCtrl, true},
		{"ctrl f opens find", "102;5u", actionOpenFind, cdpModCtrl, true},
		{"ctrl a selects all", "97;5u", actionSelectAll, cdpModCtrl, true},
		{"alt r cycles the backend", "114;3u", actionCycleBackend, cdpModAlt, true},
		{"alt shift r reverses the cycle", "114;4u", actionCycleBackendReverse, cdpModAlt | cdpModShift, true},
		{"alt b cycles blocks", "98;3u", actionCycleBlocks, cdpModAlt, true},
		{"alt shift b reverses blocks", "98;4u", actionCycleBlocksReverse, cdpModAlt | cdpModShift, true},
		{"alt c cycles colour", "99;3u", actionCycleColour, cdpModAlt, true},
		{"alt shift c reverses colour", "99;4u", actionCycleColourReverse, cdpModAlt | cdpModShift, true},
		{"alt f opens the menu", "102;3u", actionOpenMenu, cdpModAlt, true},
		{"upper-case key code lower-cases", "82;3u", actionCycleBackend, cdpModAlt, true},
		{"sub-parameters are cut", "13:65;2:1u", actionActivate, cdpModShift, true},
		// Unbound and malformed reports are refused, so nextEvent drops them.
		{"plain letter is not a chord", "113u", actionNone, 0, false},
		{"shift letter is not a chord", "113;2u", actionNone, 0, false},
		{"alt with no binding", "122;3u", actionNone, 0, false},
		{"ctrl with no binding", "122;5u", actionNone, 0, false},
		{"unknown functional key", "57358u", actionNone, 0, false},
		{"flags reply is not a key", "?1u", actionNone, 0, false},
		{"non-numeric code", "abcu", actionNone, 0, false},
		{"non-numeric modifier", "13;xu", actionNone, 0, false},
		{"zero modifier is malformed", "13;0u", actionNone, 0, false},
		{"wrong final", "13;2A", actionNone, 0, false},
		{"empty body", "u", actionNone, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act, mod, ok := decodeKittyKey([]byte(c.body))
			if ok != c.ok || act != c.want || mod != c.mod {
				t.Fatalf("decodeKittyKey(%q) = (%d, %d, %v), want (%d, %d, %v)",
					c.body, act, mod, ok, c.want, c.mod, c.ok)
			}
		})
	}
}

// TestNextEventKittyKeyReports asserts the reader resolves kitty key reports
// through the same CSI path as every other sequence, and normalises each one to
// the event a legacy terminal would have produced: Escape becomes the ESC byte,
// Shift+Enter becomes the Enter byte carrying the Shift bit (the signal
// find-in-page needs for previous-match), a chord with no legacy byte stays an
// action, an unbound report is consumed silently, and a late flags reply from the
// startup query never leaks bytes as keys.
func TestNextEventKittyKeyReports(t *testing.T) {
	events := collectEvents(t, "\x1b[27u"+"\x1b[13;2u"+"\x1b[114;3u"+"\x1b[57358u"+"\x1b[?1u"+"q")
	if len(events) != 4 {
		t.Fatalf("events = %d (%+v), want 4: escape, shift-enter, alt-r, and the key", len(events), events)
	}
	if events[0].b != keyEsc || events[0].act != actionNone || events[0].mod != 0 {
		t.Errorf("event 0 = %+v, want the ESC byte event a legacy terminal sends", events[0])
	}
	if byteKeyAction(events[0].b) != actionEscape {
		t.Errorf("normalised Escape does not dispatch as Escape: %+v", events[0])
	}
	if events[1].b != keyEnter || events[1].mod != cdpModShift {
		t.Errorf("event 1 = %+v, want the Enter byte carrying the Shift bit", events[1])
	}
	if events[2].act != actionCycleBackend || events[2].mod != cdpModAlt {
		t.Errorf("event 2 = %+v, want the Alt+R cycle", events[2])
	}
	if events[3].b != 'q' || events[3].act != actionNone {
		t.Errorf("event 3 = %+v, want the plain key 'q'", events[3])
	}

	// Plain and shifted Enter both dispatch as activate, and only the shifted one
	// carries Shift, so find mode can tell them apart on either path.
	for _, in := range []string{"\r", "\x1b[13u"} {
		plain := collectEvents(t, in)
		if len(plain) != 1 || byteKeyAction(plain[0].b) != actionActivate || plain[0].mod != 0 {
			t.Fatalf("Enter from %q = %+v, want an unmodified activate", in, plain)
		}
	}
	shifted := collectEvents(t, "\x1b[13;2u")
	if len(shifted) != 1 || byteKeyAction(shifted[0].b) != actionActivate || shifted[0].mod&cdpModShift == 0 {
		t.Fatalf("Shift+Enter = %+v, want an activate carrying Shift", shifted)
	}

	// Ctrl+Q and Backspace reports normalise to the bytes every mode already
	// binds, including the pre-navigation quit check.
	quit := collectEvents(t, "\x1b[113;5u")
	if len(quit) != 1 || !startupQuit(quit[0]) {
		t.Fatalf("Ctrl+Q report = %+v, want it to satisfy the startup quit check", quit)
	}
	back := collectEvents(t, "\x1b[127u")
	if len(back) != 1 || byteKeyAction(back[0].b) != actionBackspace {
		t.Fatalf("Backspace report = %+v, want the backspace binding", back)
	}
}

// TestKittyKeyReportsDriveBarModes asserts the protocol's Escape reaches the
// bar-mode handlers, which bind Esc by byte: the URL editor cancels and the menu
// and About rows close, exactly as they do on a legacy terminal. Without the
// normalisation an Escape report would arrive as an action those handlers swallow,
// leaving a modal no one could close.
func TestKittyKeyReportsDriveBarModes(t *testing.T) {
	escape := collectEvents(t, "\x1b[27u")
	if len(escape) != 1 {
		t.Fatalf("Escape report = %+v, want one event", escape)
	}

	t.Run("edit cancels", func(t *testing.T) {
		s, _ := modeSession(t, streamTestEngine{currentURL: "https://example.org/"})
		s.handleKey(keyEvent{b: keyCtrlL})
		if s.barMode != barModeEdit {
			t.Fatalf("Ctrl+L did not open the editor: mode %d", s.barMode)
		}
		s.handleKey(escape[0])
		if s.barMode != barModeChrome || s.editor != nil {
			t.Fatalf("an Escape report did not cancel the editor: mode %d", s.barMode)
		}
	})
	t.Run("menu closes", func(t *testing.T) {
		s, _ := modeSession(t, streamTestEngine{})
		s.handleKey(keyEvent{act: actionOpenMenu})
		s.handleKey(escape[0])
		if s.barMode != barModeChrome {
			t.Fatalf("an Escape report did not close the menu: mode %d", s.barMode)
		}
	})
	t.Run("about closes", func(t *testing.T) {
		s, _ := modeSession(t, streamTestEngine{})
		s.barMode = barModeAbout
		s.handleKey(escape[0])
		if s.barMode != barModeChrome {
			t.Fatalf("an Escape report did not close About: mode %d", s.barMode)
		}
	})
	t.Run("chrome forwards to the page", func(t *testing.T) {
		st := NewState(80, 24)
		var sent []string
		s := dispatchSession(streamTestEngine{
			sendKey: func(code string) { sent = append(sent, code) },
		}, &st)
		s.handleKey(escape[0])
		if len(sent) != 1 || sent[0] != "Escape" {
			t.Fatalf("chrome Escape report forwarded %v, want [Escape]", sent)
		}
	})
}

// TestNextEventLegacyKeepsEscapeAndAltChords asserts the legacy path is
// untouched by the protocol decode: a bare Esc still emits an ESC byte through
// the buffered-byte check, an Alt chord still resolves from ESC then the letter
// with Shift carried by its case, and the legacy Ctrl and page keys still
// arrive as bytes. Terminals without the protocol depend on all of this.
func TestNextEventLegacyKeepsEscapeAndAltChords(t *testing.T) {
	bare := collectEvents(t, "\x1b")
	if len(bare) != 1 || bare[0].b != keyEsc || bare[0].act != actionNone {
		t.Fatalf("bare Esc = %+v, want one ESC byte event", bare)
	}

	chords := collectEvents(t, "\x1br"+"\x1bR"+"\x1bf")
	if len(chords) != 3 {
		t.Fatalf("Alt chords = %d events (%+v), want 3", len(chords), chords)
	}
	wantChords := []keyAction{actionCycleBackend, actionCycleBackendReverse, actionOpenMenu}
	for i, want := range wantChords {
		if chords[i].act != want || chords[i].mod != 0 {
			t.Fatalf("legacy chord %d = %+v, want action %d with no modifier bits", i, chords[i], want)
		}
	}

	bytes := collectEvents(t, "\x11"+"\x7f"+"a"+" ")
	if len(bytes) != 4 {
		t.Fatalf("legacy bytes = %d events (%+v), want 4", len(bytes), bytes)
	}
	if byteKeyAction(bytes[0].b) != actionQuit {
		t.Errorf("legacy Ctrl+Q = %+v, want the quit action", bytes[0])
	}
	if byteKeyAction(bytes[1].b) != actionBackspace {
		t.Errorf("legacy Backspace = %+v, want the backspace action", bytes[1])
	}
	if !printableByte(bytes[2].b) || !printableByte(bytes[3].b) {
		t.Errorf("legacy printables = %+v, want page text", bytes[2:])
	}
}

// TestNextEventOSCAndDCSReplies asserts the reader consumes a terminal OSC
// reply (both its BEL and ST terminators) and a DCS reply (ST only) as one
// typed event each, carrying the body in the osc or dcs field, so the reply
// bytes never leak as rune or byte key events. A trailing key after each
// reply pins that the parse consumes exactly the sequence.
func TestNextEventOSCAndDCSReplies(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOSC string
		wantDCS string
	}{
		{"osc bel", "\x1b]11;rgb:1c1c/1c1c/1c1c\x07", "11;rgb:1c1c/1c1c/1c1c", ""},
		{"osc st", "\x1b]11;rgb:1c1c/1c1c/1c1c\x1b\\", "11;rgb:1c1c/1c1c/1c1c", ""},
		{"dcs st", "\x1bP1$r0m\x1b\\", "", "1$r0m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := collectEvents(t, c.in+"q")
			if len(events) != 2 {
				t.Fatalf("events = %d (%+v), want 2: the reply and the key", len(events), events)
			}
			ev := events[0]
			if ev.osc != c.wantOSC || ev.dcs != c.wantDCS {
				t.Fatalf("reply = %+v, want osc %q dcs %q", ev, c.wantOSC, c.wantDCS)
			}
			if ev.b != 0 || ev.r != 0 || ev.act != actionNone || ev.scroll != ScrollNone {
				t.Fatalf("reply leaked as a key: %+v", ev)
			}
			if events[1].b != 'q' || events[1].osc != "" || events[1].dcs != "" {
				t.Fatalf("trailing key = %+v, want the plain key 'q'", events[1])
			}
		})
	}
}

// TestNextEventOSCBodyBounded asserts an OSC or DCS body longer than
// maxAPCBody is consumed to its own terminator and dropped as one whole
// event: no byte of the over-long reply arrives as a key event, the buffer
// stays bounded, and the key after the terminator still arrives.
func TestNextEventOSCBodyBounded(t *testing.T) {
	long := strings.Repeat("a", maxAPCBody+1)
	cases := []struct {
		name string
		in   string
	}{
		{"osc bel", "\x1b]" + long + "\x07" + "\x1b[A"},
		{"osc st", "\x1b]" + long + "\x1b\\" + "\x1b[A"},
		{"dcs st", "\x1bP" + long + "\x1b\\" + "\x1b[A"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := collectEvents(t, c.in)
			if len(events) != 1 {
				t.Fatalf("events = %d (%+v), want 1: only the Up arrow after the drop", len(events), events)
			}
			if events[0].scroll != ScrollUp || events[0].osc != "" || events[0].dcs != "" {
				t.Fatalf("event after the drop = %+v, want ScrollUp", events[0])
			}
		})
	}
}

// TestNextEventOSCKeepsEscBehaviour asserts the OSC and DCS arms do not
// disturb the ESC paths that share the introducer byte: a bare Esc press
// stays an ESC byte event and ESC r stays the Alt+R chord.
func TestNextEventOSCKeepsEscBehaviour(t *testing.T) {
	bare := collectEvents(t, "\x1b")
	if len(bare) != 1 || bare[0].b != keyEsc || bare[0].osc != "" || bare[0].dcs != "" {
		t.Fatalf("bare Esc = %+v, want one ESC byte event", bare)
	}
	chord := collectEvents(t, "\x1br")
	if len(chord) != 1 || chord[0].act != actionCycleBackend {
		t.Fatalf("ESC r = %+v, want the Alt+R chord", chord)
	}
}

// TestReadKeysOSCSplitWrite asserts a reply cut at a write boundary is still
// one event with no loss: the terminal delivers the terminator in a second
// write, the reader blocks on it, and the reply plus the following key arrive
// as exactly two events.
func TestReadKeysOSCSplitWrite(t *testing.T) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	defer close(done)
	keys := readKeys(pr, done)
	go func() {
		defer pw.Close()
		if _, err := pw.Write([]byte("\x1b]11;rgb:1c1c/1c1c")); err != nil {
			return
		}
		if _, err := pw.Write([]byte("/1c1c\x07q")); err != nil {
			return
		}
	}()

	first, ok := <-keys
	if !ok || first.osc != "11;rgb:1c1c/1c1c/1c1c" {
		t.Fatalf("split reply = %+v (ok %v), want the whole OSC body", first, ok)
	}
	if first.b != 0 || first.r != 0 {
		t.Fatalf("split reply leaked as a key: %+v", first)
	}
	second, ok := <-keys
	if !ok || second.b != 'q' {
		t.Fatalf("key after the split reply = %+v (ok %v), want 'q'", second, ok)
	}
}

// TestNextEventPasteBound pins the 1 MiB paste bound now that a paste reaches
// the page: a payload at the bound is delivered whole, and one byte past it is
// rejected outright rather than truncated, so an oversized paste never reaches
// the browser and cannot grow the reader's buffer without limit.
func TestNextEventPasteBound(t *testing.T) {
	if maxPasteBody != 1<<20 {
		t.Fatalf("maxPasteBody = %d, want 1 MiB", maxPasteBody)
	}
	atBound := strings.Repeat("a", maxPasteBody)
	br := bufio.NewReader(strings.NewReader("\x1b[200~" + atBound + pasteEnd))
	first, _ := br.ReadByte()
	ev, emit := nextEvent(br, first)
	if !emit || len(ev.paste) != maxPasteBody {
		t.Fatalf("paste at the bound = %d bytes (emit %v), want %d delivered whole",
			len(ev.paste), emit, maxPasteBody)
	}

	over := strings.Repeat("a", maxPasteBody+1)
	br = bufio.NewReader(strings.NewReader("\x1b[200~" + over + pasteEnd))
	first, _ = br.ReadByte()
	if ev, emit := nextEvent(br, first); emit {
		t.Fatalf("oversized paste emitted %d bytes, want rejection", len(ev.paste))
	}
}
