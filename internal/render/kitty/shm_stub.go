//go:build !linux

package kitty

import (
	"errors"
	"fmt"
)

// errSHMUnsupported reports that the Kitty t=s shared-memory transport is
// Linux-only. It wraps errors.ErrUnsupported so callers can test for it.
var errSHMUnsupported = fmt.Errorf("kitty: shm transport: %w", errors.ErrUnsupported)

// SHMObject is the non-Linux stub of the shared-memory frame object. It is
// never produced because SHMWriter cannot create objects here.
type SHMObject struct{}

// Name returns the empty string on non-Linux platforms.
func (o *SHMObject) Name() string { return "" }

// Data returns nil on non-Linux platforms.
func (o *SHMObject) Data() []byte { return nil }

// Close is a no-op on non-Linux platforms.
func (o *SHMObject) Close() error { return nil }

// SHMWriter is the non-Linux stub of the shared-memory frame writer. Every
// creation method fails with errors.ErrUnsupported, so the viewer's transport
// resolution falls back to the direct path.
type SHMWriter struct{}

// NewSHMWriter returns the stub writer.
func NewSHMWriter() *SHMWriter { return &SHMWriter{} }

// Create fails with errors.ErrUnsupported on non-Linux platforms.
func (w *SHMWriter) Create(id, size int) (*SHMObject, error) { return nil, errSHMUnsupported }

// WriteFrame fails with errors.ErrUnsupported on non-Linux platforms.
func (w *SHMWriter) WriteFrame(id int, payload []byte) (string, error) {
	return "", errSHMUnsupported
}

// Close is a no-op on non-Linux platforms.
func (w *SHMWriter) Close() error { return nil }
