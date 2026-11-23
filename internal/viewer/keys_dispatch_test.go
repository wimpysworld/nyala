package viewer

import (
	"bytes"
	"image/color"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// dispatchSession builds a streamSession that dispatches keys synchronously
// (no keySender worker) onto the recording engine, so a test asserts the
// engine seam calls in order.
func dispatchSession(eng streamTestEngine, st *State) *streamSession {
	return &streamSession{eng: eng, st: st, tty: &bytes.Buffer{}}
}

// TestHandleKeyForwardsPageKeys pins the page-key forwarding: Tab, Shift+Tab
// (backtab), and Enter reach the engine as key codes for native focus
// traversal and activation, and none of them cycles the backend. Home and End
// forward as caret-key codes for the engine to stroke into the page.
func TestHandleKeyForwardsPageKeys(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
		want string
	}{
		{"tab", keyEvent{b: keyTab}, "Tab"},
		{"backtab", keyEvent{act: actionFocusPrev}, "ShiftTab"},
		{"enter", keyEvent{b: keyEnter}, "Enter"},
		{"home", keyEvent{scroll: ScrollHome}, "Home"},
		{"end", keyEvent{scroll: ScrollEnd}, "End"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			st.SetEnabled([]Backend{BackendKitty, BackendBlock})
			startBackend := st.Backend
			var sent []string
			var backends []string
			s := dispatchSession(streamTestEngine{
				sendKey:    func(code string) { sent = append(sent, code) },
				setBackend: func(name string) { backends = append(backends, name) },
			}, &st)

			if quit := s.handleKey(c.ev); quit {
				t.Fatalf("%s requested quit", c.name)
			}
			if len(sent) != 1 || sent[0] != c.want {
				t.Fatalf("SendKey calls = %v, want [%s]", sent, c.want)
			}
			if st.Backend != startBackend || len(backends) != 0 {
				t.Fatalf("%s cycled the backend: state %v, SetBackend calls %v", c.name, st.Backend, backends)
			}
		})
	}
}

// TestHandleKeyForwardsPageText pins the page-text forwarding: a printable
// byte, a Space, and an assembled multi-byte rune reach the engine's text seam
// with the typed character, and none of them takes the key-code seam. Space
// rides the text path so Chromium arbitrates it: it types in a focused field
// and scrolls otherwise.
func TestHandleKeyForwardsPageText(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
		want string
	}{
		{"letter", keyEvent{b: 'a'}, "a"},
		{"digit", keyEvent{b: '7'}, "7"},
		{"space", keyEvent{b: ' '}, " "},
		{"tilde", keyEvent{b: '~'}, "~"},
		{"two-byte rune", keyEvent{r: 'é'}, "é"},
		{"three-byte rune", keyEvent{r: '日'}, "日"},
		{"four-byte rune", keyEvent{r: '🙂'}, "🙂"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			var text []string
			var sent []string
			s := dispatchSession(streamTestEngine{
				sendText: func(t string) { text = append(text, t) },
				sendKey:  func(code string) { sent = append(sent, code) },
			}, &st)

			if quit := s.handleKey(c.ev); quit {
				t.Fatalf("%s requested quit", c.name)
			}
			if len(text) != 1 || text[0] != c.want {
				t.Fatalf("SendText calls = %q, want [%q]", text, c.want)
			}
			if len(sent) != 0 {
				t.Fatalf("%s also forwarded key codes %v, want none", c.name, sent)
			}
		})
	}
}

// TestHandleKeyDropsTerminalReplies pins the terminal-reply consumption at
// both ends of the seam. The Run loop in stream.go consumes an osc or dcs
// event before handleKey and is not reachable from this harness, so the test
// asserts the two halves the loop composes: the parser turns the raw reply
// bytes into one typed event that is not a key, and the dispatcher lets a
// reply event reach neither the text seam nor the key seam. On the reverted
// tree the raw bytes become an Escape event plus text, so the parse
// assertion fails there.
func TestHandleKeyDropsTerminalReplies(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"osc bel", "\x1b]11;rgb:1c1c/1c1c/1c1c\x07"},
		{"osc st", "\x1b]11;rgb:1c1c/1c1c/1c1c\x1b\\"},
		{"dcs st", "\x1bP1$r0m\x1b\\"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := collectEvents(t, c.in)
			if len(events) != 1 {
				t.Fatalf("events = %d (%+v), want one consumed reply", len(events), events)
			}
			ev := events[0]
			if ev.osc == "" && ev.dcs == "" {
				t.Fatalf("reply = %+v, want the body in osc or dcs", ev)
			}
			if ev.b != 0 || ev.r != 0 {
				t.Fatalf("reply parsed as a key: %+v", ev)
			}

			st := NewState(80, 24)
			var text, sent []string
			s := dispatchSession(streamTestEngine{
				sendText: func(t string) { text = append(text, t) },
				sendKey:  func(code string) { sent = append(sent, code) },
			}, &st)
			for _, forward := range []keyEvent{ev, {osc: "11;test"}, {dcs: "1$r0m"}} {
				if quit := s.handleKey(forward); quit {
					t.Fatalf("%+v requested quit", forward)
				}
			}
			if len(text) != 0 || len(sent) != 0 {
				t.Fatalf("a terminal reply reached the engine: text %q, keys %v", text, sent)
			}
		})
	}
}

