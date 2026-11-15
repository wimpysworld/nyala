package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// remoteConnectFake records every dial launchRemote makes and returns the
// configured error, or a chromedp holder whose Browser is a zero value and whose
// context is already cancelled. A command on that Browser has no socket to
// write to and returns the context error, so newInstanceTarget fails cleanly
// without a running Chromium.
type remoteConnectFake struct {
	urls     []string
	timeouts []time.Duration
	err      error
}

func (f *remoteConnectFake) connect(parent context.Context, url string, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	f.urls = append(f.urls, url)
	f.timeouts = append(f.timeouts, timeout)
	if f.err != nil {
		return nil, nil, f.err
	}
	// The chromedp cancel waits for an allocation that never happens, so the
	// fake hands back the plain context cancel instead.
	base, cancel := context.WithCancel(parent)
	holder, _ := chromedp.NewContext(base)
	chromedp.FromContext(holder).Browser = &chromedp.Browser{}
	cancel()
	return holder, cancel, nil
}

// TestLaunchRemoteSelectsRemotePath asserts that launchRemote dials the
// configured endpoint exactly once with sharedConnectTimeout, opens no profile
// guard, and names the endpoint when the attach fails after the dial. The fake
// holder carries no Browser, so newInstanceTarget fails and the wrap is checked.
func TestLaunchRemoteSelectsRemotePath(t *testing.T) {
	const url = "http://127.0.0.1:9222"
	profileDir := t.TempDir()
	t.Setenv("NYALA_PROFILE", profileDir)
	e := New("")
	e.SetRemoteDebuggingURL(url)
	fake := &remoteConnectFake{}
	e.connectSharedOp = fake.connect

	_, _, _, err := e.launchRemote(url)
	if err == nil {
		t.Fatal("launchRemote with a fake holder returned nil error, want a target creation failure")
	}
	if !strings.Contains(err.Error(), url) {
		t.Fatalf("error %q does not name the endpoint %s", err, url)
	}
	if !strings.Contains(err.Error(), "did not launch Chromium") {
		t.Fatalf("error %q does not say that Nyala did not launch Chromium", err)
	}
	if !slices.Equal(fake.urls, []string{url}) {
		t.Fatalf("connect urls = %v, want [%s]", fake.urls, url)
	}
	if !slices.Equal(fake.timeouts, []time.Duration{sharedConnectTimeout}) {
		t.Fatalf("connect timeouts = %v, want [%v]", fake.timeouts, sharedConnectTimeout)
	}
	if e.profileGuard != nil {
		t.Fatal("launchRemote opened a profile guard, want none")
	}
	if _, err := waitDevToolsActivePort(expiredContext(t), profileDir); err == nil {
		t.Fatal("a DevToolsActivePort file appeared under the unused profile")
	}
}

// expiredContext returns a context that is already cancelled, so a poller
// returns at once with its current observation.
func expiredContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestLaunchRemoteSkipsExecutableResolution asserts that a set remote URL makes
// launch skip resolveExecutable: with every discovery route empty the error is
// the connect failure, not the "no Chromium found" resolution text.
func TestLaunchRemoteSkipsExecutableResolution(t *testing.T) {
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")
	t.Setenv("PATH", "")
	hideInstalledBrowsers(t)
	if _, err := resolveExecutable(""); err == nil {
		t.Fatal("resolveExecutable found a Chromium with every route empty, the test premise is wrong")
	}
	const url = "ws://127.0.0.1:9222/devtools/browser/abc"
	e := New("")
	e.SetRemoteDebuggingURL(url)
	dialErr := errors.New("dial refused")
	fake := &remoteConnectFake{err: dialErr}
	e.connectSharedOp = fake.connect

	err := e.launch("")
	if !errors.Is(err, dialErr) {
		t.Fatalf("launch error = %v, want the connect failure %v", err, dialErr)
	}
	if strings.Contains(err.Error(), "no Chromium found") {
		t.Fatalf("launch resolved an executable on the remote path: %v", err)
	}
	if len(fake.urls) != 1 || fake.urls[0] != url {
		t.Fatalf("connect urls = %v, want [%s]", fake.urls, url)
	}
}

// TestRemoteAllocatorOptionsByURLForm asserts the option choice per URL form: a
// ws URL that names /devtools/browser/ is used as given, and any other form is
// left to chromedp's /json/version lookup.
func TestRemoteAllocatorOptionsByURLForm(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"ws://127.0.0.1:9222/devtools/browser/0123-abcd", 1},
		{"http://127.0.0.1:9222", 0},
		{"http://127.0.0.1:9222/", 0},
		{"ws://127.0.0.1:9222", 0},
	}
	for _, c := range cases {
		if got := len(remoteAllocatorOptions(c.url)); got != c.want {
			t.Errorf("remoteAllocatorOptions(%q) returned %d options, want %d", c.url, got, c.want)
		}
	}
}

