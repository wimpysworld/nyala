package capture

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// sixelDCSCommand is the DCS final byte that selects the Sixel graphics
// function. The ansi parser encodes the DCS command as its final byte 'q', so a
// handler registered under this value fires when a well-framed Sixel stream is
// parsed.
const sixelDCSCommand = 'q'

// TestVTSixelStreamParses feeds the sixel.Renderer byte stream through the
// charmbracelet/x/vt oracle. A real terminal parser must recognise the framing:
// the DCS handler fires with a non-empty body, proving the ESC P q introducer,
// the encoder body, and the ESC backslash terminator are well formed.
func TestVTSixelStreamParses(t *testing.T) {
	var buf bytes.Buffer
	var r sixel.Renderer
	if err := r.Render(frameFixture(), &buf); err != nil {
		t.Fatalf("sixel Render: %v", err)
	}

	e := vt.NewEmulator(80, 24)
	var fired bool
	var body []byte
	e.RegisterDcsHandler(sixelDCSCommand, func(_ ansi.Params, data []byte) bool {
		fired = true
		body = append([]byte(nil), data...)
		return true
	})
	if _, err := e.Write(buf.Bytes()); err != nil {
		t.Fatalf("emulator Write: %v", err)
	}

	if !fired {
		t.Fatal("VT oracle did not parse a Sixel DCS sequence from the stream")
	}
	if len(body) == 0 {
		t.Error("VT oracle parsed an empty Sixel body, want a non-empty payload")
	}
}

// TestVTKittyStreamParses feeds the kitty.Renderer byte stream through the VT
// oracle. The APC handler fires with a non-empty body, proving the ESC _ G
// introducer, the options and payload, and the ESC backslash terminator parse
// as a Kitty graphics Application Programming Command.
func TestVTKittyStreamParses(t *testing.T) {
	var buf bytes.Buffer
	var r kitty.Renderer
	if err := r.Render(frameFixture(), &buf); err != nil {
		t.Fatalf("kitty Render: %v", err)
	}

	e := vt.NewEmulator(80, 24)
	var fired bool
	var body []byte
	e.RegisterApcHandler(func(data []byte) bool {
		fired = true
		body = append([]byte(nil), data...)
		return true
	})
	if _, err := e.Write(buf.Bytes()); err != nil {
		t.Fatalf("emulator Write: %v", err)
	}

	if !fired {
		t.Fatal("VT oracle did not parse a Kitty APC sequence from the stream")
	}
	if len(body) == 0 {
		t.Error("VT oracle parsed an empty Kitty body, want a non-empty payload")
	}
}

// TestVTSixelMalformedFramingFails confirms the oracle is a real check, not a
// rubber stamp: a stream with the DCS introducer stripped never reaches the
// Sixel handler, so the handler must not fire.
func TestVTSixelMalformedFramingFails(t *testing.T) {
	var buf bytes.Buffer
	var r sixel.Renderer
	if err := r.Render(frameFixture(), &buf); err != nil {
		t.Fatalf("sixel Render: %v", err)
	}
	mangled := bytes.TrimPrefix(buf.Bytes(), []byte("\x1bP"))

	e := vt.NewEmulator(80, 24)
	var fired bool
	e.RegisterDcsHandler(sixelDCSCommand, func(_ ansi.Params, _ []byte) bool {
		fired = true
		return true
	})
	if _, err := e.Write(mangled); err != nil {
		t.Fatalf("emulator Write: %v", err)
	}
	if fired {
		t.Error("VT oracle parsed a Sixel DCS from a stream missing the ESC P introducer")
	}
}

// TestVTKittyMalformedFramingFails confirms the oracle rejects bad Kitty
// framing: with the APC introducer stripped the handler must not fire.
func TestVTKittyMalformedFramingFails(t *testing.T) {
	var buf bytes.Buffer
	var r kitty.Renderer
	if err := r.Render(frameFixture(), &buf); err != nil {
		t.Fatalf("kitty Render: %v", err)
	}
	mangled := bytes.TrimPrefix(buf.Bytes(), []byte("\x1b_"))

	e := vt.NewEmulator(80, 24)
	var fired bool
	e.RegisterApcHandler(func(_ []byte) bool {
		fired = true
		return true
	})
	if _, err := e.Write(mangled); err != nil {
		t.Fatalf("emulator Write: %v", err)
	}
	if fired {
		t.Error("VT oracle parsed a Kitty APC from a stream missing the ESC _ introducer")
	}
}