// TestHandleKeyForwardsEditKeys pins the field-editing keys: both Backspace
// encodings (0x08 and DEL), Delete, and Esc forward as key codes for the engine
// to dispatch as full strokes, and none reaches the text seam.
func TestHandleKeyForwardsEditKeys(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
		want string
	}{
		{"backspace", keyEvent{b: keyBackspace}, "Backspace"},
		{"del", keyEvent{b: keyDel}, "Backspace"},
		{"delete", keyEvent{act: actionDelete}, "Delete"},
		{"esc", keyEvent{b: keyEsc}, "Escape"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			var sent []string
			var text []string
			s := dispatchSession(streamTestEngine{
				sendKey:  func(code string) { sent = append(sent, code) },
				sendText: func(t string) { text = append(text, t) },
			}, &st)

			if quit := s.handleKey(c.ev); quit {
				t.Fatalf("%s requested quit", c.name)
			}
			if len(sent) != 1 || sent[0] != c.want {
				t.Fatalf("SendKey calls = %v, want [%s]", sent, c.want)
			}
			if len(text) != 0 {
				t.Fatalf("%s reached the text seam: %q", c.name, text)
			}
		})
	}
}

// TestHandleKeySelectAllForwardsToPage pins the Ctrl+A binding: chrome mode
// forwards the select-all pseudo-code to the page rather than swallowing the
// byte, so a reader can select the document and copy it with Alt+W. It must not
// reach the text seam: Ctrl+A is a chord, not the letter a.
func TestHandleKeySelectAllForwardsToPage(t *testing.T) {
	st := NewState(80, 24)
	var sent, text []string
	s := dispatchSession(streamTestEngine{
		sendKey:  func(code string) { sent = append(sent, code) },
		sendText: func(t string) { text = append(text, t) },
	}, &st)

	if quit := s.handleKey(keyEvent{b: keyCtrlA}); quit {
		t.Fatal("Ctrl+A requested quit")
	}
	if len(sent) != 1 || sent[0] != "SelectAll" {
		t.Fatalf("SendKey calls = %v, want [SelectAll]", sent)
	}
	if len(text) != 0 {
		t.Fatalf("Ctrl+A reached the text seam: %q", text)
	}
}

// TestHandleKeyDropsNonText pins what chrome mode still drops: an unbound Ctrl
// chord is not text, so it reaches no engine seam.
func TestHandleKeyDropsNonText(t *testing.T) {
	st := NewState(80, 24)
	var text []string
	var sent []string
	s := dispatchSession(streamTestEngine{
		sendText: func(t string) { text = append(text, t) },
		sendKey:  func(code string) { sent = append(sent, code) },
	}, &st)

	for _, ev := range []keyEvent{{b: keyCtrlK}, {b: keyCtrlW}, {b: 0x00}} {
		if quit := s.handleKey(ev); quit {
			t.Fatalf("%+v requested quit", ev)
		}
	}
	if len(text) != 0 || len(sent) != 0 {
		t.Fatalf("dropped input reached the engine: text %q, keys %v", text, sent)
	}
}

// TestHandleKeyForwardsPaste pins the page paste route: a chrome-mode paste
// reaches the engine's paste seam as one unit, whatever its length, and never
// the typed-text or key seams. A one-character paste stays on the paste seam
// too, because a paste is not a key press.
func TestHandleKeyForwardsPaste(t *testing.T) {
	cases := []struct {
		name  string
		paste string
	}{
		{"ascii", "hello world"},
		{"multi-line", "one\ntwo\rthree"},
		{"multi-byte", "héllo 日本 🙂"},
		{"single character", "x"},
		{"single space", " "},
		{"at the bound", strings.Repeat("a", maxPasteBody)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			var pastes, text, sent []string
			s := dispatchSession(streamTestEngine{
				sendPaste: func(p string) { pastes = append(pastes, p) },
				sendText:  func(t string) { text = append(text, t) },
				sendKey:   func(code string) { sent = append(sent, code) },
			}, &st)

			if quit := s.handleKey(keyEvent{paste: c.paste}); quit {
				t.Fatalf("%s paste requested quit", c.name)
			}
			if len(pastes) != 1 || pastes[0] != c.paste {
				t.Fatalf("SendPaste calls = %d, want one carrying the payload whole", len(pastes))
			}
			if len(text) != 0 || len(sent) != 0 {
				t.Fatalf("%s paste leaked to the key seams: text %q, keys %v", c.name, text, sent)
			}
		})
	}
}

