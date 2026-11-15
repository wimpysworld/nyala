package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const profileLockName = ".nyala.lock"

// sharedLockPoll is the interval between LOCK_SH|LOCK_NB attempts in
// acquireShared.
const sharedLockPoll = 50 * time.Millisecond

// profileGuard is the open lock file beneath a persistent profile. One fd is
// held for the engine lifetime, including recovery rebuilds, so lock mode
// conversions happen atomically on the same descriptor. mode is 0 when no
// lock is held, lockExclusive while this engine launches Chromium, and
// lockShared while it is connected as one of the profile's clients.
type profileGuard struct {
	file *os.File
	mode int
}

// openProfileGuard creates the profile directory and opens the lock file
// without taking a lock. It is idempotent: a guard already held by the engine
// is kept, so rebuilds reuse the same fd. A private engine (empty profileDir)
// is a no-op.
func (e *ChromiumEngine) openProfileGuard() error {
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
	e.profileGuard = &profileGuard{file: file}
	return nil
}

// tryExclusive attempts LOCK_EX|LOCK_NB. It returns false when another
// process holds the lock in any mode, and true when this engine now holds the
// exclusive lock and is the launcher or the last client.
func (g *profileGuard) tryExclusive() (bool, error) {
	err := tryLockFile(g.file, lockExclusive)
	if isLockBusy(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock profile guard exclusively: %w", err)
	}
	g.mode = lockExclusive
	return true, nil
}

// acquireShared polls LOCK_SH|LOCK_NB until it succeeds or ctx ends. It
// waits while a launcher holds LOCK_EX. A launcher that dies before it
// downgrades releases its lock when the kernel closes its fd, so the poll
// succeeds and the caller can retry the launch itself.
func (g *profileGuard) acquireShared(ctx context.Context) error {
	for {
		err := tryLockFile(g.file, lockShared)
		if err == nil {
			g.mode = lockShared
			return nil
		}
		if !isLockBusy(err) {
			return fmt.Errorf("lock profile guard shared: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for shared profile guard: %w", ctx.Err())
		case <-time.After(sharedLockPoll):
		}
	}
}

// downgrade converts the held LOCK_EX to LOCK_SH on the same fd. flock
// converts atomically, so no other process can take the exclusive lock in
// between.
func (g *profileGuard) downgrade() error {
	if err := downgradeLockFile(g.file); err != nil {
		return fmt.Errorf("downgrade profile guard: %w", err)
	}
	g.mode = lockShared
	return nil
}

// unlock releases the held lock and keeps the fd open for a later attempt.
func (g *profileGuard) unlock() error {
	if err := unlockFile(g.file); err != nil {
		return fmt.Errorf("unlock profile guard: %w", err)
	}
	g.mode = 0
	return nil
}

// writePID replaces the lock file content with the decimal Chromium pid. Only
// the LOCK_EX holder writes, so clients never race on the content. The last
// client out reads the pid to stop the browser it did not spawn.
func (g *profileGuard) writePID(pid int) error {
	// Write before truncating, so a launcher killed midway never leaves the
	// file empty. Stale trailing digits from a longer previous pid are gated by
	// browserOwnsProfile like any other pid.
	text := strconv.Itoa(pid)
	if _, err := g.file.WriteAt([]byte(text), 0); err != nil {
		return fmt.Errorf("write profile guard pid: %w", err)
	}
	if err := g.file.Truncate(int64(len(text))); err != nil {
		return fmt.Errorf("truncate profile guard: %w", err)
	}
	return nil
}

// readPID returns the Chromium pid stored by writePID. It returns false when
// the file is empty or does not hold a positive decimal integer.
func (g *profileGuard) readPID() (int, bool) {
	if _, err := g.file.Seek(0, io.SeekStart); err != nil {
		return 0, false
	}
	data, err := io.ReadAll(g.file)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// purgeSessionState removes Chromium's tab-restore state beneath profileDir.
// Nyala drives every navigation itself, so a restored session is never wanted,
// and an aborted run leaves Session and Tabs files that grow with each
// navigation. Headless Chromium reloads that state at launch and respawns the
// recorded tabs, which can wedge the CDP handshake before the first milestone
// prints. launch calls this while it holds the exclusive guard and before the
// process spawns, so no live Chromium owns the files. An empty profileDir (a
// private engine) and a missing directory are both no-ops.
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

// releaseProfileGuard is the single LOCK_UN plus close path. Close calls it
// last, after the session is torn down.
func (e *ChromiumEngine) releaseProfileGuard() error {
	e.mu.Lock()
	guard := e.profileGuard
	e.profileGuard = nil
	e.mu.Unlock()
	if guard == nil {
		return nil
	}
	unlockErr := unlockFile(guard.file)
	closeErr := guard.file.Close()
	return errors.Join(unlockErr, closeErr)
}
