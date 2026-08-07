//go:build darwin

package posixshm

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

const libSystem = "/usr/lib/libSystem.B.dylib"

var (
	loadOnce     sync.Once
	loadErr      error
	shmOpenSym   uintptr
	shmUnlinkSym uintptr
)

// Object is a writable mapping of one Darwin POSIX shared-memory object.
// Close releases the mapping, while Unlink removes the named object.
type Object struct {
	name string
	data []byte
}

// Create creates, sizes, and maps a fresh shared-memory object. Name must pass
// ValidateName and size must be positive. A failed call removes any object it
// created.
func Create(name string, size int) (*Object, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, fmt.Errorf("posixshm: object size %d, want positive", size)
	}
	if err := loadLibSystem(); err != nil {
		return nil, err
	}

	fd, err := shmOpen(name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("posixshm: create object %s: %w", name, err)
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		err = fmt.Errorf("posixshm: size object %s: %w", name, err)
		return nil, errors.Join(err, closeFD(fd, name), unlink(name))
	}
	data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		err = fmt.Errorf("posixshm: map object %s: %w", name, err)
		return nil, errors.Join(err, closeFD(fd, name), unlink(name))
	}
	if err := closeFD(fd, name); err != nil {
		return nil, errors.Join(err, unix.Munmap(data), unlink(name))
	}
	return &Object{name: name, data: data}, nil
}

// Name returns the object's leading-slash POSIX name.
func (o *Object) Name() string { return o.name }

// Data returns the writable shared mapping. It is nil after Close.
func (o *Object) Data() []byte { return o.data }

// Close releases the object's mapping. It does not unlink the object. Close
// is idempotent.
func (o *Object) Close() error {
	if o.data == nil {
		return nil
	}
	data := o.data
	o.data = nil
	if err := unix.Munmap(data); err != nil {
		return fmt.Errorf("posixshm: unmap object %s: %w", o.name, err)
	}
	return nil
}

// Unlink removes name from the POSIX shared-memory namespace. It returns nil
// when the object was already removed, including when a terminal consumed it.
func Unlink(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := loadLibSystem(); err != nil {
		return err
	}
	return unlink(name)
}

func loadLibSystem() error {
	loadOnce.Do(func() {
		handle, err := purego.Dlopen(libSystem, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			loadErr = fmt.Errorf("posixshm: load libSystem: %w", err)
			return
		}
		shmOpenSym, err = purego.Dlsym(handle, "shm_open")
		if err != nil {
			loadErr = errors.Join(fmt.Errorf("posixshm: resolve shm_open: %w", err), purego.Dlclose(handle))
			return
		}
		shmUnlinkSym, err = purego.Dlsym(handle, "shm_unlink")
		if err != nil {
			loadErr = errors.Join(fmt.Errorf("posixshm: resolve shm_unlink: %w", err), purego.Dlclose(handle))
		}
	})
	return loadErr
}

func shmOpen(name string, flags int, mode uint32) (int, error) {
	cname := append([]byte(name), 0)
	r1, _, errno := purego.SyscallN(
		shmOpenSym,
		uintptr(unsafe.Pointer(&cname[0])),
		uintptr(flags),
		uintptr(mode),
	)
	runtime.KeepAlive(cname)
	if int32(r1) == -1 {
		return -1, syscall.Errno(errno)
	}
	return int(int32(r1)), nil
}

func unlink(name string) error {
	cname := append([]byte(name), 0)
	r1, _, errno := purego.SyscallN(shmUnlinkSym, uintptr(unsafe.Pointer(&cname[0])))
	runtime.KeepAlive(cname)
	if int32(r1) != -1 {
		return nil
	}
	err := syscall.Errno(errno)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return fmt.Errorf("posixshm: unlink object %s: %w", name, err)
}

func closeFD(fd int, name string) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("posixshm: close object %s: %w", name, err)
	}
	return nil
}
