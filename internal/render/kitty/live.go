package kitty

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	emmb64 "github.com/emmansun/base64"

	"github.com/charmbracelet/x/ansi/kitty"
)

// ErrInvalidPNGData reports that a direct-PNG payload is not the standard
// base64 PNG form CDP emits.
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

// LiveEncoder frames direct PNG data into Kitty graphics APC payloads while
// reusing its assembly buffer across live frames. It is not safe for concurrent
// use; the viewer drives it from one goroutine.
type LiveEncoder struct {
	frame []byte // reused APC frame assembly buffer
}

// EncodePNGID frames an owned CDP base64 PNG payload directly as Kitty f=100
// data. Validation decodes bounded base64 chunks into pooled scratch, but the path
// does not materialise a full decode, PNG-decode, repack, compress, or re-encode
// the payload. The command uses direct transmission and transmit-and-put, tags
// the image with id when positive, leaves the cursor in place so a full-height
// placement cannot scroll the terminal, and chunks at the Kitty protocol limit.
// A positive cols and rows pair adds the c= and r= placement keys, so the
// terminal scales the image to exactly that cell area: after a mid-session zoom
// Chromium keeps painting at its launch density, so the delivered frame is the
// complete viewport at a stale pixel size, and the grid-fit placement keeps it
// spanning the terminal (a matching pixel size scales 1:1, a no-op). A
// non-positive pair omits both keys. A malformed or incomplete payload returns
// ErrInvalidPNGData before writing.
func (e *LiveEncoder) EncodePNGID(data string, id, cols, rows int, w io.Writer) error {
	return e.encodePNGID(data, id, 0, false, cols, rows, w)
}

// EncodePNGIDAtZ is EncodePNGID with an explicit Kitty placement z-index. The
// z option is emitted only on the first APC chunk; continuation chunks retain
// their protocol-only m control. The viewer uses this live-only variant when a
// terminal text badge must remain above the page image.
func (e *LiveEncoder) EncodePNGIDAtZ(data string, id, z, cols, rows int, w io.Writer) error {
	return e.encodePNGID(data, id, z, true, cols, rows, w)
}

func (e *LiveEncoder) encodePNGID(data string, id, z int, withZ bool, cols, rows int, w io.Writer) error {
	control := "f=100,t=d,a=T,q=2,C=1"
	if cols > 0 && rows > 0 {
		control += fmt.Sprintf(",c=%d,r=%d", cols, rows)
	}
	if id > 0 {
		control += fmt.Sprintf(",i=%d", id)
	}
	if withZ {
		control += fmt.Sprintf(",z=%d", z)
	}
	return e.writePNG(data, control, false, w)
}

// EncodeTmuxPNGID sends a virtual placement for the given image and cell grid.
// Each Kitty APC has its own tmux passthrough envelope. The caller displays the
// image with Unicode placeholder cells outside those envelopes.
func (e *LiveEncoder) EncodeTmuxPNGID(data string, id, cols, rows int, w io.Writer) error {
	control := fmt.Sprintf("f=100,t=d,a=T,q=2,U=1,c=%d,r=%d,i=%d", cols, rows, id)
	return e.writePNG(data, control, true, w)
}

func (e *LiveEncoder) writePNG(data, control string, tmux bool, w io.Writer) error {
	if !ValidDirectPNGData(data) {
		return ErrInvalidPNGData
	}

	e.frame = e.frame[:0]
	if len(data) <= kitty.MaxChunkSize {
		e.frame = appendPNGChunk(e.frame, control, data, tmux)
	} else {
		first := control + "," + chunkMore
		for len(data) > kitty.MaxChunkSize {
			e.frame = appendPNGChunk(e.frame, first, data[:kitty.MaxChunkSize], tmux)
			data = data[kitty.MaxChunkSize:]
			first = chunkMore
		}
		e.frame = appendPNGChunk(e.frame, chunkLast, data, tmux)
	}
	if _, err := w.Write(e.frame); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}

