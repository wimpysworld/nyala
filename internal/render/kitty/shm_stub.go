//go:build !linux && !darwin

package kitty

import (
	"errors"
	"fmt"
)

// errSHMUnsupported reports that the Kitty t=s shared-memory transport is not
// available on this platform. It wraps errors.ErrUnsupported for callers.
var errSHMUnsupported = fmt.Errorf("kitty: shm transport: %w", errors.ErrUnsupported)

// SHMObject is the unsupported-platform stub of the shared-memory frame
// object. It is never produced because SHMWriter cannot create objects here.
type SHMObject struct{}

// Name returns the empty string on unsupported platforms.
func (o *SHMObject) Name() string { return "" }

// Data returns nil on unsupported platforms.
func (o *SHMObject) Data() []byte { return nil }

// Close is a no-op on unsupported platforms.
func (o *SHMObject) Close() error { return nil }

// SHMWriter is the unsupported-platform stub of the shared-memory frame
// writer. Every creation method fails with errors.ErrUnsupported, so the
// viewer's transport resolution falls back to the direct path.
type SHMWriter struct{}

// NewSHMWriter returns the stub writer.
func NewSHMWriter() *SHMWriter { return &SHMWriter{} }

// Create fails with errors.ErrUnsupported on unsupported platforms.
func (w *SHMWriter) Create(id, size int) (*SHMObject, error) { return nil, errSHMUnsupported }

// WriteFrame fails with errors.ErrUnsupported on unsupported platforms.
func (w *SHMWriter) WriteFrame(id int, payload []byte) (string, error) {
	return "", errSHMUnsupported
}

// Close is a no-op on unsupported platforms.
func (w *SHMWriter) Close() error { return nil }