// TestBarModeKeepsPaste pins bar-mode paste ownership: a paste in URL edit mode
// goes into the editor line and a paste in menu mode is swallowed, so neither
// reaches the page.
func TestBarModeKeepsPaste(t *testing.T) {
	t.Run("edit inserts", func(t *testing.T) {
		var pastes []string
		s, _ := modeSession(t, streamTestEngine{
			sendPaste: func(p string) { pastes = append(pastes, p) },
		})
		s.handleKey(keyEvent{b: keyCtrlL})
		s.handleKey(keyEvent{b: keyCtrlU})
		s.handleKey(keyEvent{paste: "example.org"})
		if got := s.editor.String(); got != "example.org" {
			t.Fatalf("editor text = %q, want the paste inserted", got)
		}
		if len(pastes) != 0 {
			t.Fatalf("edit-mode paste reached the page: %q", pastes)
		}
	})
	t.Run("menu swallows", func(t *testing.T) {
		var pastes []string
		s, _ := modeSession(t, streamTestEngine{
			sendPaste: func(p string) { pastes = append(pastes, p) },
		})
		s.handleKey(keyEvent{act: actionOpenMenu})
		s.handleKey(keyEvent{paste: "example.org"})
		if s.barMode != barModeMenu {
			t.Fatalf("paste closed the menu: mode %d", s.barMode)
		}
		if len(pastes) != 0 {
			t.Fatalf("menu-mode paste reached the page: %q", pastes)
		}
	})
}

// TestBarModeKeepsSelectAllAsLineStart pins the readline binding the page
// chord must not take: Ctrl+A in the URL editor and the find field still moves
// the caret to the line start, on the legacy byte and on the kitty report the
// decode normalises to it, and never selects the page.
func TestBarModeKeepsSelectAllAsLineStart(t *testing.T) {
	kitty := collectEvents(t, "\x1b[97;5u")
	if len(kitty) != 1 || kitty[0].b != keyCtrlA || kitty[0].act != actionNone {
		t.Fatalf("kitty Ctrl+A report = %+v, want the legacy Ctrl+A byte event", kitty)
	}
	for _, ev := range []keyEvent{{b: keyCtrlA}, kitty[0]} {
		var sent []string
		s, _ := modeSession(t, streamTestEngine{
			sendKey: func(code string) { sent = append(sent, code) },
		})
		s.handleKey(keyEvent{b: keyCtrlL})
		for _, b := range []byte("example.org") {
			s.handleKey(keyEvent{b: b})
		}
		s.handleKey(ev)
		if s.editor.cursor != 0 {
			t.Fatalf("editor cursor after Ctrl+A = %d, want 0 (the line start)", s.editor.cursor)
		}
		if len(sent) != 0 {
			t.Fatalf("edit-mode Ctrl+A reached the page: %v", sent)
		}
	}
}

// TestBarModeKeepsDeleteAsForwardDelete pins the second readline binding the
// page must not take: Delete in the URL editor and in the find field still
// removes the rune under the bar's own cursor and never reaches the page. The
// bar's first claim in handleKey is the only thing separating the two, because
// chrome mode now forwards Delete to the focused page field.
func TestBarModeKeepsDeleteAsForwardDelete(t *testing.T) {
	modes := []struct {
		name  string
		enter keyEvent
	}{
		{"edit", keyEvent{b: keyCtrlL}},
		{"find", keyEvent{b: keyCtrlF}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			var sent []string
			s, _ := modeSession(t, streamTestEngine{
				sendKey: func(code string) { sent = append(sent, code) },
			})

			s.handleKey(m.enter)
			s.handleKey(keyEvent{b: keyCtrlU})
			for _, b := range []byte("abc") {
				s.handleKey(keyEvent{b: b})
			}
			// Park the caret at the line start, where a backspace has nothing to
			// take, so only a forward delete can change the line.
			s.handleKey(keyEvent{b: keyCtrlA})
			s.handleKey(keyEvent{act: actionDelete})

			if len(sent) != 0 {
				t.Fatalf("%s mode leaked keys to the page: %v", m.name, sent)
			}
			if got := s.editor.String(); got != "bc" {
				t.Fatalf("%s field after Delete = %q, want %q", m.name, got, "bc")
			}
		})
	}
}

