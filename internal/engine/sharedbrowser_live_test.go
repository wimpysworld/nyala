//go:build live

package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"golang.org/x/sys/unix"
)

// TestLiveSharedBrowserFacts measures the Chromium and chromedp behaviours that
// the shared-browser design (WW-259) depends on. Each fact runs in its own
// subtest and logs a line that starts with "FACT (x):" so the result can be
// copied into docs/ARCHITECTURE.md. The facts, as measured on Chromium 152:
//
//   - (a) two newWindow=true targets have distinct window ids, and
//     Browser.setContentsSize on one window leaves the other window's bounds
//     and CSS viewport unchanged.
//   - (c) chromedp's own first page target lives in a window distinct from a
//     newWindow=true target.
//   - (b1) headless Chromium stays alive after every page target closes. A
//     built-in extension background_page target keeps it alive, so the
//     launcher cannot rely on last-page exit.
//   - (b2) Browser.close is unreachable from a remote chromedp context, so the
//     last client ends the browser with SIGTERM to the pid the launcher stored
//     in the lock file. terminateBrowser must observe the exit within 5 s.
//   - (d) whether DevToolsActivePort remains after that SIGTERM exit.
//
// The shared_two_engines subtest then drives the production path: two engines
// on one profile, one Chromium, and the last one out terminates it.
//
// Run it with `just live`. It skips when no Chromium resolves.
func TestLiveSharedBrowserFacts(t *testing.T) {
	execPath, err := resolveExecutable("")
	if err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	t.Run("a_window_isolation", func(t *testing.T) { liveFactWindowIsolation(t, execPath) })
	t.Run("c_first_target_window", func(t *testing.T) { liveFactFirstTargetWindow(t, execPath) })
	t.Run("b1_survives_last_page_close", func(t *testing.T) { liveFactSurvivesLastPageClose(t, execPath) })
	t.Run("b2_d_sigterm_ends_process", func(t *testing.T) { liveFactSigtermEndsProcess(t, execPath) })
	t.Run("shared_two_engines", func(t *testing.T) { liveSharedTwoEngines(t) })
	t.Run("shared_browser_killed_both_recover", func(t *testing.T) { liveSharedBrowserKilledBothRecover(t) })
}

// liveAllocator launches Chromium through newAllocator with the production
// flag set and returns the first chromedp context, whose browser is started.
func liveAllocator(t *testing.T, execPath string) context.Context {
	t.Helper()
	cfg := allocatorConfig{
		execPath:      execPath,
		surfaceWidth:  640,
		surfaceHeight: 384,
		scale:         1,
	}
	parent, cancelParent := context.WithTimeout(context.Background(), 60*time.Second)
	allocCtx, cancelAlloc := newAllocator(parent, cfg)
	ctx, cancel := chromedp.NewContext(allocCtx)
	t.Cleanup(func() {
		cancel()
		cancelAlloc()
		cancelParent()
	})
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	return ctx
}

// newWindowTarget creates an about:blank page target in a new window and
// returns its attached context and window id.
func newWindowTarget(t *testing.T, ctx context.Context) (context.Context, browser.WindowID) {
	t.Helper()
	browserCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
	id, err := target.CreateTarget("about:blank").WithNewWindow(true).Do(browserCtx)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	targetCtx, cancel := chromedp.NewContext(ctx, chromedp.WithTargetID(id))
	t.Cleanup(cancel)
	if err := chromedp.Run(targetCtx); err != nil {
		t.Fatalf("attach target: %v", err)
	}
	win, _, err := browser.GetWindowForTarget().Do(cdp.WithExecutor(targetCtx, chromedp.FromContext(targetCtx).Target))
	if err != nil {
		t.Fatalf("window for target: %v", err)
	}
	return targetCtx, win
}

func innerWidth(t *testing.T, ctx context.Context) int {
	t.Helper()
	var w int
	if err := chromedp.Run(ctx, chromedp.Evaluate("window.innerWidth", &w)); err != nil {
		t.Fatalf("evaluate innerWidth: %v", err)
	}
	return w
}

