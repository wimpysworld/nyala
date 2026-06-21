// Package sixel renders a Frame as a Sixel DCS payload for terminals that
// speak the Sixel graphics protocol.
//
// Renderer wraps charmbracelet/x/ansi/sixel, which emits the body of the
// Sixel sequence: the raster attribute, the colour palette, and the pixel
// bands. This renderer frames that body in the Device Control String envelope
// the terminal expects, so the emitted bytes are a complete, drawable sequence.
package sixel

import (
	"fmt"
	"io"

	"github.com/charmbracelet/x/ansi/sixel"

	"github.com/wimpysworld/nyala/internal/render"
)

// dcsIntro opens the Sixel Device Control String: ESC P selects the DCS, and the
// q final byte selects the Sixel graphics function with the default parameters.
const dcsIntro = "\x1bPq"

// st is the String Terminator, ESC backslash, that closes the DCS payload.
const st = "\x1b\\"

// Renderer emits a Frame as a Sixel DCS payload. It owns no cell grid; it
// encodes the frame image directly with charmbracelet/x/ansi/sixel and wraps the
// encoder body in the DCS envelope. It satisfies render.Renderer.
type Renderer struct{}

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// Name returns the backend identifier "sixel".
func (*Renderer) Name() string { return "sixel" }

// Render encodes the frame image as Sixel and writes the framed DCS payload to
// w: the DCS introducer ESC P q, the encoder body (raster attribute, palette,
// and pixel bands), then the String Terminator ESC backslash. A nil frame image
// emits an empty but well-framed payload. The frame image is RGBA; the encoder
// reads it through the image.Image colour interface, so channel order holds.
func (*Renderer) Render(f render.Frame, w io.Writer) error {
	if _, err := io.WriteString(w, dcsIntro); err != nil {
		return fmt.Errorf("sixel: write DCS introducer: %w", err)
	}
	if f.Img != nil {
		var enc sixel.Encoder
		if err := enc.Encode(w, f.Img); err != nil {
			return fmt.Errorf("sixel: encode image: %w", err)
		}
	}
	if _, err := io.WriteString(w, st); err != nil {
		return fmt.Errorf("sixel: write string terminator: %w", err)
	}
	return nil
}