// TestBarModeOwnsTextAndEscape pins the bar-mode first claim: while the URL
// editor or the menu is open, printable input, Delete and Esc belong to the
// mode, so Delete is the mode's own forward delete, Esc closes it and nothing
// reaches the page. Only after the mode closes does Esc forward to the page.
func TestBarModeOwnsTextAndEscape(t *testing.T) {
	modes := []struct {
		name  string
		enter keyEvent
	}{
		{"edit", keyEvent{b: keyCtrlL}},
		{"menu", keyEvent{act: actionOpenMenu}},
		{"find", keyEvent{b: keyCtrlF}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			var text []string
			var sent []string
			s, _ := modeSession(t, streamTestEngine{
				sendText: func(t string) { text = append(text, t) },
				sendKey:  func(code string) { sent = append(sent, code) },
			})

			s.handleKey(m.enter)
			if s.barMode == barModeChrome {
				t.Fatalf("%s mode did not open", m.name)
			}
			s.handleKey(keyEvent{b: 'a'})
			s.handleKey(keyEvent{r: 'é'})
			s.handleKey(keyEvent{act: actionDelete})
			s.handleKey(keyEvent{b: keyEsc})
			if s.barMode != barModeChrome {
				t.Fatalf("Esc did not close %s mode: mode %d", m.name, s.barMode)
			}
			if len(text) != 0 || len(sent) != 0 {
				t.Fatalf("%s mode leaked input: text %q, keys %v", m.name, text, sent)
			}

			s.handleKey(keyEvent{b: keyEsc})
			if len(sent) != 1 || sent[0] != "Escape" {
				t.Fatalf("chrome Esc = %v, want [Escape] once the mode closed", sent)
			}
		})
	}
}

// TestHandleKeyForwardsNav pins the navigation forwarding: Ctrl+R and F5
// reload, Alt+Left goes back, Alt+Right goes forward, and Alt+Home opens the
// launch page through the engine seam that holds the launch URL.
func TestHandleKeyForwardsNav(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
		want NavAction
	}{
		{"ctrl-r", keyEvent{b: keyCtrlR}, NavReload},
		{"f5", keyEvent{act: actionReload}, NavReload},
		{"alt-left", keyEvent{act: actionBack}, NavBack},
		{"alt-right", keyEvent{act: actionForward}, NavForward},
		{"alt-home", keyEvent{act: actionOpenHome}, NavHome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(80, 24)
			var navs []NavAction
			s := dispatchSession(streamTestEngine{
				sendNav: func(a NavAction) { navs = append(navs, a) },
			}, &st)

			if quit := s.handleKey(c.ev); quit {
				t.Fatalf("%s requested quit", c.name)
			}
			if len(navs) != 1 || navs[0] != c.want {
				t.Fatalf("SendNav calls = %v, want [%d]", navs, c.want)
			}
		})
	}
}

