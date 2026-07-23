package viewer

import (
	"image"
	"io"

	"github.com/wimpysworld/nyala/internal/render/kitty"
)

// kittySHMWriter is the shared-memory writer seam encodeKittySHM drives. The
// production value is *kitty.SHMWriter; a test stub forces create failures to
// exercise the runtime demotion to the direct path.
type kittySHMWriter interface {
	// Create makes a new size-byte object for the given double-buffer image
	// id, mapped for writing, unlinking the unconsumed predecessor.
	Create(id, size int) (*kitty.SHMObject, error)
	// Close unlinks every tracked object the terminal never consumed.
	Close() error
}

// kittySHMLive reports whether live Kitty frames should travel over shared
// memory: the resolved transport is shm and the session has not demoted to
// direct.
func (s *State) kittySHMLive() bool {
	return s.kittyTransport == KittyTransportSHM && !s.kittyShmDemoted
}

// demoteKittySHM records the runtime fallback from the shm transport to
// direct PNG for the rest of the Kitty stint and closes the shm writer,
// unlinking unconsumed objects. The first error wins; later calls are no-ops.
// The stream session collects the recorded error with takeKittySHMDemotion.
func (s *State) demoteKittySHM(err error) {
	if s.kittyShmDemoted {
		return
	}
	s.kittyShmDemoted = true
	s.kittyShmDemoteErr = err
	s.closeKittySHM()
}

// rearmKittySHM clears a runtime shm demotion so the next Kitty stint runs
// the shm transport again. The stream session calls it when Tab returns to
// the Kitty backend: the startup probe proved the transport, so a demotion
// lasts one stint rather than pinning the session on the parser-bound direct
// path. A genuinely broken transport demotes again on its next shm failure.
func (s *State) rearmKittySHM() {
	s.kittyShmDemoted = false
	s.kittyShmDemoteErr = nil
}

// takeKittySHMDemotion returns the error that demoted the shm transport, once,
// and clears it, so the session logs the fallback a single time. It returns
// nil when no demotion is pending.
func (s *State) takeKittySHMDemotion() error {
	err := s.kittyShmDemoteErr
	s.kittyShmDemoteErr = nil
	return err
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
// and drops the writer, so no /dev/shm entry outlives the kitty session. The
// unlink is best-effort: a failure leaves at most a stale tmpfs entry, which
// must not abort a backend switch or teardown.
func (s *State) closeKittySHM() {
	if s.kittySHM == nil {
		return
	}
	_ = s.kittySHM.Close()
	s.kittySHM = nil
}

// encodeKittyPNG transmits a retained CDP PNG unchanged, then deletes the prior
// double-buffer image. The placement carries the c=/r= grid keys, so the
// terminal scales the image to the full cell area: after a mid-session zoom the
// engine delivers the complete viewport at the stale launch paint density, and
// the grid fit keeps it spanning the terminal (a matching frame scales 1:1).
func (s *State) encodeKittyPNG(data string, w io.Writer, belowText bool) error {
	next := nextKittyID(s.kittyID)
	if s.kittyEnc == nil {
		s.kittyEnc = &kitty.LiveEncoder{}
	}
	var err error
	if belowText {
		err = s.kittyEnc.EncodePNGIDAtZ(data, next, kittyFPSPageZ, s.Cols, s.Rows, w)
	} else {
		err = s.kittyEnc.EncodePNGID(data, next, s.Cols, s.Rows, w)
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
// zero-extent plane writes nothing. Any error is an shm transport failure the
// caller answers with runtime demotion.
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
		s.kittySHM = kitty.NewSHMWriter()
	}
	obj, err := s.kittySHM.Create(next, 4*width*height)
	if err != nil {
		return err
	}
	copyRGBAPlane(obj.Data(), img)
	if err := obj.Close(); err != nil {
		return err
	}
	if belowText {
		err = s.kittyEnc.EncodeSHMIDAtZ(obj.Name(), width, height, next, kittyFPSPageZ, kittySHMQuiet, s.Cols, s.Rows, w)
	} else {
		err = s.kittyEnc.EncodeSHMID(obj.Name(), width, height, next, kittySHMQuiet, s.Cols, s.Rows, w)
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
// backgrounds, so Kitty can paint the terminal FPS badge above the image.
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
// calls it when Tab cycles the backend away from kitty.
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