// TestRemoteCloseOpNeverTerminates asserts that the remote close op closes the
// target, then the socket, and never terminates a pid. When the target close
// fails, the error propagates and the socket still closes.
func TestRemoteCloseOpNeverTerminates(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		rec := &closeOpRecorder{}
		op := remoteCloseOp(rec.closeTarget, rec.closeSocket)
		if err := op(context.Background()); err != nil {
			t.Fatalf("remoteCloseOp: %v", err)
		}
		want := []string{"closeTarget", "closeSocket"}
		if !slices.Equal(rec.calls, want) {
			t.Fatalf("call order = %v, want %v", rec.calls, want)
		}
		if len(rec.pids) != 0 {
			t.Fatalf("terminate pids = %v, want none", rec.pids)
		}
	})
	t.Run("target_close_error", func(t *testing.T) {
		rec := &closeOpRecorder{}
		closeErr := errors.New("target gone")
		closeTarget := func(ctx context.Context) error {
			_ = rec.closeTarget(ctx)
			return closeErr
		}
		op := remoteCloseOp(closeTarget, rec.closeSocket)
		if err := op(context.Background()); !errors.Is(err, closeErr) {
			t.Fatalf("remoteCloseOp error = %v, want %v", err, closeErr)
		}
		want := []string{"closeTarget", "closeSocket"}
		if !slices.Equal(rec.calls, want) {
			t.Fatalf("call order = %v, want %v", rec.calls, want)
		}
		if len(rec.pids) != 0 {
			t.Fatalf("terminate pids = %v, want none", rec.pids)
		}
	})
}

// TestRemoteLostControlsListsLaunchOnlyFlags asserts the lost-controls list is
// non-empty, has no duplicates, and names the controls a user is most likely
// to set and then miss on a remote attach.
func TestRemoteLostControlsListsLaunchOnlyFlags(t *testing.T) {
	got := RemoteLostControls()
	if len(got) == 0 {
		t.Fatal("RemoteLostControls is empty")
	}
	seen := make(map[string]bool, len(got))
	for _, entry := range got {
		if seen[entry] {
			t.Fatalf("RemoteLostControls repeats %q", entry)
		}
		seen[entry] = true
	}
	for _, want := range []string{"force-device-scale-factor", "window-size", "proxy-server", "profile"} {
		found := false
		for _, entry := range got {
			if strings.Contains(entry, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("RemoteLostControls %v has no entry mentioning %q", got, want)
		}
	}
}

// TestLaunchRemoteRetriesOnRebuild asserts the reconnect window. The first
// launch fails at once on a dead endpoint. A rebuild, marked by a committed
// currentURL, keeps dialling until the endpoint answers, and stops early when
// Close arrives.
func TestLaunchRemoteRetriesOnRebuild(t *testing.T) {
	const url = "http://127.0.0.1:9222"
	saved := remoteReconnectTimeout
	remoteReconnectTimeout = 5 * time.Second
	t.Cleanup(func() { remoteReconnectTimeout = saved })

	t.Run("first launch fails fast", func(t *testing.T) {
		e := New("")
		e.SetRemoteDebuggingURL(url)
		fake := &remoteConnectFake{err: errors.New("dial refused")}
		e.connectSharedOp = fake.connect
		start := time.Now()
		if _, _, _, err := e.launchRemote(url); err == nil {
			t.Fatal("first launch on a dead endpoint returned nil")
		}
		if len(fake.urls) != 1 || time.Since(start) > remoteReconnectInterval {
			t.Fatalf("first launch dialled %d times in %v, want once at once", len(fake.urls), time.Since(start))
		}
	})

	t.Run("rebuild retries until the endpoint answers", func(t *testing.T) {
		e := New("")
		e.SetRemoteDebuggingURL(url)
		e.mu.Lock()
		e.currentURL = "https://example.com/"
		e.mu.Unlock()
		fake := &remoteConnectFake{err: errors.New("dial refused")}
		e.connectSharedOp = func(parent context.Context, u string, timeout time.Duration) (context.Context, context.CancelFunc, error) {
			if len(fake.urls) == 2 {
				fake.err = nil
			}
			return fake.connect(parent, u, timeout)
		}
		_, _, _, err := e.launchRemote(url)
		if err == nil || !strings.Contains(err.Error(), "did not launch Chromium") {
			t.Fatalf("rebuild error = %v, want the attach wrap after the dial succeeded", err)
		}
		if len(fake.urls) != 3 {
			t.Fatalf("rebuild dialled %d times, want 3 (two refusals, then success)", len(fake.urls))
		}
	})

	t.Run("rebuild stops on close", func(t *testing.T) {
		e := New("")
		e.SetRemoteDebuggingURL(url)
		e.mu.Lock()
		e.currentURL = "https://example.com/"
		e.mu.Unlock()
		fake := &remoteConnectFake{err: errors.New("dial refused")}
		e.connectSharedOp = fake.connect
		go func() {
			time.Sleep(remoteReconnectInterval / 2)
			e.closeOnce.Do(func() { close(e.closing) })
		}()
		start := time.Now()
		if _, _, _, err := e.launchRemote(url); err == nil {
			t.Fatal("rebuild returned nil while closing")
		}
		if time.Since(start) > 2*remoteReconnectInterval {
			t.Fatalf("rebuild kept dialling for %v after Close", time.Since(start))
		}
	})
}