// TestGlobalAltBindingsAcrossBarModes checks each registered Alt binding through
// the legacy and Kitty decoders in chrome, About, edit, find, and menu modes.
func TestGlobalAltBindingsAcrossBarModes(t *testing.T) {
	bindings := []struct {
		name   string
		legacy string
		kitty  string
		action keyAction
	}{
		{"backend forward", "\x1br", "\x1b[114;3u", actionCycleBackend},
		{"backend reverse", "\x1bR", "\x1b[114;4u", actionCycleBackendReverse},
		{"blocks forward", "\x1bb", "\x1b[98;3u", actionCycleBlocks},
		{"blocks reverse", "\x1bB", "\x1b[98;4u", actionCycleBlocksReverse},
		{"colour forward", "\x1bc", "\x1b[99;3u", actionCycleColour},
		{"colour reverse", "\x1bC", "\x1b[99;4u", actionCycleColourReverse},
		{"palette", "\x1bp", "\x1b[112;3u", actionTogglePalette},
		{"menu", "\x1bf", "\x1b[102;3u", actionOpenMenu},
		{"copy", "\x1bw", "\x1b[119;3u", actionCopySelection},
		// Kitty uses the same modified CSI form as legacy terminals for these
		// functional keys when disambiguate-only mode is active.
		{"back", "\x1b[1;3D", "\x1b[1;3D", actionBack},
		{"forward", "\x1b[1;3C", "\x1b[1;3C", actionForward},
		{"home", "\x1b[1;3H", "\x1b[1;3H", actionOpenHome},
	}
	modes := []struct {
		name string
		mode barMode
	}{
		{"chrome", barModeChrome},
		{"About", barModeAbout},
		{"edit", barModeEdit},
		{"find", barModeFind},
		{"menu", barModeMenu},
	}

	for _, binding := range bindings {
		for _, source := range []struct {
			name string
			in   string
		}{
			{"legacy", binding.legacy},
			{"Kitty", binding.kitty},
		} {
			events := collectEvents(t, source.in)
			if len(events) != 1 || events[0].act != binding.action {
				t.Fatalf("%s %s decoded as %+v, want action %d", source.name, binding.name, events, binding.action)
			}
			for _, mode := range modes {
				t.Run(binding.name+"/"+source.name+"/"+mode.name, func(t *testing.T) {
					var navs []NavAction
					findClears := 0
					eng := streamTestEngine{
						sendNav:   func(a NavAction) { navs = append(navs, a) },
						findClear: func() { findClears++ },
						selection: func() (string, bool) { return "selected", false },
					}
					st := NewState(80, 24)
					st.SetEnabled([]Backend{BackendBlock, BackendKitty})
					st.sextantOK = true
					st.Colour = ColourXTerm256
					st.paletteQueryOK = true
					s := dispatchSession(eng, &st)
					s.version = "dev"
					s.ctl = &control{pending: make(chan captureConfig, 1)}
					s.barMode = mode.mode
					s.menuSel = 2
					if mode.mode == barModeEdit || mode.mode == barModeFind {
						s.editor = newURLEditor("needle")
					}
					if mode.mode == barModeFind {
						s.findQuery = "needle"
					}

					if quit := s.handleKey(events[0]); quit {
						t.Fatal("Alt binding requested quit")
					}

					wantMode := mode.mode
					switch binding.action {
					case actionOpenMenu:
						wantMode = barModeMenu
						if s.menuSel != 0 {
							t.Fatalf("menu selection = %d, want 0", s.menuSel)
						}
					case actionCopySelection:
						wantMode = barModeChrome
						if s.signal != "Copied 8 bytes" {
							t.Fatalf("copy signal = %q", s.signal)
						}
					case actionBack, actionForward, actionOpenHome:
						wantMode = barModeChrome
						wantNav := map[keyAction]NavAction{
							actionBack: NavBack, actionForward: NavForward, actionOpenHome: NavHome,
						}[binding.action]
						if len(navs) != 1 || navs[0] != wantNav {
							t.Fatalf("navigation calls = %v, want [%v]", navs, wantNav)
						}
					case actionCycleBackend, actionCycleBackendReverse:
						if st.Backend != BackendKitty {
							t.Fatalf("backend = %v, want Kitty", st.Backend)
						}
					case actionCycleBlocks, actionCycleBlocksReverse:
						if st.Scheme != block.SchemeSextant {
							t.Fatalf("scheme = %v, want sextant", st.Scheme)
						}
					case actionCycleColour:
						if st.Colour != ColourXTermGrey {
							t.Fatalf("colour = %v, want grey", st.Colour)
						}
					case actionCycleColourReverse:
						if st.Colour != ColourTrue {
							t.Fatalf("colour = %v, want true colour", st.Colour)
						}
					case actionTogglePalette:
						if !st.staticPaletteSelected {
							t.Fatal("palette policy stayed adaptive")
						}
					}
					if s.barMode != wantMode {
						t.Fatalf("bar mode = %d, want %d", s.barMode, wantMode)
					}
					closesMode := binding.action == actionOpenMenu || binding.action == actionCopySelection ||
						binding.action == actionBack || binding.action == actionForward || binding.action == actionOpenHome
					wantClears := 0
					if mode.mode == barModeFind && closesMode {
						wantClears = 1
					}
					if findClears != wantClears {
						t.Fatalf("find clears = %d, want %d", findClears, wantClears)
					}
					if closesMode && (mode.mode == barModeEdit || mode.mode == barModeFind) && s.editor != nil {
						t.Fatal("closed text mode kept its editor")
					}
					if !closesMode && !strings.Contains(s.tty.(*bytes.Buffer).String(), "\x1b[24;1H") {
						t.Fatal("render action did not repaint the current bar row")
					}
				})
			}
		}
	}
}

