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

// purgeSessionState removes Chromium's tab-restore state beneath profileDir.
// Nyala drives every navigation itself, so a restored session is never wanted,
// and an aborted run leaves Session and Tabs files that grow with each
// navigation. Headless Chromium reloads that state at launch and respawns the
// recorded tabs, which can wedge the CDP handshake before the first milestone
// prints. launch calls this after acquireProfileGuard and before the process
// spawns, so no live Chromium owns the files. An empty profileDir (a private
// engine) and a missing directory are both no-ops.
func purgeSessionState(profileDir string) error {
	if profileDir == "" {
		return nil
	}
	dir := filepath.Join(profileDir, "Default", "Sessions")
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("purge session state %q: %w", dir, err)
	}
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
