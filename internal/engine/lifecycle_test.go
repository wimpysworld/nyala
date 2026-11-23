package engine

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

func TestCloseAfterNavigateFailureReturns(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")
	hideInstalledBrowsers(t)
	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate("https://example.invalid"); err == nil {
		t.Fatal("Navigate succeeded with no browser on PATH")
	}
	done := make(chan struct{})
	go func() { _ = e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked after failed Navigate")
	}
}

func TestCloseBeforeNavigatePreventsLateStartup(t *testing.T) {
	e := New("missing-browser")
	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := e.Navigate("https://example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Navigate error = %v, want context.Canceled", err)
	}
}

func TestCloseDuringStartupReturns(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { _ = e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked before the supervisor started")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Close did not cancel the startup context")
	}
}

func TestFinishStartupAfterCloseCleansLifecycle(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	e.closeOnce.Do(func() { close(e.closing) })
	if err := e.finishStartup(); !errors.Is(err, context.Canceled) {
		t.Fatalf("finishStartup error = %v, want context.Canceled", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("finishStartup did not cancel the startup context")
	}
	select {
	case _, ok := <-e.frames:
		if ok {
			t.Fatal("frames channel is open")
		}
	default:
		t.Fatal("frames channel was not closed")
	}
	select {
	case <-e.supDone:
	default:
		t.Fatal("supDone channel was not closed")
	}
}

func TestSuperviseWithNilContextReturns(t *testing.T) {
	e := New("")
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	go e.supervise(make(chan struct{}))
	select {
	case <-e.supDone:
	case <-time.After(time.Second):
		t.Fatal("supervise blocked with nil context")
	}
}

func TestSuperviseCloseWinsOverRebuild(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")
	browserPath := filepath.Join(t.TempDir(), "fake-browser")
	if err := os.WriteFile(browserPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake browser: %v", err)
	}
	e := New(browserPath)
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	crashed := make(chan struct{})
	close(crashed)
	e.closeOnce.Do(func() { close(e.closing) })
	go e.supervise(crashed)
	select {
	case <-e.supDone:
	case <-time.After(time.Second):
		t.Fatal("supervise blocked instead of tearing down on close")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("supervise did not tear down the session on close")
	}
	if e.ctx != nil {
		t.Fatal("supervise rebuilt a session during shutdown")
	}
}

func TestSuperviseCloseDuringTeardownSkipsRelaunch(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(t.Context())
	e.ctx, e.cancel = ctx, cancel
	e.frames = make(chan render.Frame)
	e.supDone = make(chan struct{})
	e.supervised = true
	teardownStarted := make(chan struct{})
	e.closeBrowserOp = func(ctx context.Context) error {
		close(teardownStarted)
		select {
		case <-e.closing:
			cancel()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	relaunched := false
	e.startSessionOp = func() (chan struct{}, error) {
		relaunched = true
		return nil, errors.New("unexpected relaunch")
	}
	crashed := make(chan struct{})
	close(crashed)
	go e.supervise(crashed)
	select {
	case <-teardownStarted:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not start teardown")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return")
	}
	if relaunched {
		t.Fatal("supervisor relaunched after Close signalled during teardown")
	}
	if err := e.StreamError(); err != nil {
		t.Errorf("StreamError after Close = %v, want nil", err)
	}
}

// TestCloseGracefullyClosesLiveSession asserts that Close runs the graceful
// closeBrowserOp with no engine lock held and skips raw cancellation when the
// op succeeds. On a shared profile the op is sharedCloseOp: close this
// instance's own target, then terminate the browser only as the last client.
func TestCloseGracefullyClosesLiveSession(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	gracefulCalled := false
	rawCancelled := false
	engineUnlocked := false
	screencastUnlocked := false
	e.ctx = ctx
	e.closeBrowserOp = func(context.Context) error {
		engineUnlocked = e.mu.TryLock()
		if engineUnlocked {
			e.mu.Unlock()
		}
		screencastUnlocked = e.screencastCtl.TryLock()
		if screencastUnlocked {
			e.screencastCtl.Unlock()
		}
		gracefulCalled = true
		cancel()
		return nil
	}
	e.cancel = func() {
		rawCancelled = true
		cancel()
	}
	e.frames = make(chan render.Frame)
	e.closing, e.supDone = make(chan struct{}), make(chan struct{})
	e.supervised = true
	go e.supervise(make(chan struct{}))
	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if !gracefulCalled {
		t.Fatal("Close did not request graceful browser shutdown")
	}
	if rawCancelled {
		t.Fatal("Close selected raw cancellation after a successful graceful close")
	}
	if !engineUnlocked || !screencastUnlocked {
		t.Fatal("graceful close ran while an engine lock was held")
	}
}

// TestTeardownSessionDeadlineFallsBackToRawCancellation asserts that a
// closeBrowserOp that never returns (a close-own-target, then terminate the
// browser when last, op stuck on a wedged browser) is cut off at closeTimeout
// and raw cancellation runs instead.
func TestTeardownSessionDeadlineFallsBackToRawCancellation(t *testing.T) {
	if gracefulCloseTimeout != 500*time.Millisecond {
		t.Fatalf("gracefulCloseTimeout = %v, want 500ms", gracefulCloseTimeout)
	}
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	closeStarted := make(chan struct{})
	rawCancelled := make(chan struct{})
	releaseClose := make(chan struct{})
	e.ctx = ctx
	e.closeTimeout = 20 * time.Millisecond
	e.closeBrowserOp = func(context.Context) error {
		close(closeStarted)
		<-releaseClose
		return errors.New("close remained blocked")
	}
	e.cancel = func() {
		cancel()
		close(rawCancelled)
		close(releaseClose)
	}
	done := make(chan struct{})
	go func() {
		e.teardownSession()
		close(done)
	}()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("graceful close did not start")
	}
	select {
	case <-rawCancelled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("deadline did not trigger raw cancellation")
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("teardown did not return after raw cancellation")
	}
}

func TestTeardownCancelledSessionSkipsGracefulClose(t *testing.T) {
	e := New("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gracefulCalled := false
	rawCancelled := false
	e.ctx = ctx
	e.closeBrowserOp = func(context.Context) error {
		gracefulCalled = true
		return nil
	}
	e.cancel = func() { rawCancelled = true }
	e.teardownSession()
	if gracefulCalled {
		t.Fatal("cancelled session attempted graceful browser shutdown")
	}
	if !rawCancelled {
		t.Fatal("cancelled session did not use raw cancellation")
	}
}

func TestRebuildContinuesAfterBoundedTeardown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gracefulClose func(context.Context, context.CancelFunc) error
		wantFallback  bool
	}{
		{
			name: "graceful close",
			gracefulClose: func(_ context.Context, cancel context.CancelFunc) error {
				cancel()
				return nil
			},
		},
		{
			name: "deadline fallback",
			gracefulClose: func(ctx context.Context, _ context.CancelFunc) error {
				<-ctx.Done()
				return ctx.Err()
			},
			wantFallback: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New("")
			oldCtx, cancelOld := context.WithCancel(context.Background())
			replacementCtx, cancelReplacement := context.WithCancel(context.Background())
			t.Cleanup(cancelReplacement)
			fallback := make(chan struct{})
			e.ctx = oldCtx
			e.closeTimeout = 20 * time.Millisecond
			e.closeBrowserOp = func(ctx context.Context) error {
				return tc.gracefulClose(ctx, cancelOld)
			}
			e.cancel = func() {
				cancelOld()
				close(fallback)
			}

			replacementStarted := make(chan struct{})
			replacementCrashed := make(chan struct{})
			var replacementErr error
			e.startSessionOp = func() (chan struct{}, error) {
				defer close(replacementStarted)
				select {
				case <-oldCtx.Done():
				default:
					replacementErr = errors.New("replacement started before the old session was released")
					return nil, replacementErr
				}
				e.mu.Lock()
				oldSessionCleared := e.ctx == nil && e.closeBrowserOp == nil && e.cancel == nil
				e.mu.Unlock()
				if !oldSessionCleared {
					replacementErr = errors.New("replacement started while old session fields remained")
					return nil, replacementErr
				}
				if tc.wantFallback {
					select {
					case <-fallback:
					default:
						replacementErr = errors.New("replacement started before bounded fallback")
						return nil, replacementErr
					}
				} else {
					select {
					case <-fallback:
						replacementErr = errors.New("successful graceful close selected raw fallback")
						return nil, replacementErr
					default:
					}
				}
				e.mu.Lock()
				e.ctx = replacementCtx
				e.cancel = cancelReplacement
				e.mu.Unlock()
				return replacementCrashed, nil
			}

			rebuildDone := make(chan bool, 1)
			go func() {
				crashed, err := e.rebuild()
				rebuildDone <- err == nil && crashed == replacementCrashed
			}()
			select {
			case <-replacementStarted:
			case <-time.After(time.Second):
				t.Fatal("rebuild did not continue to the replacement session")
			}
			if replacementErr != nil {
				t.Fatal(replacementErr)
			}
			select {
			case ok := <-rebuildDone:
				if !ok {
					t.Fatal("rebuild did not return the replacement session")
				}
			case <-time.After(time.Second):
				t.Fatal("rebuild did not return")
			}
			e.mu.Lock()
			gotCtx := e.ctx
			e.mu.Unlock()
			if gotCtx != replacementCtx {
				t.Fatal("rebuild did not install the replacement session")
			}
		})
	}
}

func TestRecoveryProfileGuardHeldUntilClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rebuild recorder is a POSIX shell script")
	}
	clearProxyEnv(t, nil)
	profileDir := filepath.Join(t.TempDir(), "profile")
	e := New("")
	other := New("")
	for _, engine := range []*ChromiumEngine{e, other} {
		if err := engine.SetProfileDir(profileDir); err != nil {
			t.Fatalf("SetProfileDir(%q): %v", profileDir, err)
		}
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Errorf("other Close: %v", err)
		}
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := e.openProfileGuard(); err != nil {
		t.Fatalf("initial openProfileGuard: %v", err)
	}
	guard := e.profileGuard
	fd := guard.file.Fd()
	// A connected client holds LOCK_SH, so a rebuild starts from that mode.
	if err := guard.acquireShared(context.Background()); err != nil {
		t.Fatalf("initial acquireShared: %v", err)
	}

	argvFile := filepath.Join(t.TempDir(), "argv")
	t.Setenv("NYALA_TEST_ARGV_FILE", argvFile)
	_, browserPath := fakeExecutable(t, "chromium",
		"printf '%s\\n' \"$@\" > \"$NYALA_TEST_ARGV_FILE\"\nexit 1\n")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", browserPath)
	t.Setenv("CHROME_PATH", "")
	alternateProfile := filepath.Join(t.TempDir(), "alternate-profile")
	t.Setenv("NYALA_PROFILE", alternateProfile)
	// The recorder exits without writing DevToolsActivePort, so each launch
	// attempt would otherwise wait the full production budget.
	savedWait := devToolsWaitTimeout
	devToolsWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { devToolsWaitTimeout = savedWait })

	if _, err := e.rebuild(); err == nil {
		t.Fatal("rebuild with recording browser succeeded, want its start failure")
	}
	if e.profileGuard != guard || e.profileGuard.file.Fd() != fd {
		t.Fatal("rebuild replaced the held profile guard descriptor")
	}
	if e.profileDir != profileDir {
		t.Errorf("profileDir after rebuild = %q, want held path %q", e.profileDir, profileDir)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read rebuild argv: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if want := "--user-data-dir=" + profileDir; !slices.Contains(argv, want) {
		t.Errorf("rebuild argv %v is missing held profile argument %q", argv, want)
	}
	if _, err := os.Stat(alternateProfile); !os.IsNotExist(err) {
		t.Fatalf("alternate profile Stat error = %v, want path absent", err)
	}

	// Every failed attempt unlocked the guard. Hold LOCK_SH again as a
	// connected client would, then prove a second engine joins while this one
	// still holds: shared succeeds at once, exclusive is refused.
	if err := guard.acquireShared(context.Background()); err != nil {
		t.Fatalf("acquireShared after rebuild: %v", err)
	}
	if err := other.openProfileGuard(); err != nil {
		t.Fatalf("other openProfileGuard: %v", err)
	}
	if exclusive, err := other.profileGuard.tryExclusive(); err != nil || exclusive {
		t.Fatalf("other tryExclusive while owner holds shared = (%t, %v), want (false, nil)", exclusive, err)
	}
	sharedCtx, cancelShared := context.WithTimeout(context.Background(), time.Second)
	defer cancelShared()
	if err := other.profileGuard.acquireShared(sharedCtx); err != nil {
		t.Fatalf("other acquireShared while owner holds shared: %v", err)
	}
	if e.profileGuard != guard {
		t.Fatal("owner guard changed while another client joined")
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if e.profileGuard != nil {
		t.Fatal("Close retained the profile guard")
	}
	if exclusive, err := other.profileGuard.tryExclusive(); err != nil || !exclusive {
		t.Fatalf("other tryExclusive after owner Close = (%t, %v), want (true, nil)", exclusive, err)
	}
}

// newTestGuard opens a lock file beneath a fresh temp directory, holds LOCK_SH
// on it as a connected client does, and stores pid as the launcher would. The
// file closes in t.Cleanup.
func newTestGuard(t *testing.T, pid int) (*profileGuard, string) {
	t.Helper()
	lockPath := filepath.Join(t.TempDir(), profileLockName)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	guard := &profileGuard{file: file}
	if err := guard.acquireShared(context.Background()); err != nil {
		t.Fatalf("acquireShared: %v", err)
	}
	if err := guard.writePID(pid); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	return guard, lockPath
}

// closeOpRecorder records the order in which sharedCloseOp calls its three
// funcs and the pid it passes to terminate.
type closeOpRecorder struct {
	calls []string
	pids  []int
}

func (r *closeOpRecorder) closeTarget(context.Context) error {
	r.calls = append(r.calls, "closeTarget")
	return nil
}

func (r *closeOpRecorder) terminate(_ context.Context, pid int) error {
	r.calls = append(r.calls, "terminate")
	r.pids = append(r.pids, pid)
	return nil
}

func (r *closeOpRecorder) closeSocket() {
	r.calls = append(r.calls, "closeSocket")
}

// TestLastClientTerminatesBrowser asserts the last-out path of sharedCloseOp:
// with no other lock holder, LOCK_EX succeeds, so the op closes its own
// target, closes the websocket, then terminates the pid from the lock file.
// The socket closes before the terminate so chromedp never writes to a socket
// the dying browser reset.
func TestLastClientTerminatesBrowser(t *testing.T) {
	const pid = 424242
	guard, _ := newTestGuard(t, pid)
	rec := &closeOpRecorder{}
	op := sharedCloseOp(guard, rec.closeTarget, rec.terminate, rec.closeSocket)
	if err := op(context.Background()); err != nil {
		t.Fatalf("sharedCloseOp: %v", err)
	}
	want := []string{"closeTarget", "closeSocket", "terminate"}
	if !slices.Equal(rec.calls, want) {
		t.Fatalf("call order = %v, want %v", rec.calls, want)
	}
	if !slices.Equal(rec.pids, []int{pid}) {
		t.Fatalf("terminate pids = %v, want [%d]", rec.pids, pid)
	}
	if guard.mode != lockExclusive {
		t.Fatalf("guard mode after last-client close = %d, want LOCK_EX (%d)", guard.mode, lockExclusive)
	}
}

// TestNonLastClientClosesSocketOnly asserts that sharedCloseOp leaves the
// browser alive while another client holds LOCK_SH on its own descriptor: the
// op closes its own target and the websocket, and never calls terminate.
func TestNonLastClientClosesSocketOnly(t *testing.T) {
	const pid = 424242
	guard, lockPath := newTestGuard(t, pid)
	// flock locks are shared within a process by open file description, so
	// the other client must hold its lock through a second os.OpenFile.
	otherFile, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open second descriptor: %v", err)
	}
	t.Cleanup(func() { _ = otherFile.Close() })
	other := &profileGuard{file: otherFile}
	if err := other.acquireShared(context.Background()); err != nil {
		t.Fatalf("other acquireShared: %v", err)
	}

	rec := &closeOpRecorder{}
	op := sharedCloseOp(guard, rec.closeTarget, rec.terminate, rec.closeSocket)
	if err := op(context.Background()); err != nil {
		t.Fatalf("sharedCloseOp: %v", err)
	}
	want := []string{"closeTarget", "closeSocket"}
	if !slices.Equal(rec.calls, want) {
		t.Fatalf("call order = %v, want %v", rec.calls, want)
	}
	if len(rec.pids) != 0 {
		t.Fatalf("terminate ran with pids %v while another client held the guard", rec.pids)
	}
	if guard.mode != lockShared {
		t.Fatalf("guard mode after non-last close = %d, want LOCK_SH (%d)", guard.mode, lockShared)
	}
}