// TestUnboundAltHandlingAcrossBarModes keeps the two decoder fallbacks. A
// legacy Alt+Z passes through as Escape then text, while Kitty drops its report.
func TestUnboundAltHandlingAcrossBarModes(t *testing.T) {
	legacy := collectEvents(t, "\x1bz")
	if len(legacy) != 2 || legacy[0].b != keyEsc || legacy[1].b != 'z' {
		t.Fatalf("legacy Alt+Z = %+v, want Escape then z", legacy)
	}
	if kitty := collectEvents(t, "\x1b[122;3u"); len(kitty) != 0 {
		t.Fatalf("Kitty Alt+Z = %+v, want no event", kitty)
	}

	for _, mode := range []barMode{barModeChrome, barModeAbout, barModeEdit, barModeFind, barModeMenu} {
		var keys, text []string
		findClears := 0
		st := NewState(80, 24)
		s := dispatchSession(streamTestEngine{
			sendKey:   func(code string) { keys = append(keys, code) },
			sendText:  func(value string) { text = append(text, value) },
			findClear: func() { findClears++ },
		}, &st)
		s.barMode = mode
		if mode == barModeEdit || mode == barModeFind {
			s.editor = newURLEditor("value")
		}
		for _, ev := range legacy {
			s.handleKey(ev)
		}

		if s.barMode != barModeChrome || len(text) != 1 || text[0] != "z" {
			t.Fatalf("mode %d legacy passthrough left mode %d, keys %v, text %q", mode, s.barMode, keys, text)
		}
		wantKeys := 0
		if mode == barModeChrome {
			wantKeys = 1
		}
		if len(keys) != wantKeys || wantKeys == 1 && keys[0] != "Escape" {
			t.Fatalf("mode %d forwarded keys %v, want %d Escape keys", mode, keys, wantKeys)
		}
		wantClears := 0
		if mode == barModeFind {
			wantClears = 1
		}
		if findClears != wantClears {
			t.Fatalf("mode %d find clears = %d, want %d", mode, findClears, wantClears)
		}
	}
}

// TestGlobalAltNavigationClosesFindBeforeNavigation checks the lifecycle order:
// the viewer clears find state before it sends the navigation request.
func TestGlobalAltNavigationClosesFindBeforeNavigation(t *testing.T) {
	var calls []string
	st := NewState(80, 24)
	s := dispatchSession(streamTestEngine{
		findClear: func() { calls = append(calls, "clear") },
		sendNav:   func(NavAction) { calls = append(calls, "navigate") },
	}, &st)
	s.barMode = barModeFind
	s.editor = newURLEditor("needle")
	s.findQuery = "needle"

	s.handleKey(keyEvent{act: actionBack})

	if strings.Join(calls, ",") != "clear,navigate" {
		t.Fatalf("calls = %v, want [clear navigate]", calls)
	}
	if s.barMode != barModeChrome || s.editor != nil || s.findQuery != "" {
		t.Fatalf("find state stayed open: mode %d, editor %v, query %q", s.barMode, s.editor, s.findQuery)
	}
}

// TestHandleKeyQuit pins the quit policy: Ctrl+Q quits and Ctrl+C stays as the
// interrupt fallback, while plain q is inert (printable keys stay free for
// future page form input) and reaches no engine seam.
func TestHandleKeyQuit(t *testing.T) {
	st := NewState(80, 24)
	var sent []string
	var navs []NavAction
	s := dispatchSession(streamTestEngine{
		sendKey: func(code string) { sent = append(sent, code) },
		sendNav: func(a NavAction) { navs = append(navs, a) },
	}, &st)

	if !s.handleKey(keyEvent{b: keyQuitCtrlQ}) {
		t.Fatal("Ctrl+Q did not quit")
	}
	if !s.handleKey(keyEvent{b: keyQuitCtrlC}) {
		t.Fatal("Ctrl+C did not quit")
	}
	for _, b := range []byte{'q', 'Q'} {
		if s.handleKey(keyEvent{b: b}) {
			t.Fatalf("plain %q quit; quit is Ctrl+Q", b)
		}
	}
	if len(sent) != 0 || len(navs) != 0 {
		t.Fatalf("quit keys reached the engine: keys %v, navs %v", sent, navs)
	}
}

// TestHandleKeyCycleBackendBothWays pins Alt+R stepping the enabled cycle
// forwards and Alt+Shift+R stepping it backwards, wrapping at both ends.
func TestHandleKeyCycleBackendBothWays(t *testing.T) {
	st := NewState(80, 24)
	st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
	eng := streamTestEngine{}
	done := make(chan struct{})
	defer close(done)
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}

	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendSixel {
		t.Fatalf("after Alt+R, backend = %v, want Sixel", st.Backend)
	}
	s.handleKey(keyEvent{act: actionCycleBackendReverse})
	if st.Backend != BackendKitty {
		t.Fatalf("after Alt+Shift+R, backend = %v, want Kitty", st.Backend)
	}
	s.handleKey(keyEvent{act: actionCycleBackendReverse})
	if st.Backend != BackendBlock {
		t.Fatalf("reverse cycle did not wrap: backend = %v, want Block", st.Backend)
	}
}

