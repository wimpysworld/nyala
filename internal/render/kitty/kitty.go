// Package kitty renders a Frame as a Kitty graphics APC payload for terminals
// that speak the Kitty graphics protocol.
//
// Renderer encodes the frame image as zlib-compressed 32-bit RGBA with
// charmbracelet/x/ansi/kitty, base64-encodes that payload, and frames it in the
// Application Programming Command envelope the terminal expects. The transmitted
// options record the format, the zlib compression, the pixel dimensions, direct
// transmission, and the transmit action, so a decoder can reverse the payload
// without out-of-band metadata.
package kitty

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/wimpysworld/nyala/internal/render"
)

// Renderer emits a Frame as a Kitty graphics APC payload. It owns no cell
// grid; it encodes the frame image directly with charmbracelet/x/ansi/kitty and
// frames the base64 payload in the APC envelope. It satisfies render.Renderer.
type Renderer struct{}

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// Name returns the backend identifier "kitty".
func (*Renderer) Name() string { return "kitty" }

// Render encodes the frame image as zlib-compressed RGBA, base64-encodes the
// result, and writes the framed APC payload to w: the APC introducer ESC _ G,
// the comma-separated options, a semicolon, the base64 payload, then the String
// Terminator ESC backslash. The options carry the RGBA format, zlib
// compression, the image width and height, direct transmission, and the
// transmit-and-display action, so the terminal both loads and shows the image.
// The q=2 key suppresses the terminal's OK and error replies. A payload over the
// protocol chunk limit is split across successive APC frames with the m= key. A
// nil frame image emits an empty but well-framed payload. The frame image is
// RGBA; the encoder reads it through the image.Image colour interface, so
// channel order holds.
func (*Renderer) Render(f render.Frame, w io.Writer) error {
	opts := kitty.Options{
		Format:       kitty.RGBA,
		Compression:  kitty.Zlib,
		Transmission: kitty.Direct,
		Action:       kitty.TransmitAndPut,
		Quite:        2,
	}

	var payload []byte
	if f.Img != nil {
		b := f.Img.Bounds()
		opts.ImageWidth = b.Dx()
		opts.ImageHeight = b.Dy()

		var raw bytes.Buffer
		enc := kitty.Encoder{Compress: true, Format: kitty.RGBA}
		if err := enc.Encode(&raw, f.Img); err != nil {
			return fmt.Errorf("kitty: encode image: %w", err)
		}

		payload = make([]byte, base64.StdEncoding.EncodedLen(raw.Len()))
		base64.StdEncoding.Encode(payload, raw.Bytes())
	}

	if len(payload) <= kitty.MaxChunkSize {
		if _, err := io.WriteString(w, ansi.KittyGraphics(payload, opts.Options()...)); err != nil {
			return fmt.Errorf("kitty: write APC payload: %w", err)
		}
		return nil
	}

	return writeChunks(w, payload, opts.Options())
}

// writeChunks splits payload into frames of at most kitty.MaxChunkSize bytes and
// writes one APC frame per chunk. The first frame carries the control options
// plus m=1; middle frames carry only m=1; the final frame carries m=0. Every
// frame uses the ESC _ G ... ESC backslash envelope. MaxChunkSize is a multiple
// of 4 and a full base64 payload length is a multiple of 4, so each non-final
// chunk stays a multiple of 4.
func writeChunks(w io.Writer, payload []byte, optList []string) error {
	first := strings.Join(optList, ",") + ",m=1"
	for len(payload) > kitty.MaxChunkSize {
		chunk := payload[:kitty.MaxChunkSize]
		payload = payload[kitty.MaxChunkSize:]
		control := "m=1"
		if first != "" {
			control = first
			first = ""
		}
		if _, err := io.WriteString(w, "\x1b_G"+control+";"+string(chunk)+"\x1b\\"); err != nil {
			return fmt.Errorf("kitty: write APC payload: %w", err)
		}
	}
	if _, err := io.WriteString(w, "\x1b_Gm=0;"+string(payload)+"\x1b\\"); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}