// TestSharedCloseOpHonoursDeadline asserts that a terminate that never
// observes the browser exit returns when the caller's deadline ends, with that
// deadline error, so teardownSession can fall back to raw cancellation.
func TestSharedCloseOpHonoursDeadline(t *testing.T) {
	guard, _ := newTestGuard(t, 424242)
	rec := &closeOpRecorder{}
	blockingTerminate := func(ctx context.Context, pid int) error {
		<-ctx.Done()
		return fmt.Errorf("terminate browser %d: still alive: %w", pid, ctx.Err())
	}
	op := sharedCloseOp(guard, rec.closeTarget, blockingTerminate, rec.closeSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := op(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sharedCloseOp error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("sharedCloseOp returned after %v, want within 500ms of a 50ms deadline", elapsed)
	}
	if !slices.Equal(rec.calls, []string{"closeTarget", "closeSocket"}) {
		t.Fatalf("calls before terminate = %v, want closeTarget then closeSocket", rec.calls)
	}
}

func TestComposeFrameNilTextKeepsBitmap(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	frame := New("").composeFrame(pendingFrame{img: img}, nil, nil)
	if frame.Img != img {
		t.Fatal("composeFrame dropped the bitmap")
	}
	if len(frame.Text) != 0 {
		t.Fatalf("composeFrame text = %v, want bitmap-only frame", frame.Text)
	}
}
