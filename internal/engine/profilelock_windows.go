//go:build windows

package engine

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Lock modes held in profileGuard.mode.
const (
	lockShared    = 1
	lockExclusive = 2
)

// lockRangeHigh places the one locked byte far past the pid text. Windows
// byte-range locks are mandatory, so a lock over the text would stop readPID
// in another process.
const lockRangeHigh = 0x7fffffff

func lockRange() *windows.Overlapped {
	return &windows.Overlapped{OffsetHigh: lockRangeHigh}
}

// tryLockFile takes mode on f without waiting. isLockBusy reports the error
// when another handle holds a conflicting lock.
func tryLockFile(f *os.File, mode int) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if mode == lockExclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, lockRange())
}

// isLockBusy reports whether err from tryLockFile means another holder.
func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// downgradeLockFile converts the exclusive lock on f to a shared lock. The
// handle that holds the exclusive lock may add a shared lock on the same
// range, and the next unlock drops the exclusive lock first, so no other
// process can take the exclusive lock in between.
func downgradeLockFile(f *os.File) error {
	if err := tryLockFile(f, lockShared); err != nil {
		return err
	}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRange())
}

// unlockFile releases the lock on f. Like flock, it succeeds when f holds no
// lock.
func unlockFile(f *os.File) error {
	err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRange())
	if errors.Is(err, windows.ERROR_NOT_LOCKED) {
		return nil
	}
	return err
}
