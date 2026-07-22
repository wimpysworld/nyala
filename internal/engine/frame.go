package engine

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register the JPEG decoder for the image.Decode JPEG fallback
	"sync"
	"unsafe"

	emmb64 "github.com/emmansun/base64"
	"github.com/gen2brain/jpegn"

	"github.com/wimpysworld/nyala/internal/pngdecode"
)

// pngSignature is the 8-byte PNG file magic. decodeFrameImage sniffs it to route
// PNG frames explicitly through the woozymasta decoder instead of the image.Decode
// registry, so a second registered "png" format cannot make dispatch init-order
// dependent (correctness packages also import stdlib image/png for encoding).
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// decodeFrameImage decodes one raw image payload, routing by file signature. A PNG
// (matched by its 8-byte magic) decodes through woozymasta/png, a byte-identical and
// faster drop-in for image/png; everything else (JPEG) decodes through the stdlib
// image.Decode registry. The explicit PNG route avoids registering a second "png"
// format alongside correctness code's stdlib image/png import, which would make
// image.Decode dispatch init-order dependent.
func decodeFrameImage(raw []byte) (image.Image, error) {
	if bytes.HasPrefix(raw, pngSignature) {
		return pngdecode.Decode(bytes.NewReader(raw))
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// b64Buf is one pooled base64 output buffer plus its once-built release
// closure, so a steady-state decode allocates neither the buffer nor the
// closure per frame.
type b64Buf struct {
	buf     []byte
	release func()
}

// base64DecodeBufPool pools the decoded-bytes output buffer for decodeBase64Pooled,
// so the ~188 KB output allocation each screencast frame makes is reused across frames
// instead of allocated fresh. sync.Pool is safe under the concurrent live decode-pool
// workers: each Get hands one worker its own buffer. New is set in init because
// newB64Buf refers back to the pool, which a var initialiser would reject as an
// initialisation cycle.
var base64DecodeBufPool sync.Pool

func init() { base64DecodeBufPool.New = newB64Buf }

// newB64Buf builds one pool entry with its release closure. The closure reads
// the pool variable at call time, so a test-swapped pool still receives the
// returned entry.
func newB64Buf() any {
	b := &b64Buf{}
	b.release = func() { base64DecodeBufPool.Put(b) }
	return b
}

// decodeBase64Pooled decodes one base64 screencast payload into a pooled output buffer,
// returning the raw bytes plus a release func the caller must call once the downstream
// image decode has finished with the bytes. It prefers the SIMD emmansun Decode(dst,
// src) form, which decodes into the reused pooled dst and so makes no per-call output
// allocation. A decode error is authoritative and returns the borrowed buffer to the
// pool before reaching the caller.
//
// src is a read-only, zero-copy view of data via unsafe.Slice: the decoder only reads
// src, never writes it, so aliasing the string's backing bytes is safe, and it avoids
// the ~245 KB string-to-[]byte copy a plain conversion would make. This mirrors what
// emmansun's own DecodeString does internally.
//
// The returned raw slice aliases the pooled buffer, so it must not escape past release:
// decodeScreencastNative and decodeLiveFrame consume it synchronously into an independent
// image and defer release before returning, so the buffer never reaches the decode
// collector or the emitted frame.
func decodeBase64Pooled(data string) (raw []byte, release func(), err error) {
	decLen := emmb64.StdEncoding.DecodedLen(len(data))
	bp, _ := base64DecodeBufPool.Get().(*b64Buf)
	buf := bp.buf
	if cap(buf) < decLen {
		buf = make([]byte, decLen)
	} else {
		buf = buf[:decLen]
	}
	// Read-only zero-copy view of the string bytes; the decoder never writes src.
	src := unsafe.Slice(unsafe.StringData(data), len(data))
	n, derr := emmb64.StdEncoding.Decode(buf, src)
	bp.buf = buf
	if derr == nil {
		return buf[:n], bp.release, nil
	}
	bp.release()
	return nil, nil, derr
}

// blockScreencastQuality is the JPEG quality every live JPEG capture uses:
// Block, Sixel, and the kitty-shm transport. Quality 90 retains page detail
// while native-YCbCr composition keeps the live Block path within its
// processing budget.
const blockScreencastQuality = 90

// frameChannelBuffer is the buffer size of the emitted-frame channel. A small
// buffer stops a transient restartStream stall from parking the frame loop on the
// frames send while the viewer control goroutine runs the restart; the latest-wins
// raw queue already bounds upstream backpressure, so a couple of results are enough.
const frameChannelBuffer = 2

// blockCellCSSW anchors the CSS density of a block-backend cell: each terminal
// column lays out blockCellCSSW CSS pixels regardless of the device pixels the
// terminal reports per cell. The density is 8 because Block's raster is 2
// sub-cells wide per cell, half the full-pixel resolution the graphics
// backends show at graphicsCellCSSW (16); Browsh's measured 9 CSS px per
// column is the prior art in the same band. Anchoring density per cell makes
// Z = cellW/blockCellCSSW a per-terminal constant (1 at 8 px cells, 2 at
// 16 px), integer on the common cells and stable across resizes.
const blockCellCSSW = 8

// blockLayoutFloor is the minimum block-backend target CSS layout width. It
// keeps a narrow grid on a desktop layout rather than a mobile reflow, the
// same role graphicsLayoutFloor plays for the graphics backends.
const blockLayoutFloor = 1024

// blockLayoutScale is the pure block-backend continuous device scale Z. The
// target CSS width anchors blockCellCSSW CSS pixels per column, held to at
// least blockLayoutFloor. The CSS layout width is min(target, surface), so
// content never lays out wider than the device surface and Z never drops
// below 1: under a sub-floor surface the surface wins, unlike the graphics
// formula, because shrinking Block's sub-cell raster below the terminal's
// natural size costs legibility it cannot afford. Z = surface/cssW is
// continuous (fractional), and a degenerate grid returns 1. The captured
// surface is unaffected: Z rescales only the CSS layout, never the painted
// pixel count, so cssW*Z = surfW stays an identity.
func blockLayoutScale(cols, cellW int) float64 {
	surfW := cols * cellW
	if surfW <= 0 {
		return 1
	}
	target := max(cols*blockCellCSSW, blockLayoutFloor)
	cssW := min(target, surfW)
	return float64(surfW) / float64(cssW)
}

// graphicsCellCSSW anchors the CSS density of a graphics-backend cell: each
// terminal column lays out graphicsCellCSSW CSS pixels regardless of the device
// pixels the terminal reports per cell. Terminals split over the unit of the
// reported cell size (Kitty, Rio, and Contour report logical points; Ghostty
// and WezTerm report scaled device pixels), so anchoring density per cell keeps
// the laid-out page identical across them while the fractional device scale
// carries the difference.
const graphicsCellCSSW = 16

// graphicsLayoutFloor is the minimum graphics-backend target CSS layout
// width. It keeps a tiny window on a desktop layout rather than a mobile
// reflow. There is no ceiling: the per-cell density anchor already sets the
// text size, so a very wide grid lays out wider at the same density.
const graphicsLayoutFloor = 1024

// graphicsLayoutScale is the pure graphics-backend continuous device scale Z.
// The target CSS width anchors graphicsCellCSSW CSS pixels per column, held
// to at least graphicsLayoutFloor, so one density applies everywhere. The CSS
// layout width is min(target, surface) so content never lays out wider than
// the device surface (Z >= 1 keeps text from shrinking below the terminal's
// natural size), except under a sub-floor surface, where the floor
// deliberately wins to keep a desktop layout and Z drops below 1.
// Z = surface/cssW is continuous (fractional), and a degenerate grid returns
// 1. The captured surface is unaffected: Z rescales only the CSS layout,
// never the painted pixel count.
func graphicsLayoutScale(cols, cellW int) float64 {
	surfW := cols * cellW
	if surfW <= 0 {
		return 1
	}
	target := max(cols*graphicsCellCSSW, graphicsLayoutFloor)
	cssW := target
	if surfW >= graphicsLayoutFloor && surfW < target {
		cssW = surfW
	}
	return float64(surfW) / float64(cssW)
}

// layoutScale returns the layout scale Z for the active backend, read under the
// caller's existing e.mu hold. Both backends normalise cell density: the block
// backend anchors blockCellCSSW CSS pixels per column through the pure
// blockLayoutScale, and the graphics backends anchor graphicsCellCSSW through
// graphicsLayoutScale, so a terminal that reports device-pixel cells (Ghostty,
// WezTerm) lays out the same page as one that reports logical-point cells
// (Kitty, Rio, Contour).
func (e *ChromiumEngine) layoutScale() float64 {
	if !e.backend.UsesLayoutScaling() {
		return graphicsLayoutScale(e.cols, e.cellW)
	}
	return blockLayoutScale(e.cols, e.cellW)
}

// snapshotCellPx returns the fractional CSS-pixel cell size, the forced cell the
// in-page walk snaps origins against and calibration corrects the font-size for.
// The CSS layout viewport is the real surface (surfaceSize) divided by Z: under Z
// the page lays out at surf/Z CSS pixels, so a client rect is in CSS pixels and
// dividing the CSS-pixel viewport by the grid maps a rect to its cell. The result is
// fractional so no premature int cast reintroduces the sub-cell binning error; the
// page rounds only at the final cell bin. A zero grid falls back to the resolved
// cell px.
func (e *ChromiumEngine) snapshotCellPx() (cellW, cellH float64) {
	if e.cols > 0 && e.rows > 0 {
		surfW, surfH := e.surfaceSize(e.cols, e.rows)
		z := e.layoutScale()
		return float64(surfW) / z / float64(e.cols), float64(surfH) / z / float64(e.rows)
	}
	return float64(e.cellW), float64(e.cellH)
}

// decodeJPEGFast decodes one live-frame image, preferring the SIMD jpegn decoder
// and falling back to decodeFrameImage on the same bytes. JPEG remains native
// *image.YCbCr for Block and other pixel consumers. Any non-nil jpegn error falls
// back, including jpegn.ErrUnsupported and non-JPEG input. Each call gets its own
// reader, so parallel workers share no decode state.
func decodeJPEGFast(raw []byte) (image.Image, error) {
	if img, err := jpegn.Decode(bytes.NewReader(raw)); err == nil {
		return img, nil
	}
	return decodeFrameImage(raw)
}

// planeReaderPool recycles the bytes.Reader each kitty-shm plane decode wraps
// around its pooled payload bytes, so the hot path allocates no reader.
var planeReaderPool = sync.Pool{New: func() any { return new(bytes.Reader) }}

// errUnknownPlaneFormat reports a kitty-shm payload whose decoded bytes carry
// neither a PNG signature nor a JPEG start-of-image marker, a corrupt frame the
// caller drops like any failed decode.
var errUnknownPlaneFormat = errors.New("engine: kitty plane frame is neither PNG nor JPEG")

// jpegSOI is the two-byte JPEG start-of-image marker decodeKittyPlane sniffs to
// route a JPEG payload to the jpegn RGBA decode.
var jpegSOI = []byte{0xFF, 0xD8}

// kittyPlaneJPEGOpts forces the jpegn decode to emit RGBA directly, so a JPEG
// kitty-shm plane lands in the project's RGBA byte order without a second
// conversion pass. Shared across workers: jpegn only reads the options.
var kittyPlaneJPEGOpts = &jpegn.Options{ToRGBA: true}

// decodeKittyPlane is the kitty-shm decode: it base64-decodes one screencast
// payload into the pooled scratch and decodes the image into an RGBA plane,
// routing by file signature. A PNG (the direct-transport format still in flight
// across a restart) decodes into a pooled plane, so the steady state allocates
// no plane per frame; the caller owns that plane through the returned release
// hook. A JPEG (the shm-mode capture format) decodes through jpegn with ToRGBA
// set; jpegn exposes no decode-into seam, so the JPEG path accepts one fresh
// *image.RGBA per decode, the same per-frame allocation production Block
// already pays at 60 fps, and returns a nil release because the plane is not
// pooled and the GC reclaims it.
func (e *ChromiumEngine) decodeKittyPlane(data string) (image.Image, func(), error) {
	raw, release, err := decodeBase64Pooled(data)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	defer release()
	if w, h, ok := pngdecode.Dimensions(raw); ok {
		fp := livePlanePool().get(w, h)
		br, _ := planeReaderPool.Get().(*bytes.Reader)
		br.Reset(raw)
		_, _, derr := pngdecode.DecodeInto(br, fp.img.Pix)
		br.Reset(nil) // drop the pooled payload reference before returning the reader
		planeReaderPool.Put(br)
		if derr != nil {
			fp.release()
			return nil, nil, fmt.Errorf("engine: decode kitty plane: %w", derr)
		}
		return &fp.img, fp.release, nil
	}
	if !bytes.HasPrefix(raw, jpegSOI) {
		return nil, nil, errUnknownPlaneFormat
	}
	br, _ := planeReaderPool.Get().(*bytes.Reader)
	br.Reset(raw)
	img, derr := jpegn.Decode(br, kittyPlaneJPEGOpts)
	br.Reset(nil) // drop the pooled payload reference before returning the reader
	planeReaderPool.Put(br)
	if derr != nil {
		return nil, nil, fmt.Errorf("engine: decode kitty plane: %w", derr)
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		return nil, nil, fmt.Errorf("engine: decode kitty plane: jpegn returned %T, want *image.RGBA", img)
	}
	return rgba, nil, nil
}

// decodeLiveFrame is the live decode-pool's pixel decode. It keeps the decoder's
// native image layout. Block averages native JPEG YCbCr planes directly while
// composing its final grid. Sixel decodes JPEG q90 through the same signature
// sniff, so its encoder receives native YCbCr; Kitty passes its PNG through
// without calling this method.
func (e *ChromiumEngine) decodeLiveFrame(data string) (image.Image, error) {
	raw, release, err := decodeBase64Pooled(data)
	if err != nil {
		return nil, fmt.Errorf("engine: base64-decode frame: %w", err)
	}
	defer release()
	img, err := decodeJPEGFast(raw)
	if err != nil {
		return nil, fmt.Errorf("engine: decode frame image: %w", err)
	}
	return img, nil
}