// TestCycleScheme pins the Alt+B order quadrant -> sextant with wrap, the
// reverse order, and the no-op on a terminal that cannot self-draw sextant
// glyphs, where quadrant is the only renderable scheme.
func TestCycleScheme(t *testing.T) {
	st := NewState(80, 24)
	st.sextantOK = true
	st.Scheme = block.SchemeQuadrant
	want := []block.Scheme{block.SchemeSextant, block.SchemeQuadrant}
	for i, w := range want {
		if got := st.CycleScheme(false); got != w {
			t.Fatalf("cycle %d = %d, want %d", i, got, w)
		}
	}
	if got := st.CycleScheme(true); got != block.SchemeSextant {
		t.Fatalf("reverse cycle = %d, want sextant", got)
	}

	st.sextantOK = false
	st.Scheme = block.SchemeQuadrant
	if got := st.CycleScheme(false); got != block.SchemeQuadrant {
		t.Fatalf("unsupported sextant not skipped: got %d, want quadrant", got)
	}
	if got := st.CycleScheme(true); got != block.SchemeQuadrant {
		t.Fatalf("reverse skip = %d, want quadrant", got)
	}
}

// TestCycleColour pins the full Alt+C ladder in both directions for every cap.
func TestCycleColour(t *testing.T) {
	st := NewState(80, 24)
	want := []ColourMode{ColourXTerm256, ColourXTermGrey, ColourTrue}
	for i, w := range want {
		if got := st.CycleColour(false); got != w {
			t.Fatalf("cycle %d = %d, want %d", i, got, w)
		}
	}
	if got := st.CycleColour(true); got != ColourXTermGrey {
		t.Fatalf("reverse cycle = %d, want grey", got)
	}

	// The ladder is the cycle order filtered by one gate: the ordered modes at
	// or below the cap.
	for _, cap := range []ColourMode{ColourTrue, ColourXTerm256, ColourXTermGrey} {
		t.Run(strconv.Itoa(int(cap)), func(t *testing.T) {
			var allowed []ColourMode
			for _, mode := range colourCycle {
				if mode >= cap {
					allowed = append(allowed, mode)
				}
			}
			for i, current := range allowed {
				st.colourCap = cap
				st.Colour = current
				if got, want := st.CycleColour(false), allowed[(i+1)%len(allowed)]; got != want {
					t.Fatalf("cap %d forward from %d = %d, want %d", cap, current, got, want)
				}
				st.Colour = current
				if got, want := st.CycleColour(true), allowed[(i-1+len(allowed))%len(allowed)]; got != want {
					t.Fatalf("cap %d reverse from %d = %d, want %d", cap, current, got, want)
				}
			}
		})
	}
}

func TestDegradedColourModesUseTheirWriters(t *testing.T) {
	c := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	cases := []struct {
		mode   ColourMode
		wantFG string
		wantBG string
	}{
		{ColourXTermGrey, "\x1b[38;5;255m", "\x1b[48;5;255m"},
	}
	for _, tc := range cases {
		if got := string(fgSGRFunc(tc.mode, nil, false)(nil, c)); got != tc.wantFG {
			t.Errorf("mode %d foreground = %q, want %q", tc.mode, got, tc.wantFG)
		}
		if got := string(bgSGRFunc(tc.mode, nil, false)(nil, c)); got != tc.wantBG {
			t.Errorf("mode %d background = %q, want %q", tc.mode, got, tc.wantBG)
		}
	}
}

// TestXTerm256WritersFollowThePaletteVerdict proves the OSC 4 probe verdict,
// not the table alone, selects the 256 writer: with the verdict false the
// writers emit the static xterm-256 register even when a quantised table is
// attached, and with the verdict true they emit the register the table names.
// The two paths must differ on at least one sample, or the proof is inert.
func TestXTerm256WritersFollowThePaletteVerdict(t *testing.T) {
	_, lut := paletteTestState(t, true)
	differ := 0
	for _, sample := range []color.RGBA{
		{R: 255, G: 255, B: 255, A: 255},
		{R: 12, G: 200, B: 91, A: 255},
		{R: 200, G: 7, B: 43, A: 255},
		{R: 40, G: 40, B: 210, A: 255},
		{A: 255},
	} {
		static, adaptive := block.XTerm256(sample), block.PaletteIndex(lut, sample)
		if static != adaptive {
			differ++
		}
		for _, tc := range []struct {
			paletteOK bool
			prefix    string
			fn        func(ColourMode, []uint8, bool) block.SGRFunc
			want      uint8
		}{
			{false, fg256Prefix, fgSGRFunc, static},
			{false, bg256Prefix, bgSGRFunc, static},
			{true, fg256Prefix, fgSGRFunc, adaptive},
			{true, bg256Prefix, bgSGRFunc, adaptive},
		} {
			got := string(tc.fn(ColourXTerm256, lut, tc.paletteOK)(nil, sample))
			if want := string(appendSGR256(nil, tc.prefix, tc.want)); got != want {
				t.Errorf("paletteOK %t %q writer for %v = %q, want %q", tc.paletteOK, tc.prefix, sample, got, want)
			}
		}
	}
	if differ == 0 {
		t.Fatal("the static and the adaptive registers agreed on every sample, so the test proves nothing")
	}
}

