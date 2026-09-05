package viewer

import (
	"bytes"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

func automaticFallbackState(flag string) State {
	st := NewState(80, 24)
	st.SetEnabled([]Backend{BackendBlock})
	st.configureColour(flag, nil, ColourTrue, true)
	st.paletteQueryOK = true
	return st
}

func armAutomaticFallback(s *streamSession) {
	s.writePressure.sampledWrites = writePressureStartupSamples
	s.writePressure.slowSamples = writePressureSustainedSamples - 1
}

func TestAutomaticColourFallbackEligibility(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*State, *streamSession)
		want    bool
	}{
		{"automatic True Colour Block", func(*State, *streamSession) {}, true},
		{"explicit True Colour", func(st *State, _ *streamSession) { st.configureColour("truecolor", nil, ColourTrue, true) }, false},
		{"automatic 256 selection", func(st *State, _ *streamSession) { st.configureColour("auto", nil, ColourXTerm256, true) }, false},
		{"missing OSC 4", func(st *State, _ *streamSession) { st.paletteQueryOK = false }, false},
		{"static palette", func(st *State, _ *streamSession) { st.staticPaletteSelected = true }, false},
		{"Kitty backend", func(st *State, _ *streamSession) { st.Backend = BackendKitty }, false},
		{"Sixel backend", func(st *State, _ *streamSession) { st.Backend = BackendSixel }, false},
		{"256 colour", func(st *State, _ *streamSession) { st.Colour = ColourXTerm256 }, false},
		{"grey colour", func(st *State, _ *streamSession) { st.Colour = ColourXTermGrey }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := automaticFallbackState("auto")
			s := &streamSession{st: &st}
			tc.prepare(&st, s)
			if got := s.writePressureEligible(); got != tc.want {
				t.Fatalf("writePressureEligible() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestEveryColourFlagStillCyclesInAbout(t *testing.T) {
	for _, flag := range []string{"auto", "truecolor", "256", "grey"} {
		t.Run(flag, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				reverse bool
			}{
				{"Alt+C", false},
				{"Alt+Shift+C", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					st := automaticFallbackState(flag)
					before := st.Colour
					eng := streamTestEngine{}
					done := make(chan struct{})
					defer close(done)
					s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done), barMode: barModeAbout}

					act := actionCycleColour
					if tc.reverse {
						act = actionCycleColourReverse
					}
					s.handleKey(keyEvent{act: act})

					if st.Colour == before {
						t.Fatalf("%s left %q at colour %v", tc.name, flag, st.Colour)
					}
					if s.barMode != barModeAbout {
						t.Fatalf("%s closed About", tc.name)
					}
					if !strings.Contains(s.tty.(*bytes.Buffer).String(), "Colour:") {
						t.Fatalf("%s did not refresh About", tc.name)
					}
				})
			}
		})
	}
}

func TestColourCycleRearmsAutomaticFallbackOnTrueColour(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    string
		reverse bool
		want    bool
	}{
		{"automatic Alt+C", "auto", false, true},
		{"automatic Alt+Shift+C", "auto", true, true},
		{"explicit Alt+C", "truecolor", false, false},
		{"explicit Alt+Shift+C", "truecolor", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := automaticFallbackState(tc.flag)
			eng := streamTestEngine{}
			done := make(chan struct{})
			defer close(done)
			s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}
			s.writePressure.reported = true
			oldGeneration := s.writePressureGen

			for range colourCycle {
				s.cycleColour(tc.reverse)
			}

			if st.Colour != ColourTrue {
				t.Fatalf("colour = %v, want True Colour", st.Colour)
			}
			if s.writePressure != (writePressureObserver{}) || s.writePressureGen == oldGeneration {
				t.Fatalf("cycle kept observer %+v or generation %d", s.writePressure, s.writePressureGen)
			}
			if got := s.writePressureEligible(); got != tc.want {
				t.Fatalf("writePressureEligible() = %t, want %t", got, tc.want)
			}
			armAutomaticFallback(s)
			s.observeWritePressure(freeItem{
				pressureSample: true,
				pressureGen:    s.writePressureGen,
				writeDur:       writePressureThreshold,
			})
			wantColour := ColourTrue
			if tc.want {
				wantColour = ColourXTerm256
			}
			if st.Colour != wantColour {
				t.Fatalf("colour after pressure = %v, want %v", st.Colour, wantColour)
			}
		})
	}
}

