package viewer

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/kittyinterop"
	"github.com/wimpysworld/nyala/internal/render"
)

func TestTmuxKittyPNGWireAndPlacement(t *testing.T) {
	st := NewState(3, 3)
	st.Backend = BackendKitty
	st.kittyTmux = true
	st.kittyTransport = KittyTransportSHM
	if st.kittySHMLive() {
		t.Fatal("tmux Kitty tried shared memory")
	}

	// Incompressible pixel data reliably forces multiple APC chunks.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	rng := rand.NewChaCha8([32]byte{1, 2, 3})
	_, _ = rng.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	frame := render.Frame{EncodedPNG: encodedPNG(t, img)}
	var output bytes.Buffer
	if err := st.RenderStreamFrame(frame, &output); err != nil {
		t.Fatal(err)
	}
	apcs, plain, count := splitTmuxKittyTestWire(t, output.String())
	if count < 2 {
		t.Fatalf("expected separately wrapped continuation chunks, got %d", count)
	}
	term, err := kittyinterop.Decode(apcs)
	if err != nil {
		t.Fatal(err)
	}
	if len(term.Events) != 1 || term.Events[0].ID != st.kittyID || len(term.Events[0].Chunks) != count {
		t.Fatal("decoded Kitty events do not match the wrapped transmission")
	}
	opts := term.Events[0].Chunks[0].Options
	if opts["U"] != "1" || opts["c"] != "3" || opts["r"] != "2" {
		t.Fatalf("expected 3x2 virtual placement, got %v", opts)
	}
	got := term.Images[st.kittyID]
	if got == nil || got.Bounds() != img.Bounds() || !bytes.Equal(got.Pix, img.Pix) {
		t.Fatal("decoded tmux Kitty image differs from the input PNG")
	}
	const cells = "\x1b[1;1H\U0010EEEE\u0305\u0305\U0010EEEE\u0305\u030D\U0010EEEE\u0305\u030E" +
		"\x1b[2;1H\U0010EEEE\u030D\u0305\U0010EEEE\u030D\u030D\U0010EEEE\u030D\u030E"
	id := st.kittyID
	want := fmt.Sprintf("\x1b[?7h\x1b[38;2;%d;%d;%dm", id>>16, id>>8&255, id&255) + cells + "\x1b[H\x1b[?7l\x1b[0m"
	if plain != want {
		t.Fatalf("placeholder grid = %q, want %q", plain, want)
	}
}

func TestTmuxKittyScopedLifecycle(t *testing.T) {
	st := NewState(2, 2)
	st.Backend = BackendKitty
	st.kittyTmux = true
	st.kittyTmuxPaint = newTmuxKittyPlacement()
	first, second := st.kittyTmuxPaint.ids[0], st.kittyTmuxPaint.ids[1]
	if first == second || first <= 255 || second <= 255 || first > 0xffffff || second > 0xffffff {
		t.Fatalf("invalid tmux Kitty image IDs: %d, %d", first, second)
	}
	frame := render.Frame{EncodedPNG: encodedPNG(t, solidImageSize(8, 8, color.RGBA{R: 0x31, A: 0xff}))}
	var out bytes.Buffer
	if err := st.RenderStreamFrame(frame, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := st.RenderStreamFrame(frame, &out); err != nil {
		t.Fatal(err)
	}
	apcs, _, count := splitTmuxKittyTestWire(t, out.String())
	cmds := kittyCommands(string(apcs))
	if count != 2 || len(cmds) != 2 {
		t.Fatal("second frame must transmit and then delete one image")
	}
	if !hasOpt(cmds[0], "a=T") || !hasOpt(cmds[0], "U=1") || !hasOpt(cmds[0], "i="+strconv.Itoa(second)) {
		t.Fatal("second frame did not transmit a virtual placement under its own ID")
	}
	assertDelete(t, cmds[1], strconv.Itoa(first))

	out.Reset()
	st.Resize(2, 1)
	if err := st.resetKitty(&out); err != nil {
		t.Fatal(err)
	}
	apcs, plain, count := splitTmuxKittyTestWire(t, out.String())
	cmds = kittyCommands(string(apcs))
	if count != 2 || len(cmds) != 2 || plain != "" || st.kittyID != 0 {
		t.Fatal("Kitty reset must only delete the two live IDs")
	}
	assertDelete(t, cmds[0], strconv.Itoa(first))
	assertDelete(t, cmds[1], strconv.Itoa(second))
}

func TestTmuxKittyTTYTracksWrittenPaint(t *testing.T) {
	frame := render.Frame{EncodedPNG: encodedPNG(t, solidImageSize(8, 8, color.RGBA{R: 0x31, A: 0xff}))}
	for _, tc := range []struct {
		name       string
		writePaint bool
		stageReset bool
		writeReset bool
		wantClear  bool
	}{
		{name: "Kitty frame still assembled"},
		{name: "Kitty frame written", writePaint: true, wantClear: true},
		{name: "reset still assembled", writePaint: true, stageReset: true, wantClear: true},
		{name: "reset written before Block", writePaint: true, writeReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(2, 1)
			st.SetEnabled([]Backend{BackendKitty, BackendBlock})
			st.kittyTmux = true
			p := newTmuxKittyPlacement()
			st.kittyTmuxPaint = p

			var out bytes.Buffer
			tty := &tmuxKittyTTY{tty: &out, deletes: p.deletes}
			s := &streamSession{st: &st, tty: tty, ctl: &control{pending: make(chan captureConfig, 1)}}
			if _, err := tty.Write([]byte("Block frame")); err != nil {
				t.Fatal(err)
			}
			s.assembled = &bytes.Buffer{}
			if _, err := s.assemble(s.assembled, frame); err != nil {
				t.Fatal(err)
			}
			s.writer = newStreamWriter(tty)
			if tc.writePaint {
				s.flushAssembled()
			}
			if tc.writeReset {
				s.cycleBackend(false)
			}
			s.writer.stop()
			if tc.stageReset {
				var reset bytes.Buffer
				if err := s.resetKitty(&reset); err != nil || reset.Len() == 0 {
					t.Fatalf("assemble reset: %v, %d bytes", err, reset.Len())
				}
			}
			if tc.writeReset {
				if _, err := tty.Write([]byte("next Block frame")); err != nil {
					t.Fatal(err)
				}
			}

			start := out.Len()
			tty.finish("restore")
			apcs, plain, count := splitTmuxKittyTestWire(t, out.String()[start:])
			if !tc.wantClear {
				if count != 0 || plain != "restore" {
					t.Fatalf("unexpected Kitty cleanup: %q", out.String()[start:])
				}
				return
			}
			cmds := kittyCommands(string(apcs))
			if count != 2 || len(cmds) != 2 || plain != sgrReset+clearScreen+"restore" {
				t.Fatalf("expected scoped Kitty cleanup before restore, got %q", out.String()[start:])
			}
			assertDelete(t, cmds[0], strconv.Itoa(p.ids[0]))
			assertDelete(t, cmds[1], strconv.Itoa(p.ids[1]))
		})
	}
}