// TestHandleKeyBlockCycleRecomposes pins the Alt+B and Alt+C repaint contract
// on the live Block backend: the differ resets, the clear goes out with the
// SGR reset first (the backend-switch clear rule), and the engine restarts at
// the unchanged grid so a static page re-emits a frame for the new
// composition.
func TestHandleKeyBlockCycleRecomposes(t *testing.T) {
	cases := []struct {
		name string
		ev   keyEvent
	}{
		{"alt-b", keyEvent{act: actionCycleBlocks}},
		{"alt-shift-b", keyEvent{act: actionCycleBlocksReverse}},
		{"alt-c", keyEvent{act: actionCycleColour}},
		{"alt-shift-c", keyEvent{act: actionCycleColourReverse}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := NewState(8, 4)
			st.SetEnabled([]Backend{BackendBlock})
			st.sextantOK = true
			diff := &block.GridDiff{}
			st.SetDiff(diff)
			// Seed the differ so an unchanged frame emits nothing; after the
			// cycle it must full-repaint.
			frame := render.Frame{Img: solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})}
			if err := st.RenderStreamFrame(frame, &bytes.Buffer{}); err != nil {
				t.Fatalf("seed Block frame: %v", err)
			}

			// The restart runs on the control goroutine, so collect it over a
			// channel rather than a shared slice.
			backendCh := make(chan string, 2)
			eng := streamTestEngine{setBackend: func(name string) { backendCh <- name }}
			done := make(chan struct{})
			defer close(done)
			out := &syncBuf{}
			s := &streamSession{eng: eng, st: &st, tty: out, ctl: newControl(eng, done), diff: diff}
			s.firstDrawn.Store(true)

			s.handleKey(c.ev)

			select {
			case name := <-backendCh:
				if name != "block" {
					t.Fatalf("cycle restarted the stream on %q, want block", name)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cycle did not restart the stream")
			}
			got := out.String()
			clearAt := strings.Index(got, clearScreen)
			if clearAt < 0 {
				t.Fatalf("cycle emitted no clear: %q", got)
			}
			if !strings.HasSuffix(got[:clearAt], sgrReset) {
				t.Fatalf("cycle clear did not reset the SGR first: %q", got)
			}
			var repaint bytes.Buffer
			if err := st.RenderStreamFrame(frame, &repaint); err != nil {
				t.Fatalf("post-cycle Block frame: %v", err)
			}
			if repaint.Len() == 0 {
				t.Fatal("differ was not reset: unchanged frame emitted nothing after the cycle")
			}
		})
	}
}

// TestHandleKeyBlockCycleOffBlockIsNoOp pins that Alt+B and Alt+C (and their
// reverse chords) taken while a graphics backend draws do nothing: no state
// change, no terminal write, no stream restart. Both cycles only affect Block
// rendering, so cycling off Block would change hidden state with no visible
// feedback and the next Block stint would arrive in an unexpected mode.
func TestHandleKeyBlockCycleOffBlockIsNoOp(t *testing.T) {
	backends := []struct {
		name    string
		backend Backend
	}{
		{"kitty", BackendKitty},
		{"sixel", BackendSixel},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			st := NewState(8, 4)
			st.SetEnabled([]Backend{b.backend, BackendBlock})
			st.sextantOK = true
			startScheme, startColour := st.Scheme, st.Colour

			var restarts []string
			eng := streamTestEngine{setBackend: func(name string) { restarts = append(restarts, name) }}
			out := &bytes.Buffer{}
			s := dispatchSession(eng, &st)
			s.tty = out
			s.firstDrawn.Store(true)

			s.handleKey(keyEvent{act: actionCycleBlocks})
			s.handleKey(keyEvent{act: actionCycleBlocksReverse})
			s.handleKey(keyEvent{act: actionCycleColour})
			s.handleKey(keyEvent{act: actionCycleColourReverse})

			if st.Scheme != startScheme || st.Colour != startColour {
				t.Fatalf("cycle off Block changed state: scheme %d->%d colour %d->%d",
					startScheme, st.Scheme, startColour, st.Colour)
			}
			if out.Len() != 0 {
				t.Fatalf("cycle off Block wrote to the terminal: %q", out.String())
			}
			if len(restarts) != 0 {
				t.Fatalf("cycle off Block restarted the stream: %v", restarts)
			}
		})
	}
}
