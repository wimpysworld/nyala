//go:build linux

package kitty

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// shmDir is the tmpfs directory backing POSIX shared-memory objects on Linux.
// golang.org/x/sys/unix exposes no ShmOpen wrapper for Linux because glibc's
// shm_open is open(2) on this directory with O_CLOEXEC and O_NOFOLLOW, so the
// writer opens the path directly and the module stays cgo-free.
const shmDir = "/dev/shm/"

// shmSeq numbers shared-memory objects across every SHMWriter in the process.
// One process-wide counter keeps names unique when two writers run at once,
// and the pid in the name keeps them unique across concurrent Nyala instances.
var shmSeq atomic.Uint64

// SHMObject is one writable POSIX shared-memory object holding a Kitty t=s
// frame payload. The caller fills Data, closes the object to release the
// mapping, and sends Name in the escape payload. The SHMWriter that created
// the object owns its unlinking.
type SHMObject struct {
	name string
	data []byte
}

// Name returns the POSIX object name for the Kitty escape payload, in the
// leading-slash form shm_open expects, for example /nyala-1234-7.
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
	data := o.data
	o.data = nil
	if err := unix.Munmap(data); err != nil {
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
	copy(obj.Data(), payload)
	if err := obj.Close(); err != nil {
		return "", err
	}
	return obj.Name(), nil
}

// Close unlinks every tracked object the terminal never consumed and clears
// the tracking, so no orphaned /dev/shm entries outlive the session. Objects
// the terminal already unlinked are skipped silently.
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

// createSHMObject creates and maps one fresh object. O_EXCL on a pid-and-
// sequence name makes creation collision-safe: a stale entry from a crashed
// process that reused the pid yields EEXIST, and the loop moves to the next
// sequence number.
func createSHMObject(size int) (*SHMObject, error) {
	if size <= 0 {
		return nil, fmt.Errorf("kitty: shm object size %d, want positive", size)
	}
	for range 100 {
		name := fmt.Sprintf("/nyala-%d-%d", os.Getpid(), shmSeq.Add(1))
		fd, err := unix.Open(shmDir+name[1:],
			unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("kitty: create shm object %s: %w", name, err)
		}
		if err := unix.Ftruncate(fd, int64(size)); err != nil {
			err = fmt.Errorf("kitty: size shm object %s: %w", name, err)
			return nil, errors.Join(err, closeFD(fd, name), unlinkSHM(name))
		}
		data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			err = fmt.Errorf("kitty: map shm object %s: %w", name, err)
			return nil, errors.Join(err, closeFD(fd, name), unlinkSHM(name))
		}
		if err := closeFD(fd, name); err != nil {
			return nil, errors.Join(err, unix.Munmap(data), unlinkSHM(name))
		}
		return &SHMObject{name: name, data: data}, nil
	}
	return nil, errors.New("kitty: no free shm object name after 100 attempts")
}

// closeFD closes the object's file descriptor once the mapping no longer
// needs it, wrapping a failure with the object name.
func closeFD(fd int, name string) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("kitty: close shm object %s: %w", name, err)
	}
	return nil
}

// unlinkSHM removes the named object from /dev/shm. ENOENT means the terminal
// already consumed and unlinked it, which is the expected happy path, so it is
// not an error.
func unlinkSHM(name string) error {
	err := unix.Unlink(shmDir + strings.TrimPrefix(name, "/"))
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("kitty: unlink shm object %s: %w", name, err)
	}
	return nil
}