func TestTmuxKittyPlaceholderBounds(t *testing.T) {
	const limit = 297
	if !tmuxKittyGridFits(limit, limit) || tmuxKittyGridFits(limit+1, 1) || tmuxKittyGridFits(1, limit+1) || tmuxKittyGridFits(0, 1) {
		t.Fatal("tmux Kitty grid limits differ from the protocol table")
	}
	last := "\U0010EEEE\U0001D244\U0001D244"
	cells := string(appendTmuxKittyCells(nil, 0x123456, limit, limit))
	if strings.Count(cells, "\U0010EEEE") != limit*limit || !strings.HasSuffix(cells, last+"\x1b[H\x1b[?7l\x1b[0m") {
		t.Fatal("last protocol table entry did not place the bottom-right cell")
	}
	st := NewState(limit+1, 1)
	st.Backend = BackendKitty
	st.kittyTmux = true
	var out bytes.Buffer
	err := st.RenderStreamFrame(render.Frame{EncodedPNG: "ignored"}, &out)
	if err == nil || !strings.Contains(err.Error(), "use --backend block") || out.Len() != 0 {
		t.Fatalf("oversized placement error = %v, terminal bytes = %d", err, out.Len())
	}
}

func TestTmuxKittyTTYStopsWritesAfterCleanup(t *testing.T) {
	var out bytes.Buffer
	writer := &tmuxKittyTTY{tty: &out}
	if _, err := writer.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	writer.finish("cleanup")
	if _, err := writer.Write([]byte("stale")); !errors.Is(err, io.ErrClosedPipe) || out.String() != "framecleanup" {
		t.Fatalf("after cleanup output = %q, write error = %v", out.String(), err)
	}
}

func splitTmuxKittyTestWire(t *testing.T, wire string) (apcs []byte, plain string, count int) {
	t.Helper()
	const outerIntro = "\x1bPtmux;"
	const end = "\x1b\x1b\\\x1b\\"
	var cells strings.Builder
	for {
		text, tail, found := strings.Cut(wire, outerIntro)
		cells.WriteString(text)
		if !found {
			break
		}
		command, rest, ok := strings.Cut(tail, end)
		if !ok || !strings.HasPrefix(command, "\x1b\x1b_G") || strings.ContainsRune(command[4:], '\x1b') {
			t.Fatal("malformed tmux wrapper or multiple APCs in one wrapper")
		}
		apcs = append(apcs, command[1:]...)
		apcs = append(apcs, "\x1b\\"...)
		count++
		wire = rest
	}
	if strings.Contains(cells.String(), "\x1b_G") {
		t.Fatal("an unwrapped Kitty command leaked into tmux text")
	}
	return apcs, cells.String(), count
}
