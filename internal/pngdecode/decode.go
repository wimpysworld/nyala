// Package pngdecode decodes production PNG frames with reusable row buffers.
package pngdecode

import (
	"image"
	"io"

	woozypng "github.com/woozymasta/png"
)

const poolDepth = 8

type bufferPool chan []byte

func (p bufferPool) Get(size int) []byte {
	select {
	case buf := <-p:
		if cap(buf) >= size {
			return buf[:size]
		}
	default:
	}
	return make([]byte, size)
}

func (p bufferPool) Put(buf []byte) {
	select {
	case p <- buf:
	default:
	}
}

// rowPool is the shared production row-buffer pool. Decode hands it to the
// upstream decoder; DecodeInto borrows its current and previous row buffers
// from it directly.
var rowPool = make(bufferPool, poolDepth)

var decoder = woozypng.Decoder{BufferPool: rowPool}

// Decode reads a PNG image using the shared production row-buffer pool.
func Decode(r io.Reader) (image.Image, error) {
	return decoder.Decode(r)
}
