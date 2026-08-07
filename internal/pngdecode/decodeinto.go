// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file is a minimal in-package adaptation of the row-decode loop in
// github.com/woozymasta/png (itself derived from the Go standard library
// image/png reader, hence the licence header above). It decodes the two PNG
// shapes Chromium screencasts emit, non-interlaced 8-bit truecolour with and
// without alpha, straight into a caller-owned RGBA plane, removing the fresh
// image allocation the upstream decoder makes on every decode. The adaptation
// is a candidate upstream pull request against woozymasta/png as a DecodeInto
// API; until that lands the fork lives here.

package pngdecode

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/klauspost/compress/zlib"
)

// pngHeader is the 8-byte PNG file signature.
const pngHeader = "\x89PNG\r\n\x1a\n"

// Truecolour colour types, as per the PNG spec.
const (
	ctTrueColor      = 2
	ctTrueColorAlpha = 6
)

// Filter types, as per the PNG spec.
const (
	ftNone    = 0
	ftSub     = 1
	ftUp      = 2
	ftAverage = 3
	ftPaeth   = 4
)

// ErrDstTooSmall reports that the destination plane passed to DecodeInto
// cannot hold the decoded image at four bytes per pixel.
var ErrDstTooSmall = errors.New("pngdecode: destination plane too small")

// ErrUnsupportedPNG reports that DecodeInto was given a PNG feature or chunk
// sequence that its direct RGB and RGBA decoder does not support.
var ErrUnsupportedPNG = errors.New("pngdecode: unsupported PNG")

// maxDecodeDim bounds each IHDR dimension the fast path accepts, so the
// 4*w*h size arithmetic cannot overflow int and admit a too-small dst.
const maxDecodeDim = 1 << 20

// errFormat is the generic malformed-stream error for the direct decoder.
var errFormat = errors.New("pngdecode: invalid PNG stream")

// Dimensions reads the pixel size of a PNG payload from its IHDR without
// decoding, so a caller can size a destination plane before DecodeInto.
// ok is false when raw is not a PNG or is shorter than the IHDR fields.
func Dimensions(raw []byte) (w, h int, ok bool) {
	if len(raw) < 24 || string(raw[:8]) != pngHeader || string(raw[12:16]) != "IHDR" {
		return 0, 0, false
	}
	w = int(binary.BigEndian.Uint32(raw[16:20]))
	h = int(binary.BigEndian.Uint32(raw[20:24]))
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// intoState is the pooled per-decode state of the fast path: the IDAT
// re-chunking reader, the zlib inflater reset onto it, and the small header
// scratch buffers. Reads through an io.Reader interface make a stack buffer
// escape, so the scratch lives in this pooled state and a steady-state decode
// allocates nothing.
type intoState struct {
	idat idatReader
	zr   io.ReadCloser
	// head holds the PNG signature and IHDR chunk (8 + 25 bytes).
	head [33]byte
	// tmp holds one chunk header or the trailing EOF probe byte.
	tmp [8]byte
}

// intoPool recycles intoState values across decodes.
var intoPool = sync.Pool{New: func() any { return new(intoState) }}

// DecodeInto decodes one PNG from r into dst as tightly packed RGBA rows
// (RGBA byte order, stride 4*w) and reports the pixel size. Non-interlaced
// 8-bit truecolour images, the shapes Chromium screencasts emit, decode row
// by row into dst with no per-decode image allocation; an RGB row expands
// with alpha 0xFF. Other PNG shapes return ErrUnsupportedPNG. dst must hold at
// least 4*w*h bytes or DecodeInto returns ErrDstTooSmall.
func DecodeInto(r io.Reader, dst []byte) (w, h int, err error) {
	st, _ := intoPool.Get().(*intoState)
	defer func() {
		st.idat.r = nil // do not retain the caller's reader in the pool
		intoPool.Put(st)
	}()
	// Signature (8) + IHDR length (4) + "IHDR" (4) + IHDR data (13) + CRC (4).
	head := st.head[:]
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, 0, err
	}
	if string(head[:8]) != pngHeader ||
		binary.BigEndian.Uint32(head[8:12]) != 13 || string(head[12:16]) != "IHDR" {
		return 0, 0, errFormat
	}
	w = int(binary.BigEndian.Uint32(head[16:20]))
	h = int(binary.BigEndian.Uint32(head[20:24]))
	if w <= 0 || h <= 0 {
		return 0, 0, errFormat
	}
	depth, colour := head[24], head[25]
	compression, filter, interlace := head[26], head[27], head[28]
	if depth != 8 || (colour != ctTrueColor && colour != ctTrueColorAlpha) ||
		compression != 0 || filter != 0 || interlace != 0 {
		return 0, 0, ErrUnsupportedPNG
	}
	// The dimension cap keeps 4*w*h inside the int range: unchecked uint32
	// dimensions could overflow the size check and admit a dst far too small.
	// Chromium screencast frames sit orders of magnitude under it.
	if w > maxDecodeDim || h > maxDecodeDim {
		return 0, 0, ErrDstTooSmall
	}

	// Walk chunks up to the first IDAT. Ancillary chunks and one optional PLTE
	// are ignorable for truecolour. tRNS changes truecolour semantics.
	seenPLTE := false
	for {
		ch := st.tmp[:8]
		if _, err := io.ReadFull(r, ch); err != nil {
			return 0, 0, err
		}
		length := binary.BigEndian.Uint32(ch[:4])
		typ := string(ch[4:8])
		if typ == "IDAT" {
			if len(dst) < 4*w*h {
				return 0, 0, ErrDstTooSmall
			}
			return decodeTruecolourInto(st, r, dst, w, h, int(colour), length)
		}
		if typ == "tRNS" || typ == "IEND" || typ == "IHDR" {
			return 0, 0, ErrUnsupportedPNG
		}
		if typ == "PLTE" {
			if seenPLTE {
				return 0, 0, ErrUnsupportedPNG
			}
			seenPLTE = true
		} else if ch[4]&0x20 == 0 {
			return 0, 0, ErrUnsupportedPNG
		}
		if err := skipChunk(r, st.tmp[:], length); err != nil {
			return 0, 0, err
		}
	}
}

