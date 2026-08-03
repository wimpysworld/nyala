package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const profileLockName = ".nyala.lock"

type profileGuard struct {
	file *os.File
}

func (e *ChromiumEngine) acquireProfileGuard() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.profileDir == "" || e.profileGuard != nil {
		return nil
	}
	if err := os.MkdirAll(e.profileDir, 0o700); err != nil {
		return fmt.Errorf("create profile directory %q: %w", e.profileDir, err)
	}
	lockPath := filepath.Join(e.profileDir, profileLockName)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open profile guard %q: %w", lockPath, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return errors.Join(
				fmt.Errorf("profile directory %q is already in use; use --private to start without it", e.profileDir),
				closeErr,
			)
		}
		return errors.Join(fmt.Errorf("lock profile directory %q: %w", e.profileDir, err), closeErr)
	}
	e.profileGuard = &profileGuard{file: file}
	return nil
}

func (e *ChromiumEngine) releaseProfileGuard() error {
	e.mu.Lock()
	guard := e.profileGuard
	e.profileGuard = nil
	e.mu.Unlock()
	if guard == nil {
		return nil
	}
	unlockErr := unix.Flock(int(guard.file.Fd()), unix.LOCK_UN)
	closeErr := guard.file.Close()
	return errors.Join(unlockErr, closeErr)
}
