package viewer

import (
	"image"
	"io"

	"github.com/wimpysworld/nyala/internal/render/kitty"
)

// kittySHMObject is the mapped-object seam returned by kittySHMWriter. The
// production value is *kitty.SHMObject; tests can inject mapped-object close
// failures without replacing the production SHM writer.
type kittySHMObject interface {
	Name() string
	Data() []byte
	Close() error
}

// kittySHMWriter is the shared-memory writer seam encodeKittySHM drives.
type kittySHMWriter interface {
	// Create makes a new size-byte object for the given double-buffer image
	// id, mapped for writing, unlinking the unconsumed predecessor.
	Create(id, size int) (kittySHMObject, error)
	// Close unlinks every tracked object the terminal never consumed.
	Close() error
}

// liveKittySHMWriter adapts the concrete production writer to the viewer-local
// object seam.
type liveKittySHMWriter struct {
	*kitty.SHMWriter
}

func (w *liveKittySHMWriter) Create(id, size int) (kittySHMObject, error) {
	return w.SHMWriter.Create(id, size)
}

// kittySHMLive reports whether live Kitty frames should travel over shared
// memory.
func (s *State) kittySHMLive() bool {
	return s.kittyTransport == KittyTransportSHM
}

// takeKittySHMWrote reports whether the frame just rendered went out as an shm
// transmit and clears the flag, so the session's pacing gate arms once per shm
// frame.
func (s *State) takeKittySHMWrote() bool {
	wrote := s.kittyWroteSHM
	s.kittyWroteSHM = false
	return wrote
}

// closeKittySHM unlinks any shared-memory objects the terminal never consumed
// and drops the writer, so no POSIX shared-memory object outlives the kitty
// session. The unlink is best-effort: a failure leaves at most a stale object,
// which must not abort a backend switch or teardown.
func (s *State) closeKittySHM() {
	if s.kittySHM == nil {
		return
	}
	_ = s.kittySHM.Close()
	s.kittySHM = nil
}

// encodeKittyPNG transmits a retained CDP PNG unchanged, then deletes the prior
// double-buffer image. The placement carries the c=/r= grid keys at the page
// grid (engineRows of the terminal rows, the same reduced grid the engine
// captures at), so the terminal scales the image to the page cell area and
// never over the bottom bar row: after a mid-session zoom the engine delivers
// the complete viewport at the stale launch paint density, and the grid fit
// keeps it spanning the page (a matching frame scales 1:1).
func (s *State) encodeKittyPNG(data string, w io.Writer, belowText bool) error {
	next := nextKittyID(s.kittyID)
	if s.kittyEnc == nil {
		s.kittyEnc = &kitty.LiveEncoder{}
	}
	rows := engineRows(s.Rows)
	var err error
	if belowText {
		err = s.kittyEnc.EncodePNGIDAtZ(data, next, kittyFPSPageZ, s.Cols, rows, w)
	} else {
		err = s.kittyEnc.EncodePNGID(data, next, s.Cols, rows, w)
	}
	if err != nil {
		return err
	}
	if s.kittyID != 0 {
		if _, err := io.WriteString(w, kitty.DeleteImage(s.kittyID)); err != nil {
			return err
		}
	}
	s.kittyID = next
	return nil
}

// kittySHMQuiet is the response-quiet key the shm path transmits. Zero keeps
// the terminal's per-transmit acknowledgement (q=1 would suppress the OK and
// q=2 every response), because the stream session paces the shm path on that
// reply: the tiny t=s escape carries no pty backpressure, so without the ack
// gate latency could build in the pty. The direct path keeps q=2.
const kittySHMQuiet = 0

// encodeKittySHM copies one decoded RGBA plane into a fresh shared-memory
// object and transmits the single unchunked f=32,t=s escape under the next
// double-buffer image id, then deletes the prior id, mirroring
// encodeKittyPNG's transmit-then-delete ordering so the swap never blanks the
// screen. The copy is the one ~15 MB memcpy the transport promotion budgeted;
// the caller releases the frame's plane after this returns. Kitty unlinks the
// object once it consumes it; the tracked writer unlinks the rest. A
// zero-extent plane writes nothing. The caller cleans up and returns any
// synchronous encode error without changing the selected transport.
func (s *State) encodeKittySHM(img *image.RGBA, w io.Writer, belowText bool) error {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= 0 || height <= 0 {
		return nil
	}
	next := nextKittyID(s.kittyID)
	if s.kittyEnc == nil {
		s.kittyEnc = &kitty.LiveEncoder{}
	}
	if s.kittySHM == nil {
		s.kittySHM = &liveKittySHMWriter{SHMWriter: kitty.NewSHMWriter()}
	}
	obj, err := s.kittySHM.Create(next, 4*width*height)
	if err != nil {
		return err
	}
	copyRGBAPlane(obj.Data(), img)
	if err := obj.Close(); err != nil {
		return err
	}
	rows := engineRows(s.Rows)
	if belowText {
		err = s.kittyEnc.EncodeSHMIDAtZ(obj.Name(), width, height, next, kittyFPSPageZ, kittySHMQuiet, s.Cols, rows, w)
	} else {
		err = s.kittyEnc.EncodeSHMID(obj.Name(), width, height, next, kittySHMQuiet, s.Cols, rows, w)
	}
	if err != nil {
		return err
	}
	if s.kittyID != 0 {
		if _, err := io.WriteString(w, kitty.DeleteImage(s.kittyID)); err != nil {
			return err
		}
	}
	s.kittyID = next
	s.kittyWroteSHM = true
	return nil
}

// copyRGBAPlane copies img's pixels into dst as tightly packed RGBA rows, the
// raw f=32 layout the Kitty shm escape references. Pooled engine planes are
// tightly packed at origin, so the steady state is one straight copy; the row
// loop covers any other stride or sub-rectangle.
func copyRGBAPlane(dst []byte, img *image.RGBA) {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	rowBytes := 4 * width
	if img.Stride == rowBytes && b.Min == (image.Point{}) {
		copy(dst, img.Pix[:rowBytes*height])
		return
	}
	for y := range height {
		off := img.PixOffset(b.Min.X, b.Min.Y+y)
		copy(dst[y*rowBytes:(y+1)*rowBytes], img.Pix[off:off+rowBytes])
	}
}

// kittyFPSPageZ places the live page below glyphs and non-default cell
// backgrounds while the fps meter is armed, so terminal text always presents
// above the image. Crisp DOM text on Kitty depends on this placement.
const kittyFPSPageZ = -1073741825

// nextKittyID returns the next double-buffer image id, alternating between 1 and
// 2. A zero prev (no image placed yet) yields 1, so the first frame uses id 1.
func nextKittyID(prev int) int {
	if prev == 1 {
		return 2
	}
	return 1
}

// resetKitty deletes both double-buffer image ids and frees their data, then
// clears the tracked id so a later return to the kitty backend rebuilds from a
// clean slate with id 1. It also closes the shm writer, unlinking any
// shared-memory objects the terminal never consumed. It is a no-op when no
// kitty image has been placed and no shm object exists. The stream session
// calls it when the backend cycle leaves kitty.
func (s *State) resetKitty(w io.Writer) error {
	s.closeKittySHM()
	if s.kittyID == 0 {
		return nil
	}
	if _, err := io.WriteString(w, kitty.DeleteImage(1)+kitty.DeleteImage(2)); err != nil {
		return err
	}
	s.kittyID = 0
	return nil
}
