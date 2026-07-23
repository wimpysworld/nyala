package engine

import (
	"context"
	"testing"

	"github.com/chromedp/cdproto/cdp"
)

// TestHistoryTarget asserts the history step maths: a step inside the entry
// list returns the target index, and a step off either end reports not-ok so
// the dispatch is a quiet no-op.
func TestHistoryTarget(t *testing.T) {
	cases := []struct {
		name    string
		current int64
		count   int
		delta   int
		want    int
		ok      bool
	}{
		{"back mid-list", 2, 4, -1, 1, true},
		{"forward mid-list", 1, 4, 1, 2, true},
		{"back at first", 0, 4, -1, 0, false},
		{"forward at last", 3, 4, 1, 0, false},
		{"empty history", 0, 0, -1, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := historyTarget(c.current, c.count, c.delta)
			if got != c.want || ok != c.ok {
				t.Fatalf("historyTarget(%d, %d, %d) = (%d, %v), want (%d, %v)",
					c.current, c.count, c.delta, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestNavHistoryDelta asserts the action-to-offset map: back steps -1, forward
// steps +1, and the non-history actions step 0.
func TestNavHistoryDelta(t *testing.T) {
	cases := []struct {
		a    NavAction
		want int
	}{
		{NavBack, -1},
		{NavForward, 1},
		{NavReload, 0},
		{NavHome, 0},
	}
	for _, c := range cases {
		if got := navHistoryDelta(c.a); got != c.want {
			t.Errorf("navHistoryDelta(%d) = %d, want %d", c.a, got, c.want)
		}
	}
}

// TestSendNavHomeNavigatesToLaunchURL asserts Alt+Home's engine half: NavHome
// re-navigates to the launch URL Navigate recorded, through the same
// runNavigate seam startup uses, and resets the held scroll offset because
// the new document starts at the top.
func TestSendNavHomeNavigatesToLaunchURL(t *testing.T) {
	e := New("")
	const launch = "https://example.org/start"
	var gotURL string
	e.mu.Lock()
	e.url = launch
	e.ctx = context.Background()
	e.scrollX, e.scrollY, e.scrollYTarget = 12, 340, 340
	e.navigateOp = func(_ context.Context, url string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		gotURL = url
		return "", "", false, nil
	}
	e.mu.Unlock()

	e.SendNav(NavHome)

	if gotURL != launch {
		t.Fatalf("NavHome navigated to %q, want the launch URL %q", gotURL, launch)
	}
	e.mu.Lock()
	x, y, target := e.scrollX, e.scrollY, e.scrollYTarget
	e.mu.Unlock()
	if x != 0 || y != 0 || target != 0 {
		t.Fatalf("NavHome kept scroll state (%d, %d, %d), want reset to 0", x, y, target)
	}
}

// TestSendNavNoSessionIsNoOp asserts a navigation request before a session
// exists (nil context) is dropped safely, matching the other input dispatchers.
func TestSendNavNoSessionIsNoOp(t *testing.T) {
	e := New("")
	for _, a := range []NavAction{NavReload, NavBack, NavForward, NavHome} {
		e.SendNav(a) // must not panic or dispatch
	}
}

// TestCurrentURL asserts the display URL seam: before the first commit
// CurrentURL falls back to the launch URL, a recorded commit replaces it, and
// an empty recorded URL is dropped rather than blanking the display.
func TestCurrentURL(t *testing.T) {
	e := New("")
	const launch = "https://example.org/start"
	e.mu.Lock()
	e.url = launch
	e.mu.Unlock()

	if got := e.CurrentURL(); got != launch {
		t.Fatalf("CurrentURL() = %q before a commit, want the launch URL %q", got, launch)
	}
	const committed = "https://example.org/page#section"
	e.recordCurrentURL(committed)
	if got := e.CurrentURL(); got != committed {
		t.Fatalf("CurrentURL() = %q after a commit, want %q", got, committed)
	}
	e.recordCurrentURL("")
	if got := e.CurrentURL(); got != committed {
		t.Fatalf("CurrentURL() = %q after an empty record, want %q kept", got, committed)
	}
}

// TestOpenURLNavigatesWithoutRewritingLaunchURL asserts the typed-URL engine
// half: OpenURL navigates through the same runNavigate seam NavHome uses,
// resets the held scroll offset because the new document starts at the top,
// and never rewrites the held launch URL NavHome returns to.
func TestOpenURLNavigatesWithoutRewritingLaunchURL(t *testing.T) {
	e := New("")
	const launch = "https://example.org/start"
	const typed = "https://example.net/typed"
	var gotURL string
	e.mu.Lock()
	e.url = launch
	e.ctx = context.Background()
	e.scrollX, e.scrollY, e.scrollYTarget = 12, 340, 340
	e.navigateOp = func(_ context.Context, url string) (cdp.FrameID, cdp.LoaderID, bool, error) {
		gotURL = url
		return "", "", false, nil
	}
	e.mu.Unlock()

	e.OpenURL(typed)

	if gotURL != typed {
		t.Fatalf("OpenURL navigated to %q, want %q", gotURL, typed)
	}
	e.mu.Lock()
	url, x, y, target := e.url, e.scrollX, e.scrollY, e.scrollYTarget
	e.mu.Unlock()
	if url != launch {
		t.Fatalf("OpenURL rewrote the launch URL to %q, want %q kept", url, launch)
	}
	if x != 0 || y != 0 || target != 0 {
		t.Fatalf("OpenURL kept scroll state (%d, %d, %d), want reset to 0", x, y, target)
	}
}

// TestOpenURLNoSessionIsNoOp asserts a typed URL before a session exists (nil
// context) is dropped safely, matching SendNav.
func TestOpenURLNoSessionIsNoOp(t *testing.T) {
	e := New("")
	e.OpenURL("https://example.org") // must not panic or dispatch
}
