package kitty

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"strings"
	"sync"
	"unsafe"

	emmb64 "github.com/emmansun/base64"
	"github.com/klauspost/compress/zlib"

	"github.com/charmbracelet/x/ansi/kitty"
)

// ErrInvalidPNGData reports that a direct-PNG payload is not the standard
// base64 PNG form CDP emits. Callers can fall back to their decoded pixel path.
var ErrInvalidPNGData = errors.New("kitty: invalid base64 PNG payload")

const base64PNGPrefix = "iVBORw0KGgo"

const pngValidationSize = 64 * 1024

var (
	pngSignature         = [...]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	strictBase64         = emmb64.StdEncoding.Strict()
	pngCRCTable          = crc32.MakeTable(crc32.IEEE)
	pngValidationScratch = sync.Pool{New: func() any { return new([pngValidationSize / 4 * 3]byte) }}
)

// ValidDirectPNGData reports whether data has the standard base64 syntax and the
// chunk structure of a complete PNG. It validates fixed-size SIMD-decoded chunks
// with bounded pooled scratch, without materialising or pixel-decoding the image.
func ValidDirectPNGData(data string) bool {
	const (
		minPNGBytes = 57 // signature plus IHDR, IDAT, and IEND chunk framing
	)
	if len(data) < 76 || len(data)%4 != 0 || !strings.HasPrefix(data, base64PNGPrefix) {
		return false
	}
	expectedBytes := len(data) / 4 * 3
	if data[len(data)-1] == '=' {
		expectedBytes--
		if data[len(data)-2] == '=' {
			expectedBytes--
		}
	}
	if expectedBytes < minPNGBytes {
		return false
	}
	validator := pngStreamValidator{total: int64(expectedBytes), firstChunk: true}

	// The decoder only reads src. This zero-copy view avoids allocating a second
	// encoded frame while the bounded scratch receives each decoded chunk.
	src := unsafe.Slice(unsafe.StringData(data), len(data))
	scratch := pngValidationScratch.Get().(*[pngValidationSize / 4 * 3]byte)
	defer pngValidationScratch.Put(scratch)
	for len(src) > 0 {
		nsrc := min(len(src), pngValidationSize)
		if nsrc < len(src) && (src[nsrc-1] == '=' || src[nsrc-2] == '=') {
			return false
		}
		n, err := strictBase64.Decode(scratch[:], src[:nsrc])
		if err != nil || !validator.feed(scratch[:n]) {
			return false
		}
		src = src[nsrc:]
	}
	return validator.complete()
}

const (
	pngPhaseSignature = iota
	pngPhaseHeader
	pngPhaseData
	pngPhaseCRC
)

type pngStreamValidator struct {
	total      int64
	read       int64
	phase      uint8
	signatureN int
	header     [8]byte
	headerN    int
	chunkType  [4]byte
	dataLeft   int64
	crc        uint32
	crcBytes   [4]byte
	crcN       int
	firstChunk bool
	seenIDAT   bool
	ended      bool
}