func liveFactWindowIsolation(t *testing.T, execPath string) {
	ctx := liveAllocator(t, execPath)
	browserCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)

	_, win1 := newWindowTarget(t, ctx)
	target2, win2 := newWindowTarget(t, ctx)
	if win1 == win2 {
		t.Fatalf("FACT (a): false, two newWindow=true targets share window id %d", win1)
	}

	before, err := browser.GetWindowBounds(win2).Do(browserCtx)
	if err != nil {
		t.Fatalf("bounds before: %v", err)
	}
	widthBefore := innerWidth(t, target2)

	if err := browser.SetContentsSize(win1).WithWidth(320).WithHeight(200).Do(browserCtx); err != nil {
		t.Fatalf("set contents size on win1: %v", err)
	}
	bounds1, err := browser.GetWindowBounds(win1).Do(browserCtx)
	if err != nil {
		t.Fatalf("bounds win1: %v", err)
	}

	after, err := browser.GetWindowBounds(win2).Do(browserCtx)
	if err != nil {
		t.Fatalf("bounds after: %v", err)
	}
	widthAfter := innerWidth(t, target2)

	ok := *before == *after && widthBefore == widthAfter
	t.Logf("FACT (a): %t, win1=%d win2=%d, win1 bounds after setContentsSize(320x200)=%+v, win2 bounds before=%+v after=%+v, win2 innerWidth before=%d after=%d",
		ok, win1, win2, *bounds1, *before, *after, widthBefore, widthAfter)
	if !ok {
		t.Fatal("FACT (a) is false: setContentsSize on win1 changed win2")
	}
}

func liveFactFirstTargetWindow(t *testing.T, execPath string) {
	ctx := liveAllocator(t, execPath)
	firstWin, _, err := browser.GetWindowForTarget().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target))
	if err != nil {
		t.Fatalf("window for first target: %v", err)
	}
	_, newWin := newWindowTarget(t, ctx)
	distinct := firstWin != newWin
	t.Logf("FACT (c): %t, chromedp first target window=%d, newWindow=true target window=%d", distinct, firstWin, newWin)
	if !distinct {
		t.Fatal("FACT (c) is false: the first target shares a window with a newWindow=true target")
	}
}

// spawnChromium starts a headless Chromium with a fresh user data dir and
// returns the command and that directory. The process is killed in t.Cleanup
// when it is still running.
func spawnChromium(t *testing.T, execPath string, extra ...string) (*exec.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	args := []string{
		"--headless",
		"--no-first-run",
		"--no-default-browser-check",
		"--remote-debugging-port=0",
		"--user-data-dir=" + dir,
	}
	args = append(args, extra...)
	args = append(args, "about:blank")
	cmd := exec.Command(execPath, args...) //nolint:noctx // the test owns the lifetime and kills it in t.Cleanup
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chromium: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	return cmd, dir
}

// livePortURL polls DevToolsActivePort under profileDir for up to 15 s through
// the production waiter and returns the browser websocket URL.
func livePortURL(t *testing.T, profileDir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url, err := waitDevToolsActivePort(ctx, profileDir)
	if err != nil {
		t.Fatalf("DevToolsActivePort not written within 15 s under %s: %v", profileDir, err)
	}
	return url
}

// connectRemote attaches to a spawned Chromium and returns the first chromedp
// context with its browser connection started.
func connectRemote(t *testing.T, url string) (context.Context, func()) {
	t.Helper()
	parent, cancelParent := context.WithTimeout(context.Background(), 60*time.Second)
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(parent, url, chromedp.NoModifyURL)
	ctx, cancel := chromedp.NewContext(allocCtx)
	disconnect := func() {
		cancel()
		cancelAlloc()
		cancelParent()
	}
	t.Cleanup(disconnect)
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("connect remote %s: %v", url, err)
	}
	return ctx, disconnect
}

// closeAllPages closes every page target and returns how many it closed.
func closeAllPages(t *testing.T, ctx context.Context) int {
	t.Helper()
	browserCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
	infos, err := target.GetTargets().Do(browserCtx)
	if err != nil {
		t.Fatalf("get targets: %v", err)
	}
	n := 0
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		if err := target.CloseTarget(info.TargetID).Do(browserCtx); err != nil {
			t.Logf("close target %s: %v", info.TargetID, err)
			continue
		}
		n++
	}
	return n
}

