//go:build darwin

package kitty

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/wimpysworld/nyala/internal/posixshm"
	"golang.org/x/sys/unix"
)

// shmSeq numbers shared-memory objects across every SHMWriter in the process.
// The pid and process-wide sequence keep names unique across writers and
// concurrent Nyala processes.
var shmSeq atomic.Uint64

// SHMObject is one writable POSIX shared-memory object holding a Kitty t=s
// frame payload. The caller fills Data, closes the object to release the
// mapping, and sends Name in the escape payload. The SHMWriter that created
// the object owns its unlinking.
type SHMObject struct {
	name  string
	data  []byte
	close func() error
}

// Name returns the POSIX object name for the Kitty escape payload, in the
// leading-slash form shm_open expects.
func (o *SHMObject) Name() string { return o.name }

// Data returns the writable mapping of the object. A frame encoder decodes
// base64 straight into this slice, so the payload lands in shared memory
// without an intermediate heap buffer. It is nil after Close.
func (o *SHMObject) Data() []byte { return o.data }

// Close releases the mapping. It does not unlink the object; the terminal
// unlinks objects it consumes and the SHMWriter unlinks the rest. Close is
// idempotent.
func (o *SHMObject) Close() error {
	if o.data == nil {
		return nil
	}
	o.data = nil
	if err := o.close(); err != nil {
		return fmt.Errorf("kitty: unmap shm object %s: %w", o.name, err)
	}
	return nil
}

// SHMWriter creates uniquely named POSIX shared-memory objects for the Kitty
// t=s transport, one object per frame. Kitty unlinks each object it consumes,
// so the writer tracks the newest object per double-buffer image id and
// unlinks only objects the terminal never consumed: an object replaced before
// consumption, or one still tracked at Close. It is not safe for concurrent
// use; the viewer drives it from one goroutine.
type SHMWriter struct {
	last map[int]string
}

// NewSHMWriter returns a writer with no tracked objects.
func NewSHMWriter() *SHMWriter {
	return &SHMWriter{last: map[int]string{}}
}

// Create makes a new size-byte object for the given double-buffer image id and
// returns it mapped for writing. The previous object tracked under the same id
// is unlinked if the terminal has not already consumed it. Size must be
// positive. A creation failure leaves nothing behind.
func (w *SHMWriter) Create(id, size int) (*SHMObject, error) {
	obj, err := createSHMObject(size)
	if err != nil {
		return nil, err
	}
	if prev, ok := w.last[id]; ok {
		if err := unlinkSHM(prev); err != nil {
			return nil, errors.Join(err, obj.Close(), unlinkSHM(obj.name))
		}
	}
	w.last[id] = obj.name
	return obj, nil
}

// WriteFrame copies payload into a new object for the given double-buffer
// image id and returns the object name for the escape payload. It is the copy
// counterpart of Create for callers that already hold the payload bytes.
func (w *SHMWriter) WriteFrame(id int, payload []byte) (string, error) {
	obj, err := w.Create(id, len(payload))
	if err != nil {
		return "", err
	}
	return writeSHMObject(obj, payload)
}

// Close unlinks every tracked object the terminal never consumed and clears
// the tracking. Objects the terminal already unlinked are skipped silently.
func (w *SHMWriter) Close() error {
	var errs []error
	for id, name := range w.last {
		if err := unlinkSHM(name); err != nil {
			errs = append(errs, err)
		}
		delete(w.last, id)
	}
	return errors.Join(errs...)
}

func writeSHMObject(obj *SHMObject, payload []byte) (string, error) {
	copy(obj.Data(), payload)
	if err := obj.Close(); err != nil {
		return "", err
	}
	return obj.Name(), nil
}

func createSHMObject(size int) (*SHMObject, error) {
	if size <= 0 {
		return nil, fmt.Errorf("kitty: shm object size %d, want positive", size)
	}
	for range 100 {
		name := darwinSHMName(os.Getpid(), shmSeq.Add(1))
		if err := posixshm.ValidateName(name); err != nil {
			return nil, fmt.Errorf("kitty: guard shm object name %s: %w", name, err)
		}
		mapped, err := posixshm.Create(name, size)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("kitty: create shm object %s: %w", name, err)
		}
		return &SHMObject{
			name:  mapped.Name(),
			data:  mapped.Data(),
			close: mapped.Close,
		}, nil
	}
	return nil, errors.New("kitty: no free shm object name after 100 attempts")
}

func unlinkSHM(name string) error {
	if err := posixshm.Unlink(name); err != nil {
		return fmt.Errorf("kitty: unlink shm object %s: %w", name, err)
	}
	return nil
}