func skipChunk(r io.Reader, scratch []byte, length uint32) error {
	remaining := uint64(length) + 4 // chunk data plus CRC
	for remaining > 0 {
		n := min(uint64(len(scratch)), remaining)
		if _, err := io.ReadFull(r, scratch[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

// decodeTruecolourInto runs the fast row loop: inflate the IDAT stream,
// unfilter each row, and write it into dst as RGBA. colour selects 3- or
// 4-byte source pixels; firstLen is the length of the already-entered first
// IDAT chunk. Chunk CRCs are not verified: the zlib Adler-32 checksum covers
// the pixel data, and a corrupt stream fails the decode either way.
func decodeTruecolourInto(st *intoState, r io.Reader, dst []byte, w, h, colour int, firstLen uint32) (int, int, error) {
	st.idat = idatReader{r: r, remain: firstLen}
	if st.zr == nil {
		zr, err := zlib.NewReader(&st.idat)
		if err != nil {
			return 0, 0, err
		}
		st.zr = zr
	} else if err := st.zr.(zlib.Resetter).Reset(&st.idat, nil); err != nil {
		return 0, 0, err
	}

	bpp := 3
	if colour == ctTrueColorAlpha {
		bpp = 4
	}
	rowSize := 1 + w*bpp // +1 for the per-row filter byte
	cr := rowPool.Get(rowSize)
	pr := rowPool.Get(rowSize)
	defer func() {
		rowPool.Put(cr)
		rowPool.Put(pr)
	}()
	// The first row filters against an all-zero previous row; a pooled buffer
	// may hold stale bytes.
	clear(pr)

	for y := range h {
		if _, err := io.ReadFull(st.zr, cr); err != nil {
			if errors.Is(err, ErrUnsupportedPNG) {
				return 0, 0, ErrUnsupportedPNG
			}
			return 0, 0, errFormat
		}
		cdat, pdat := cr[1:], pr[1:]
		switch cr[0] {
		case ftNone:
			// No-op.
		case ftSub:
			for i := bpp; i < len(cdat); i++ {
				cdat[i] += cdat[i-bpp]
			}
		case ftUp:
			for i, p := range pdat {
				cdat[i] += p
			}
		case ftAverage:
			for i := range bpp {
				cdat[i] += pdat[i] / 2
			}
			for i := bpp; i < len(cdat); i++ {
				// The mean of two bytes always fits a byte; the mask proves it.
				cdat[i] += uint8((int(cdat[i-bpp]) + int(pdat[i])) / 2 & 0xFF)
			}
		case ftPaeth:
			if bpp == 4 {
				filterPaeth4(cdat, pdat)
			} else {
				filterPaeth3(cdat, pdat)
			}
		default:
			return 0, 0, errFormat
		}
		row := dst[y*w*4 : y*w*4+w*4]
		if bpp == 4 {
			copy(row, cdat)
		} else {
			// Expand RGB (3 bytes) to RGBA (4 bytes, alpha 0xFF).
			for x := range w {
				row[4*x+0] = cdat[3*x+0]
				row[4*x+1] = cdat[3*x+1]
				row[4*x+2] = cdat[3*x+2]
				row[4*x+3] = 0xFF
			}
		}
		// The current row for y is the previous row for y+1.
		pr, cr = cr, pr
	}

	// Drain to EOF so the zlib Adler-32 checksum verifies; a short bounded
	// retry tolerates a (0, nil) read.
	tb := st.tmp[:1]
	for range 100 {
		n, err := st.zr.Read(tb)
		if n != 0 {
			return 0, 0, errFormat
		}
		if err == io.EOF {
			return w, h, nil
		}
		if err != nil {
			return 0, 0, err
		}
	}
	return 0, 0, io.ErrNoProgress
}

// idatReader presents one or more IDAT chunks as one continuous zlib stream,
// adapted from the upstream decoder's Read without CRC accounting. remain is
// the unread byte count of the current chunk; between chunks it skips the
// 4-byte CRC and expects the next chunk to be IDAT.
type idatReader struct {
	r      io.Reader
	remain uint32
	// tmp holds the inter-chunk CRC-plus-header bytes and the ReadByte
	// scratch; a local buffer would escape through the io.Reader interface.
	tmp [12]byte
}

// Read implements io.Reader over the concatenated IDAT chunk data.
func (d *idatReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for d.remain == 0 {
		// Previous chunk CRC (4) + next chunk length (4) and type (4).
		if _, err := io.ReadFull(d.r, d.tmp[:]); err != nil {
			return 0, err
		}
		if string(d.tmp[8:12]) != "IDAT" {
			return 0, ErrUnsupportedPNG
		}
		d.remain = binary.BigEndian.Uint32(d.tmp[4:8])
	}
	n, err := d.r.Read(p[:min(len(p), int(d.remain))])
	// #nosec G115 -- n is bounded by the p[:min(len(p), int(d.remain))] slice.
	d.remain -= uint32(n)
	return n, err
}

// ReadByte implements io.ByteReader so idatReader satisfies flate.Reader:
// without it the pooled zlib inflater would wrap each Reset in a fresh
// bufio.Reader and re-allocate per decode.
func (d *idatReader) ReadByte() (byte, error) {
	for {
		n, err := d.Read(d.tmp[:1])
		if n == 1 {
			return d.tmp[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// intSize is either 32 or 64.
const intSize = 32 << (^uint(0) >> 63)

// abs is the branch-free absolute value the Paeth filters use, copied from
// the upstream paeth.go (Hacker's Delight, section 2-4).
func abs(x int) int {
	m := x >> (intSize - 1)
	return (x ^ m) - m
}

// filterPaeth3 applies the Paeth filter for 3-byte pixels, copied from the
// upstream paeth.go. cdat is the current row's data, pdat the previous row's.
func filterPaeth3(cdat, pdat []byte) {
	var a0, a1, a2, c0, c1, c2 int
	for j := 0; j < len(cdat); j += 3 {
		b0 := int(pdat[j])
		b1 := int(pdat[j+1])
		b2 := int(pdat[j+2])
		// byte 0
		pa := b0 - c0
		pb := a0 - c0
		pc := abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a0 = b0
			} else {
				a0 = c0
			}
		}
		a0 += int(cdat[j])
		a0 &= 0xff
		cdat[j] = uint8(a0)
		c0 = b0
		// byte 1
		pa = b1 - c1
		pb = a1 - c1
		pc = abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a1 = b1
			} else {
				a1 = c1
			}
		}
		a1 += int(cdat[j+1])
		a1 &= 0xff
		cdat[j+1] = uint8(a1)
		c1 = b1
		// byte 2
		pa = b2 - c2
		pb = a2 - c2
		pc = abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a2 = b2
			} else {
				a2 = c2
			}
		}
		a2 += int(cdat[j+2])
		a2 &= 0xff
		cdat[j+2] = uint8(a2)
		c2 = b2
	}
}

// filterPaeth4 applies the Paeth filter for 4-byte pixels, copied from the
// upstream paeth.go. cdat is the current row's data, pdat the previous row's.
func filterPaeth4(cdat, pdat []byte) {
	var a0, a1, a2, a3, c0, c1, c2, c3 int
	for j := 0; j < len(cdat); j += 4 {
		b0 := int(pdat[j])
		b1 := int(pdat[j+1])
		b2 := int(pdat[j+2])
		b3 := int(pdat[j+3])
		// byte 0
		pa := b0 - c0
		pb := a0 - c0
		pc := abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a0 = b0
			} else {
				a0 = c0
			}
		}
		a0 += int(cdat[j])
		a0 &= 0xff
		cdat[j] = uint8(a0)
		c0 = b0
		// byte 1
		pa = b1 - c1
		pb = a1 - c1
		pc = abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a1 = b1
			} else {
				a1 = c1
			}
		}
		a1 += int(cdat[j+1])
		a1 &= 0xff
		cdat[j+1] = uint8(a1)
		c1 = b1
		// byte 2
		pa = b2 - c2
		pb = a2 - c2
		pc = abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a2 = b2
			} else {
				a2 = c2
			}
		}
		a2 += int(cdat[j+2])
		a2 &= 0xff
		cdat[j+2] = uint8(a2)
		c2 = b2
		// byte 3
		pa = b3 - c3
		pb = a3 - c3
		pc = abs(pa + pb)
		pa, pb = abs(pa), abs(pb)
		if pa > pb || pa > pc {
			if pb <= pc {
				a3 = b3
			} else {
				a3 = c3
			}
		}
		a3 += int(cdat[j+3])
		a3 &= 0xff
		cdat[j+3] = uint8(a3)
		c3 = b3
	}
}