// remainingTargetTypes lists the type and URL of every target still open, so
// the log names what keeps the process alive after the last page closed.
func remainingTargetTypes(ctx context.Context) []string {
	infos, err := target.GetTargets().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
	if err != nil {
		return []string{"getTargets error: " + err.Error()}
	}
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Type+" "+info.URL)
	}
	return out
}

// waitExit reports whether the process exits within d.
func waitExit(cmd *exec.Cmd, d time.Duration) (bool, error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return true, err
	case <-time.After(d):
		return false, nil
	}
}

// liveFactSurvivesLastPageClose measures (b1): headless Chromium does not exit
// when its last page target closes, because a built-in extension
// background_page target keeps it alive. The design therefore never waits for
// a natural exit and the last client terminates the process itself.
func liveFactSurvivesLastPageClose(t *testing.T, execPath string) {
	cmd, dir := spawnChromium(t, execPath)
	ctx, _ := connectRemote(t, livePortURL(t, dir))
	closed := closeAllPages(t, ctx)
	exited, err := waitExit(cmd, 3*time.Second)
	remaining := remainingTargetTypes(ctx)
	t.Logf("FACT (b1): %t, headless Chromium stayed alive 3 s after closing %d page target(s); remaining targets: %s (wait err: %v)",
		!exited, closed, strings.Join(remaining, "; "), err)
	if exited {
		t.Fatal("FACT (b1) is false: the process exited after its last page closed, so the measured keep-alive behaviour changed")
	}
}

// liveFactSigtermEndsProcess measures (b2) and (d) in the production order of
// sharedCloseOp: every page closed, the websocket closed, then SIGTERM through
// terminateBrowser ends the process within 5 s. The browser runs with the
// production sharedArgv flag set. The log records whether DevToolsActivePort
// survives that exit.
func liveFactSigtermEndsProcess(t *testing.T, execPath string) {
	cfg := allocatorConfig{execPath: execPath, surfaceWidth: 640, surfaceHeight: 384, scale: 1}
	dir := t.TempDir()
	cfg.profileDir = dir
	argv := sharedArgv(launchFlags(cfg), dir)
	cmd := exec.Command(execPath, argv...) //nolint:noctx // the test owns the lifetime and kills it in t.Cleanup
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chromium: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	ctx, disconnect := connectRemote(t, livePortURL(t, dir))
	closed := closeAllPages(t, ctx)
	disconnect()
	pid := cmd.Process.Pid
	// Reap the child as spawnShared does, or the exited process stays a
	// zombie that still answers the null signal terminateBrowser polls.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	termCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	termErr := terminateBrowser(termCtx, pid)
	elapsed := time.Since(start)
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-time.After(2 * time.Second):
		waitErr = errors.New("cmd.Wait did not return within 2 s of terminateBrowser")
	}
	t.Logf("FACT (b2): %t, SIGTERM to pid %d after closing %d page target(s) ended the process in %v (terminate err: %v, wait err: %v)",
		termErr == nil, pid, closed, elapsed.Round(time.Millisecond), termErr, waitErr)
	if termErr != nil {
		t.Fatal("FACT (b2) is false: SIGTERM did not end the process within 5 s")
	}

	_, statErr := os.Stat(devToolsActivePortPath(dir))
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat DevToolsActivePort: %v", statErr)
	}
	t.Logf("FACT (d): DevToolsActivePort still exists after the SIGTERM exit: %t", exists)
}