func TestAutomaticColourFallbackOrdersTransitionAndNextPalette(t *testing.T) {
	st := automaticFallbackState("auto")
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	tty := &slowWriter{}
	w := newStreamWriter(tty)
	s := &streamSession{
		eng: eng, st: &st, tty: tty, writer: w, ctl: newControl(eng, done), diff: diff,
		barMode: barModeAbout, version: "dev",
	}
	queued := bytes.NewBufferString("QUEUED_TRUE_COLOUR")
	w.send(writeItem{buf: queued})
	stale := bytes.NewBufferString("STALE_TRUE_COLOUR")
	s.assembled = stale
	armAutomaticFallback(s)

	s.observeWritePressure(freeItem{
		pressureSample: true,
		pressureGen:    s.writePressureGen,
		writeDur:       writePressureThreshold,
	})
	w.stop()
	s.writer = nil

	if st.Colour != ColourXTerm256 || st.staticPaletteSelected {
		t.Fatalf("fallback colour = %v, static = %t, want 240 Adaptive", st.Colour, st.staticPaletteSelected)
	}
	out := tty.String()
	queuedAt := strings.Index(out, "QUEUED_TRUE_COLOUR")
	staleAt := strings.Index(out, "STALE_TRUE_COLOUR")
	clearAt := strings.Index(out, sgrReset+clearScreen)
	barAt := strings.Index(out, "Colour: 240 Adaptive")
	if queuedAt < 0 || staleAt <= queuedAt || clearAt <= staleAt || barAt <= clearAt {
		t.Fatalf("fallback output order = %q", out)
	}
	wantRestart := []string{"SetBackend(block)"}
	if !waitFor(2*time.Second, func() bool { return strings.Join(eng.calls(), ",") == strings.Join(wantRestart, ",") }) {
		t.Fatalf("fallback restart calls = %v, want %v", eng.calls(), wantRestart)
	}

	var next bytes.Buffer
	frame := render.Frame{Img: solidImage(color.RGBA{R: 0x20, G: 0x50, B: 0x90, A: 0xff})}
	if err := st.RenderStreamFrame(frame, &next); err != nil {
		t.Fatalf("render after fallback: %v", err)
	}
	oscAt := strings.Index(next.String(), osc4Introducer)
	cellAt := strings.Index(next.String(), "\x1b[38;5;")
	if oscAt < 0 || cellAt < 0 || oscAt >= cellAt {
		t.Fatalf("post-fallback frame did not write OSC 4 before indexed cells: %q", next.String())
	}
}

func TestAutomaticColourFallbackRefreshesChromeBadge(t *testing.T) {
	st := automaticFallbackState("auto")
	eng := streamTestEngine{}
	done := make(chan struct{})
	defer close(done)
	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: newControl(eng, done)}
	s.firstDrawn.Store(true)
	armAutomaticFallback(s)

	s.observeWritePressure(freeItem{
		pressureSample: true,
		pressureGen:    s.writePressureGen,
		writeDur:       writePressureThreshold,
	})

	if !strings.Contains(out.String(), "│ ⬒  │ 240a │ ☰") {
		t.Fatalf("fallback bar %q missing the adaptive badge", out.String())
	}
}

func TestAutomaticColourFallbackIsOneShot(t *testing.T) {
	st := automaticFallbackState("auto")
	eng := &switchOpEngine{}
	done := make(chan struct{})
	defer close(done)
	out := &bytes.Buffer{}
	s := &streamSession{eng: eng, st: &st, tty: out, ctl: newControl(eng, done)}
	armAutomaticFallback(s)
	s.observeWritePressure(freeItem{pressureSample: true, pressureGen: s.writePressureGen, writeDur: writePressureThreshold})
	if !waitFor(2*time.Second, func() bool { return len(eng.calls()) == 1 }) {
		t.Fatalf("first fallback restart calls = %v", eng.calls())
	}
	out.Reset()
	armAutomaticFallback(s)
	s.observeWritePressure(freeItem{pressureSample: true, pressureGen: s.writePressureGen, writeDur: writePressureThreshold})
	if out.Len() != 0 || len(eng.calls()) != 1 {
		t.Fatalf("second verdict wrote %q or restarted %v", out.String(), eng.calls())
	}
}

