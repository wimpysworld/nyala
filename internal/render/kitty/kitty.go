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
// transmit action. A nil frame image emits an empty but well-framed payload.
// The frame image is RGBA; the encoder reads it through the image.Image colour
// interface, so channel order holds.
func (*Renderer) Render(f render.Frame, w io.Writer) error {
	opts := kitty.Options{
		Format:       kitty.RGBA,
		Compression:  kitty.Zlib,
		Transmission: kitty.Direct,
		Action:       kitty.Transmit,
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

	if _, err := io.WriteString(w, ansi.KittyGraphics(payload, opts.Options()...)); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}