// liveReadFrame reads one frame from e within the deadline and fails the test
// when the channel closes or the deadline passes.
func liveReadFrame(t *testing.T, e *ChromiumEngine, what string) {
	t.Helper()
	select {
	case f, ok := <-e.Frames():
		if !ok {
			t.Fatalf("%s: frames channel closed", what)
		}
		if f.Img == nil {
			t.Fatalf("%s: frame has a nil bitmap", what)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: no frame within 30 s", what)
	}
}

// liveSharedTwoEngines drives the production shared-browser path in one
// process: two engines share one profile and one Chromium, each renders a
// frame, the first to close leaves the browser running for the other, and the
// last to close terminates the pid the launcher stored in the lock file.
func liveSharedTwoEngines(t *testing.T) {
	profileDir := filepath.Join(t.TempDir(), "profile")
	t.Cleanup(func() {
		// Any Chromium still bound to this profile is a leak; kill it so the
		// temp directory removal and later runs are not blocked.
		_ = exec.Command("pkill", "-9", "-f", "--user-data-dir="+profileDir).Run() //nolint:gosec,noctx // profileDir is a t.TempDir path
	})

	engines := make([]*ChromiumEngine, 2)
	for i := range engines {
		e := New("")
		if err := e.SetProfileDir(profileDir); err != nil {
			t.Fatalf("engine %d SetProfileDir: %v", i, err)
		}
		e.SetCellPx(8, 16)
		e.Resize(80, 24)
		if err := e.Navigate(liveTestPage); err != nil {
			t.Fatalf("engine %d navigate: %v", i, err)
		}
		t.Cleanup(func() { _ = e.Close() })
		engines[i] = e
	}
	for i, e := range engines {
		liveReadFrame(t, e, "engine "+strconv.Itoa(i)+" first frame")
	}

	raw, err := os.ReadFile(filepath.Join(profileDir, profileLockName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("lock file pid %q: %v", raw, err)
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("shared browser pid %d is not alive before any close: %v", pid, err)
	}

	if err := engines[0].Close(); err != nil {
		t.Fatalf("first engine Close: %v", err)
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("shared browser pid %d died when the first of two clients closed: %v", pid, err)
	}
	engines[1].Resize(60, 20)
	liveReadFrame(t, engines[1], "surviving engine frame after Resize")

	if err := engines[1].Close(); err != nil {
		t.Fatalf("last engine Close: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for unix.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("shared browser pid %d still alive 10 s after the last client closed", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	out, _ := exec.Command("pgrep", "-f", "--user-data-dir="+profileDir).Output() //nolint:gosec,noctx // profileDir is a t.TempDir path
	if left := strings.TrimSpace(string(out)); left != "" {
		t.Fatalf("processes still bound to %s after the last close: %s", profileDir, left)
	}
	t.Logf("shared_two_engines: browser pid %d gone after the last client closed, no process left on %s", pid, profileDir)
}

// liveReadLockPID reads the browser pid the launcher stored in the profile
// lock file and fails the test when the file is missing or malformed.
func liveReadLockPID(t *testing.T, profileDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(profileDir, profileLockName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("lock file pid %q: %v", raw, err)
	}
	return pid
}

// liveWaitPIDGone polls with a null signal until pid is gone or d passes.
func liveWaitPIDGone(t *testing.T, pid int, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for unix.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s: pid %d still alive after %v", what, pid, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// liveSharedBrowserKilledBothRecover drives the shared-browser recovery path:
// two engines share one Chromium, the browser is killed with SIGKILL, and both
// engines must rebuild onto one fresh browser within 20 s. A dead browser
// closes the websocket, the remote allocator cancels each holder context,
// supervise calls rebuild, launch reruns, one engine wins LOCK_EX and spawns,
// the other reads the new DevToolsActivePort under LOCK_SH and reconnects. The
// lock file must then name a different live pid, the first close must leave
// the browser for the survivor, and the last close must end it.
func liveSharedBrowserKilledBothRecover(t *testing.T) {
	profileDir := filepath.Join(t.TempDir(), "profile")
	t.Cleanup(func() {
		// Any Chromium still bound to this profile is a leak; kill it so the
		// temp directory removal and later runs are not blocked.
		_ = exec.Command("pkill", "-9", "-f", "--user-data-dir="+profileDir).Run() //nolint:gosec,noctx // profileDir is a t.TempDir path
	})

	start := time.Now()
	engines := make([]*ChromiumEngine, 2)
	for i := range engines {
		e := New("")
		if err := e.SetProfileDir(profileDir); err != nil {
			t.Fatalf("engine %d SetProfileDir: %v", i, err)
		}
		e.SetCellPx(8, 16)
		e.Resize(80, 24)
		if err := e.Navigate(liveTestPage); err != nil {
			t.Fatalf("engine %d navigate: %v", i, err)
		}
		t.Cleanup(func() { _ = e.Close() })
		engines[i] = e
	}
	for i, e := range engines {
		liveReadFrame(t, e, "engine "+strconv.Itoa(i)+" first frame")
	}
	t.Logf("shared_browser_killed_both_recover: two engines started and framed in %v", time.Since(start).Round(time.Millisecond))

	oldPID := liveReadLockPID(t, profileDir)
	if err := unix.Kill(oldPID, 0); err != nil {
		t.Fatalf("shared browser pid %d is not alive before the kill: %v", oldPID, err)
	}

	killed := time.Now()
	if err := unix.Kill(oldPID, unix.SIGKILL); err != nil {
		t.Fatalf("SIGKILL pid %d: %v", oldPID, err)
	}
	liveWaitPIDGone(t, oldPID, 10*time.Second, "killed browser")
	t.Logf("shared_browser_killed_both_recover: pid %d gone %v after SIGKILL", oldPID, time.Since(killed).Round(time.Millisecond))

	// Wait for a launcher to publish a fresh live pid, then prove both engines
	// render on it. The recovery budget covers both the relaunch and the frames.
	recoverDeadline := killed.Add(20 * time.Second)
	newPID := 0
	for {
		raw, err := os.ReadFile(filepath.Join(profileDir, profileLockName))
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 && pid != oldPID && unix.Kill(pid, 0) == nil {
				newPID = pid
				break
			}
		}
		if time.Now().After(recoverDeadline) {
			t.Fatalf("lock file still names the killed pid %d (or no live pid) 20 s after SIGKILL: %q", oldPID, raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("shared_browser_killed_both_recover: lock file names new live pid %d %v after SIGKILL", newPID, time.Since(killed).Round(time.Millisecond))

	for i, e := range engines {
		// Drop any frame that may still sit in the buffer, then provoke a fresh
		// one through Resize, which restarts the stream on the rebuilt session.
		// A Resize before the rebuild finishes only updates the held grid, so
		// keep asking until a frame lands or the budget ends.
		deadline := recoverDeadline
		what := "engine " + strconv.Itoa(i) + " frame after SIGKILL recovery"
		for framed := false; !framed; {
			for drained := false; !drained; {
				select {
				case _, ok := <-e.Frames():
					if !ok {
						t.Fatalf("%s: frames channel closed, the supervisor gave up the rebuild", what)
					}
				default:
					drained = true
				}
			}
			e.Resize(60+i, 20)
			select {
			case f, ok := <-e.Frames():
				if !ok {
					t.Fatalf("%s: frames channel closed, the supervisor gave up the rebuild", what)
				}
				if f.Img == nil {
					t.Fatalf("%s: frame has a nil bitmap", what)
				}
				framed = true
			case <-time.After(time.Until(deadline)):
				t.Fatalf("%s: no frame within 20 s of SIGKILL", what)
			}
		}
		t.Logf("shared_browser_killed_both_recover: engine %d framed on the new browser %v after SIGKILL", i, time.Since(killed).Round(time.Millisecond))
	}

	if pid := liveReadLockPID(t, profileDir); pid != newPID {
		t.Fatalf("lock file pid changed again during recovery: %d then %d", newPID, pid)
	}
	if err := unix.Kill(newPID, 0); err != nil {
		t.Fatalf("new shared browser pid %d is not alive after both engines framed: %v", newPID, err)
	}

	if err := engines[0].Close(); err != nil {
		t.Fatalf("first engine Close: %v", err)
	}
	if err := unix.Kill(newPID, 0); err != nil {
		t.Fatalf("new shared browser pid %d died when the first of two clients closed: %v", newPID, err)
	}
	engines[1].Resize(70, 22)
	liveReadFrame(t, engines[1], "surviving engine frame after Resize on the new browser")

	if err := engines[1].Close(); err != nil {
		t.Fatalf("last engine Close: %v", err)
	}
	liveWaitPIDGone(t, newPID, 10*time.Second, "new browser after the last close")
	out, _ := exec.Command("pgrep", "-f", "--user-data-dir="+profileDir).Output() //nolint:gosec,noctx // profileDir is a t.TempDir path
	if left := strings.TrimSpace(string(out)); left != "" {
		t.Fatalf("processes still bound to %s after the last close: %s", profileDir, left)
	}
	t.Logf("FACT (e): true, both engines recovered onto new pid %d after SIGKILL of pid %d, last close ended it, no process left on %s, total %v",
		newPID, oldPID, profileDir, time.Since(start).Round(time.Millisecond))
}
