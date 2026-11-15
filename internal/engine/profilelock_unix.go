//go:build !windows

package engine

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Lock modes held in profileGuard.mode.
const (
	lockShared    = unix.LOCK_SH
	lockExclusive = unix.LOCK_EX
)

// tryLockFile takes mode on f without waiting. isLockBusy reports the error
// when another descriptor holds a conflicting lock.
func tryLockFile(f *os.File, mode int) error {
	return unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
}

// isLockBusy reports whether err from tryLockFile means another holder.
func isLockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK)
}

// downgradeLockFile converts the exclusive lock on f to a shared lock.
func downgradeLockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_SH)
}

// unlockFile releases the lock on f.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
