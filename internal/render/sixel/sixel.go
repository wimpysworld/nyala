// Package sixel renders a Frame as a Sixel DCS payload for terminals that
// speak the Sixel graphics protocol.
//
// Renderer emits the complete Device Control String: the introducer, raster
// attributes, colour palette, pixel bands, and terminator.
package sixel

import (
	"fmt"
	"io"
	"sync"

	"github.com/wimpysworld/nyala/internal/render"
)

const emptyPayload = "\x1bP0;0;8q\x1b\\"

// Renderer emits a Frame as a Sixel DCS payload. It owns no cell grid; it
// encodes the opaque frame image directly. A renderer retains the encoder's
// output and band scratch across moving frames. The mutex makes one renderer
// safe for concurrent callers while keeping production serial.
type Renderer struct {
	mu  sync.Mutex
	enc *opaqueEncoder
}

// Compile-time assertion that Renderer satisfies render.Renderer.
var _ render.Renderer = (*Renderer)(nil)

// Name returns the backend identifier "sixel".
func (*Renderer) Name() string { return "sixel" }

// Render encodes the frame image as one complete Sixel DCS payload. A nil or
// zero-area image emits an empty but valid payload. The adaptive palette uses
// the maximum 256-register budget and dithering off.
func (r *Renderer) Render(f render.Frame, w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if f.Img == nil || f.Img.Bounds().Empty() {
		if _, err := io.WriteString(w, emptyPayload); err != nil {
			return fmt.Errorf("sixel: write empty payload: %w", err)
		}
		return nil
	}
	if r.enc == nil {
		r.enc = newOpaqueEncoder(nil)
	}
	r.enc.w = w
	err := r.enc.encode(f.Img)
	r.enc.w = nil
	if err != nil {
		return fmt.Errorf("sixel: encode image: %w", err)
	}
	return nil
}