// EncodeSHMID frames a POSIX shared-memory object name as one unchunked Kitty
// raw-pixel command: f=32 RGBA pixels of the given width and height, read by
// the terminal from the named object (t=s). The whole frame travels in a
// single escape, never chunked, because chunked shm transmission fails on
// Ghostty, so no m= key is ever emitted on this path. A positive cols and rows
// pair adds the c= and r= placement keys, so the terminal scales the image to
// exactly that cell area (the post-zoom stale-density fit EncodePNGID
// documents); a non-positive pair omits both keys. The quiet key controls
// terminal responses: q=1 keeps OK acknowledgements for pacing, q=2 suppresses
// every response; the caller chooses per session.
func (e *LiveEncoder) EncodeSHMID(name string, width, height, id, quiet, cols, rows int, w io.Writer) error {
	return e.encodeSHMID(name, width, height, id, 0, false, quiet, cols, rows, w)
}

// EncodeSHMIDAtZ is EncodeSHMID with an explicit Kitty placement z-index, the
// shm counterpart of EncodePNGIDAtZ for placements that must sit below a
// terminal text badge.
func (e *LiveEncoder) EncodeSHMIDAtZ(name string, width, height, id, z, quiet, cols, rows int, w io.Writer) error {
	return e.encodeSHMID(name, width, height, id, z, true, quiet, cols, rows, w)
}

func (e *LiveEncoder) encodeSHMID(name string, width, height, id, z int, withZ bool, quiet, cols, rows int, w io.Writer) error {
	e.frame = e.frame[:0]
	e.frame = append(e.frame, apcIntro...)
	e.frame = append(e.frame, "f=32,t=s,s="...)
	e.frame = strconv.AppendInt(e.frame, int64(width), 10)
	e.frame = append(e.frame, ",v="...)
	e.frame = strconv.AppendInt(e.frame, int64(height), 10)
	e.frame = append(e.frame, ",a=T,C=1"...)
	if cols > 0 && rows > 0 {
		e.frame = append(e.frame, ",c="...)
		e.frame = strconv.AppendInt(e.frame, int64(cols), 10)
		e.frame = append(e.frame, ",r="...)
		e.frame = strconv.AppendInt(e.frame, int64(rows), 10)
	}
	e.frame = append(e.frame, ",i="...)
	e.frame = strconv.AppendInt(e.frame, int64(id), 10)
	if withZ {
		e.frame = append(e.frame, ",z="...)
		e.frame = strconv.AppendInt(e.frame, int64(z), 10)
	}
	e.frame = append(e.frame, ",q="...)
	e.frame = strconv.AppendInt(e.frame, int64(quiet), 10)
	e.frame = append(e.frame, ';')

	// Protocol payloads are base64. The object name encodes in place at the
	// end of the reused frame buffer; the zero-copy string view mirrors
	// ValidDirectPNGData, so the steady state allocates nothing per frame.
	off := len(e.frame)
	e.frame = append(e.frame, make([]byte, emmb64.StdEncoding.EncodedLen(len(name)))...)
	emmb64.StdEncoding.Encode(e.frame[off:], unsafe.Slice(unsafe.StringData(name), len(name)))
	e.frame = append(e.frame, st...)
	if _, err := w.Write(e.frame); err != nil {
		return fmt.Errorf("kitty: write APC payload: %w", err)
	}
	return nil
}

const tmuxDCSIntro = "\x1bPtmux;"

// WrapTmuxAPC wraps one complete Kitty command for tmux passthrough.
func WrapTmuxAPC(command string) string {
	return tmuxDCSIntro + strings.ReplaceAll(command, "\x1b", "\x1b\x1b") + st
}

// appendPNGChunk copies the base64 string straight into the reused APC buffer.
// The control keys and base64 cannot contain ESC, so only the APC introducer
// and terminator need doubling when wrapped for tmux.
func appendPNGChunk(dst []byte, control, chunk string, tmux bool) []byte {
	if tmux {
		dst = append(dst, tmuxDCSIntro...)
		dst = append(dst, '\x1b')
	}
	dst = append(dst, apcIntro...)
	dst = append(dst, control...)
	dst = append(dst, ';')
	dst = append(dst, chunk...)
	if tmux {
		dst = append(dst, '\x1b')
		dst = append(dst, st...)
	}
	dst = append(dst, st...)
	return dst
}
