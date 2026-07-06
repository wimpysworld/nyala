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

// apcIntro opens a Kitty graphics Application Programming Command: ESC _ G.
// st is the String Terminator, ESC backslash, that closes the envelope. They
// mirror the names the capture decoder uses for the same bytes.
const (
	apcIntro = "\x1b_G"
	st       = "\x1b\\"
)

// chunkMore is the m= key marking a frame that has more frames following;
// chunkLast marks the final frame of a chunked transmission.
const (
	chunkMore = "m=1"
	chunkLast = "m=0"
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
// channel order holds. It transmits without an explicit image id, so the capture
// round-trip stays stateless.
func (r *Renderer) Render(f render.Frame, w io.Writer) error {
	return r.render(f, 0, w)
}

// RenderID renders like Render but tags the transmitted image with the given
// Kitty image id (i=<id>), so a caller can double-buffer by alternating two ids
// and deleting the previous one with DeleteImage. An id of zero omits the i= key
// and matches Render byte for byte, so the stateless capture path is unchanged.
func (r *Renderer) RenderID(f render.Frame, id int, w io.Writer) error {
	return r.render(f, id, w)
}

// render is the shared transmit-and-place body for Render and RenderID. A
// positive id adds the i= key; a zero id omits it.
func (*Renderer) render(f render.Frame, id int, w io.Writer) error {
	opts := kitty.Options{
		Format:       kitty.RGBA,
		Compression:  kitty.Zlib,
		Transmission: kitty.Direct,
		Action:       kitty.TransmitAndPut,
		Quite:        2, // upstream spelling; sets q=2, suppress replies
	}
	if id > 0 {
		opts.ID = id
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

// DeleteImage returns the Kitty escape that deletes the image with the given id
// and frees its data (a=d,d=I,i=<id>). The capital d=I frees the stored image
// bytes, not just its on-screen placements. A double-buffered caller emits it
// after placing the next frame under the other id, so the old image leaves the
// screen and its memory frees without a blank frame in between. The q=2 key
// suppresses the terminal's OK and error replies.
func DeleteImage(id int) string {
	opts := kitty.Options{
		Action:          kitty.Delete,
		Delete:          kitty.DeleteID,
		DeleteResources: true,
		ID:              id,
		Quite:           2,
	}
	return ansi.KittyGraphics(nil, opts.Options()...)
}

// writeChunks splits payload into frames of at most kitty.MaxChunkSize bytes and
// writes one APC frame per chunk. The first frame carries the control options
// plus m=1; middle frames carry only m=1; the final frame carries m=0. Every
// frame uses the ESC _ G ... ESC backslash envelope. MaxChunkSize is a multiple
// of 4 and a full base64 payload length is a multiple of 4, so each non-final
// chunk stays a multiple of 4.
func writeChunks(w io.Writer, payload []byte, optList []string) error {
	first := strings.Join(optList, ",") + "," + chunkMore
	for len(payload) > kitty.MaxChunkSize {
		chunk := payload[:kitty.MaxChunkSize]
		payload = payload[kitty.MaxChunkSize:]
		control := chunkMore
		if first != "" {
			control = first
			first = ""
		}
		if err := writeFrame(w, control, chunk); err != nil {
			return err
		}
	}
	return writeFrame(w, chunkLast, payload)
}

// writeFrame writes one APC frame: the ESC _ G introducer, the control section,
// a semicolon separator, the chunk body, then the ESC backslash terminator. It
// wraps a write failure.
func writeFrame(w io.Writer, control string, chunk []byte) error {
	if _, err := io.WriteString(w, apcIntro+control+";"+string(chunk)+st); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}