func (v *pngStreamValidator) feed(src []byte) bool {
	for len(src) > 0 {
		if v.ended {
			return false
		}
		var ok bool
		switch v.phase {
		case pngPhaseSignature:
			src, ok = v.consumeSignature(src)
		case pngPhaseHeader:
			src, ok = v.consumeHeader(src)
		case pngPhaseData:
			src, ok = v.consumeData(src)
		case pngPhaseCRC:
			src, ok = v.consumeCRC(src)
		default:
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

func (v *pngStreamValidator) consumeSignature(src []byte) ([]byte, bool) {
	n := min(len(src), len(pngSignature)-v.signatureN)
	for i := range n {
		if src[i] != pngSignature[v.signatureN+i] {
			return nil, false
		}
	}
	v.signatureN += n
	v.read += int64(n)
	if v.signatureN == len(pngSignature) {
		v.phase = pngPhaseHeader
	}
	return src[n:], true
}

func (v *pngStreamValidator) consumeHeader(src []byte) ([]byte, bool) {
	n := min(len(src), len(v.header)-v.headerN)
	copy(v.header[v.headerN:], src[:n])
	v.headerN += n
	v.read += int64(n)
	src = src[n:]
	if v.headerN != len(v.header) {
		return src, true
	}

	length := int64(binary.BigEndian.Uint32(v.header[:4]))
	copy(v.chunkType[:], v.header[4:])
	if !validPNGChunkType(v.chunkType) || v.read > v.total || length+4 > v.total-v.read {
		return nil, false
	}
	if v.firstChunk {
		if v.chunkType != [4]byte{'I', 'H', 'D', 'R'} || length != 13 {
			return nil, false
		}
		v.firstChunk = false
	} else if v.chunkType == [4]byte{'I', 'H', 'D', 'R'} {
		return nil, false
	}
	if v.chunkType == [4]byte{'I', 'E', 'N', 'D'} && (length != 0 || !v.seenIDAT || v.total-v.read != 4) {
		return nil, false
	}

	v.dataLeft = length
	v.crc = updatePNGChunkTypeCRC(v.chunkType)
	v.phase = pngPhaseData
	if length == 0 {
		v.phase = pngPhaseCRC
	}
	return src, true
}

func (v *pngStreamValidator) consumeData(src []byte) ([]byte, bool) {
	n := int64(len(src))
	if n > v.dataLeft {
		n = v.dataLeft
	}
	v.crc = crc32.Update(v.crc, pngCRCTable, src[:n])
	v.dataLeft -= n
	v.read += n
	if v.dataLeft == 0 {
		v.phase = pngPhaseCRC
	}
	return src[n:], true
}

func (v *pngStreamValidator) consumeCRC(src []byte) ([]byte, bool) {
	n := min(len(src), len(v.crcBytes)-v.crcN)
	copy(v.crcBytes[v.crcN:], src[:n])
	v.crcN += n
	v.read += int64(n)
	src = src[n:]
	if v.crcN != len(v.crcBytes) {
		return src, true
	}
	if binary.BigEndian.Uint32(v.crcBytes[:]) != v.crc {
		return nil, false
	}
	if v.chunkType == [4]byte{'I', 'D', 'A', 'T'} {
		v.seenIDAT = true
	}
	if v.chunkType == [4]byte{'I', 'E', 'N', 'D'} {
		v.ended = true
		return src, true
	}
	v.headerN = 0
	v.crcN = 0
	v.phase = pngPhaseHeader
	return src, true
}

func (v *pngStreamValidator) complete() bool {
	return v.ended && v.read == v.total
}

func validPNGChunkType(chunkType [4]byte) bool {
	for _, c := range chunkType {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return chunkType[2] >= 'A' && chunkType[2] <= 'Z'
}

func updatePNGChunkTypeCRC(chunkType [4]byte) uint32 {
	crc := ^uint32(0)
	for _, b := range chunkType {
		crc = pngCRCTable[(crc&0xff)^uint32(b)] ^ crc>>8
	}
	return ^crc
}

// LiveEncoder encodes a stream of frames into Kitty graphics APC payloads while
// reusing its scratch buffers across frames, for the live viewer draw path where
// the same terminal receives frame after frame. It differs from the stateless
// Renderer in three ways that cut the per-frame encode cost: it reads the source
// *image.RGBA pixel buffer in bulk rather than pixel by pixel through the
// image.Image interface, it packs 24-bit RGB (f=24) and drops the opaque alpha so
// zlib compresses a quarter fewer bytes and the base64 body is smaller, and it
// compresses at zlib BestSpeed. It uses klauspost/compress zlib rather than the
// stdlib: its deflate is faster on large frames (roughly a quarter off encode
// time for a few percent more bytes), which cuts the per-frame paint wall on the
// live draw path. The output is a standard zlib stream that the terminal decodes
// unchanged. The emitted bytes stay a valid Kitty
// transmit-and-place command that renders the same opaque image; the source frame
// is treated as fully opaque, so dropping alpha changes no visible pixel. A
// LiveEncoder is not safe for concurrent use; the viewer drives it from one
// goroutine.
type LiveEncoder struct {
	rgb   []byte       // reused RGB pack scratch, 3 bytes per pixel
	zbuf  sliceWriter  // reused zlib output sink
	zw    *zlib.Writer // reused BestSpeed zlib writer, Reset per frame
	b64   []byte       // reused base64 destination
	frame []byte       // reused APC frame assembly buffer
}

// sliceWriter is a grow-only io.Writer backed by a reused byte slice, so the
// zlib writer appends compressed output without a fresh allocation each frame.
type sliceWriter struct {
	buf []byte
}

// Write appends p to the buffer and never fails, satisfying io.Writer.
func (s *sliceWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// reset truncates the buffer to empty while keeping its capacity for reuse.
func (s *sliceWriter) reset() { s.buf = s.buf[:0] }

// EncodeID encodes img as a zlib-compressed 24-bit RGB Kitty transmit-and-place
// command tagged with the given image id and writes the framed APC payload to w,
// reusing the encoder's scratch buffers. The id tags the image (i=<id>) so the
// caller can double-buffer by alternating two ids and deleting the previous one
// with DeleteImage. img must be a non-nil *image.RGBA. Its alpha is treated as
// opaque and omitted from the payload (f=24), so the terminal renders the RGB
// pixels directly. A payload over the protocol chunk limit is split across
// successive APC frames with the m= key, all assembled into one reused buffer and
// written in a single call. The RGBA byte order of the source is preserved: the
// pack copies R, G, B in order and skips A.
func (e *LiveEncoder) EncodeID(img *image.RGBA, id int, w io.Writer) error {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()

	e.packRGB(img)

	e.zbuf.reset()
	if e.zw == nil {
		zw, err := zlib.NewWriterLevel(&e.zbuf, zlib.BestSpeed)
		if err != nil {
			return fmt.Errorf("kitty: init zlib writer: %w", err)
		}
		e.zw = zw
	} else {
		e.zw.Reset(&e.zbuf)
	}
	if _, err := e.zw.Write(e.rgb); err != nil {
		return fmt.Errorf("kitty: compress image: %w", err)
	}
	if err := e.zw.Close(); err != nil {
		return fmt.Errorf("kitty: flush zlib writer: %w", err)
	}

	need := emmb64.StdEncoding.EncodedLen(len(e.zbuf.buf))
	if cap(e.b64) < need {
		e.b64 = make([]byte, need)
	}
	e.b64 = e.b64[:need]
	emmb64.StdEncoding.Encode(e.b64, e.zbuf.buf)

	opts := kitty.Options{
		Format:       kitty.RGB,
		Compression:  kitty.Zlib,
		Transmission: kitty.Direct,
		Action:       kitty.TransmitAndPut,
		Quite:        2, // upstream spelling; sets q=2, suppress replies
		ImageWidth:   width,
		ImageHeight:  height,
	}
	if id > 0 {
		opts.ID = id
	}

	e.frame = e.frame[:0]
	optList := opts.Options()
	if len(e.b64) <= kitty.MaxChunkSize {
		e.frame = appendFrame(e.frame, strings.Join(optList, ","), e.b64)
		if _, err := w.Write(e.frame); err != nil {
			return fmt.Errorf("kitty: write APC payload: %w", err)
		}
		return nil
	}

	control := strings.Join(optList, ",") + "," + chunkMore
	payload := e.b64
	for len(payload) > kitty.MaxChunkSize {
		e.frame = appendFrame(e.frame, control, payload[:kitty.MaxChunkSize])
		payload = payload[kitty.MaxChunkSize:]
		control = chunkMore
	}
	e.frame = appendFrame(e.frame, chunkLast, payload)
	if _, err := w.Write(e.frame); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}

// EncodePNGID frames an owned CDP base64 PNG payload directly as Kitty f=100
// data. Validation decodes bounded base64 chunks into pooled scratch, but the path
// does not materialise a full decode, PNG-decode, repack, compress, or re-encode
// the payload. The command uses direct transmission and transmit-and-put, tags
// the image with id when positive, and chunks at the Kitty protocol limit. A
// malformed or incomplete payload returns ErrInvalidPNGData before writing, so
// the viewer can use its decoded fallback without a partial terminal command.
func (e *LiveEncoder) EncodePNGID(data string, id int, w io.Writer) error {
	if !ValidDirectPNGData(data) {
		return ErrInvalidPNGData
	}
	control := "f=100,t=d,a=T,q=2"
	if id > 0 {
		control += fmt.Sprintf(",i=%d", id)
	}

	e.frame = e.frame[:0]
	if len(data) <= kitty.MaxChunkSize {
		e.frame = appendStringFrame(e.frame, control, data)
	} else {
		first := control + "," + chunkMore
		for len(data) > kitty.MaxChunkSize {
			e.frame = appendStringFrame(e.frame, first, data[:kitty.MaxChunkSize])
			data = data[kitty.MaxChunkSize:]
			first = chunkMore
		}
		e.frame = appendStringFrame(e.frame, chunkLast, data)
	}
	if _, err := w.Write(e.frame); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}

// packRGB copies img's R, G, B bytes (skipping the alpha byte) into the reused
// rgb scratch, growing it only when the frame grows. It reads img.Pix in row
// order via the stride, so a sub-image or padded row stays correct, and preserves
// RGBA channel order by copying the first three bytes of each 4-byte pixel.
func (e *LiveEncoder) packRGB(img *image.RGBA) {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	n := width * height * 3
	if cap(e.rgb) < n {
		e.rgb = make([]byte, n)
	}
	e.rgb = e.rgb[:n]

	di := 0
	rowBytes := width * 4
	for y := range height {
		o := img.PixOffset(b.Min.X, b.Min.Y+y)
		row := img.Pix[o : o+rowBytes]
		for i := 0; i < rowBytes; i += 4 {
			e.rgb[di] = row[i]
			e.rgb[di+1] = row[i+1]
			e.rgb[di+2] = row[i+2]
			di += 3
		}
	}
}

// appendFrame appends one APC frame (ESC _ G, the control section, a semicolon,
// the chunk body, then the ESC backslash terminator) to dst and returns the
// extended slice. It builds into the reused buffer with no per-chunk string
// allocation, unlike the stateless writeFrame.
func appendFrame(dst []byte, control string, chunk []byte) []byte {
	dst = append(dst, apcIntro...)
	dst = append(dst, control...)
	dst = append(dst, ';')
	dst = append(dst, chunk...)
	dst = append(dst, st...)
	return dst
}

// appendStringFrame is appendFrame for an already-base64 string. Appending the
// string copies its bytes straight into the reusable APC buffer without an
// intermediate []byte allocation.
func appendStringFrame(dst []byte, control, chunk string) []byte {
	dst = append(dst, apcIntro...)
	dst = append(dst, control...)
	dst = append(dst, ';')
	dst = append(dst, chunk...)
	dst = append(dst, st...)
	return dst
}