func TestWritePressureResetRejectsStaleCompletion(t *testing.T) {
	st := automaticFallbackState("auto")
	s := &streamSession{st: &st}
	armAutomaticFallback(s)
	oldGeneration := s.writePressureGen
	s.resetWritePressure()

	if s.writePressure != (writePressureObserver{}) || s.writePressureGen == oldGeneration {
		t.Fatalf("reset kept observer %+v or generation %d", s.writePressure, s.writePressureGen)
	}
	s.observeWritePressure(freeItem{pressureSample: true, pressureGen: oldGeneration, writeDur: writePressureThreshold})
	if st.Colour != ColourTrue {
		t.Fatalf("stale completion changed colour to %v", st.Colour)
	}
}

func TestWritePressureObserverResetsOnSessionChanges(t *testing.T) {
	cases := []struct {
		name   string
		adjust func(*State)
		action func(*streamSession)
	}{
		{"backend", func(st *State) { st.SetEnabled([]Backend{BackendBlock, BackendKitty}) }, func(s *streamSession) { s.cycleBackend(false) }},
		{"scheme recompose", func(st *State) { st.sextantOK = true }, func(s *streamSession) { s.recomposeBlock(true) }},
		{"manual colour", nil, func(s *streamSession) { s.cycleColour(false) }},
		{"palette policy", func(st *State) { st.Colour = ColourXTerm256 }, func(s *streamSession) { s.togglePalettePolicy() }},
		{"resize signal", nil, func(s *streamSession) { s.fd = ^uintptr(0); _ = s.resize() }},
		{"loop lifecycle", nil, func(s *streamSession) {
			keys := make(chan keyEvent)
			close(keys)
			_ = s.loop(nil, keys)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := automaticFallbackState("auto")
			if tc.adjust != nil {
				tc.adjust(&st)
			}
			eng := streamTestEngine{frames: make(chan render.Frame)}
			done := make(chan struct{})
			defer close(done)
			s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}
			armAutomaticFallback(s)
			oldGeneration := s.writePressureGen

			tc.action(s)

			if s.writePressure != (writePressureObserver{}) || s.writePressureGen == oldGeneration {
				t.Fatalf("session change kept observer %+v or generation %d", s.writePressure, s.writePressureGen)
			}
		})
	}
}

func TestWritePressureObserverContinuesAcrossNavigation(t *testing.T) {
	st := automaticFallbackState("auto")
	eng := streamTestEngine{}
	done := make(chan struct{})
	defer close(done)
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}
	s.writePressure.sampledWrites = writePressureStartupSamples - 1
	oldGeneration := s.writePressureGen

	s.forwardNav(NavReload)
	s.openURL("https://example.com/")

	if s.writePressureGen != oldGeneration || s.writePressure.sampledWrites != writePressureStartupSamples-1 {
		t.Fatalf("navigation changed observer %+v or generation %d", s.writePressure, s.writePressureGen)
	}
	if s.writePressure.observe(writePressureThreshold) {
		t.Fatal("the fourth startup sample reported pressure after navigation")
	}
	if s.writePressure.observe(writePressureThreshold) {
		t.Fatal("the first post-startup slow sample reported pressure after navigation")
	}
	for sample := range writePressureSustainedSamples - 2 {
		if s.writePressure.observe(writePressureThreshold) {
			t.Fatalf("post-startup slow sample %d reported pressure after navigation", sample+2)
		}
	}
	if !s.writePressure.observe(writePressureThreshold) {
		t.Fatal("the fourth post-startup slow sample did not report pressure after navigation")
	}
}
