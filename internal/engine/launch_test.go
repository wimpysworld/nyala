package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/fetch"
)

// fakeExecutable writes an executable file named name into a fresh temp dir and
// returns the dir and the full path. The file is mode 0o755 so exec.LookPath
// accepts it on the test PATH, keeping the lookup hermetic with no real browser.
func fakeExecutable(t *testing.T, name string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
	return dir, path
}

func TestResolveExecutablePrecedence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// A fake chromium on a temp dir prepended to PATH is the lowest-precedence
	// source. Each higher source must beat it.
	pathDir, pathExec := fakeExecutable(t, "chromium")

	cases := []struct {
		name         string
		flag         string
		nyalaBrowser string
		chromePath   string
		want         string
	}{
		{"flag wins over all", "/from/flag", "/from/nyala", "/from/chrome", "/from/flag"},
		{"nyala wins over chrome and path", "", "/from/nyala", "/from/chrome", "/from/nyala"},
		{"chrome wins over path", "", "", "/from/chrome", "/from/chrome"},
		{"path lookup is the fallback", "", "", "", pathExec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", pathDir)
			t.Setenv("NYALA_BROWSER", c.nyalaBrowser)
			t.Setenv("CHROME_PATH", c.chromePath)

			got, err := resolveExecutable(c.flag)
			if err != nil {
				t.Fatalf("resolveExecutable(%q) error: %v", c.flag, err)
			}
			if got != c.want {
				t.Errorf("resolveExecutable(%q) = %q, want %q", c.flag, got, c.want)
			}
		})
	}
}

func TestResolveExecutableNoneResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// Empty flag, unset envs, and an empty PATH leave nothing to find, so the
	// call must report a clear error rather than return a stray path.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	got, err := resolveExecutable("")
	if err == nil {
		t.Fatalf("resolveExecutable(\"\") = %q, want error", got)
	}
}

func TestFrameRateLimitFlags(t *testing.T) {
	if got := frameRateLimitFlags(false); got != nil {
		t.Errorf("frameRateLimitFlags(false) = %v, want nil", got)
	}
	want := []string{"disable-frame-rate-limit"}
	if got := frameRateLimitFlags(true); !reflect.DeepEqual(got, want) {
		t.Errorf("frameRateLimitFlags(true) = %v, want %v", got, want)
	}
}

func TestSetDisableFrameRateLimit(t *testing.T) {
	e := New("")
	if e.disableFrameRateLimit {
		t.Fatal("new engine has disableFrameRateLimit = true, want false")
	}
	e.SetDisableFrameRateLimit(true)
	if !e.disableFrameRateLimit {
		t.Fatal("SetDisableFrameRateLimit(true) left disableFrameRateLimit false")
	}
}

func TestProxyFlagsCredentialFreeServer(t *testing.T) {
	// A credentialed URL must yield a proxy-server flag whose value is the
	// credential-free Server, with no user:pass@ in any flag value. This is the
	// process-arg guard: credentials must never reach a Chromium command-line flag.
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://user:pass@host:3128"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	pairs := proxyFlags(cfg)
	if len(pairs) != 1 {
		t.Fatalf("proxyFlags = %v, want one proxy-server pair", pairs)
	}
	if pairs[0][0] != "proxy-server" {
		t.Errorf("flag name = %q, want %q", pairs[0][0], "proxy-server")
	}
	if pairs[0][1] != "http://host:3128" {
		t.Errorf("proxy-server value = %q, want %q", pairs[0][1], "http://host:3128")
	}
	// No flag value may carry credentials, in any form.
	for _, pair := range pairs {
		v := pair[1]
		if strings.Contains(v, "user") || strings.Contains(v, "pass") || strings.Contains(v, "@") {
			t.Errorf("flag %q value %q leaks credentials", pair[0], v)
		}
	}
}

func TestProxyFlagsServerOnly(t *testing.T) {
	// HTTPS_PROXY alone, no NO_PROXY, must yield a single proxy-server flag with
	// the verbatim server and no bypass flag.
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://host:3128"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	pairs := proxyFlags(cfg)
	want := [][2]string{{"proxy-server", "http://host:3128"}}
	if !reflect.DeepEqual(pairs, want) {
		t.Errorf("proxyFlags = %v, want %v", pairs, want)
	}
}

func TestProxyFlagsWithBypass(t *testing.T) {
	// A bypass value with a proxy set must yield both flags, the bypass verbatim.
	const bypass = ".internal,10.0.0.0/8,localhost"
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://host:3128", "NO_PROXY": bypass})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	pairs := proxyFlags(cfg)
	want := [][2]string{
		{"proxy-server", "http://host:3128"},
		{"proxy-bypass-list", bypass},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Errorf("proxyFlags = %v, want %v", pairs, want)
	}
}

func TestProxyFlagsStrayBypassNoProxy(t *testing.T) {
	// A NO_PROXY with no proxy var set must yield neither flag: the bypass is
	// gated on a Server, so a stray NO_PROXY alone adds nothing.
	clearProxyEnv(t, map[string]string{"NO_PROXY": "localhost,127.0.0.1"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	if pairs := proxyFlags(cfg); len(pairs) != 0 {
		t.Errorf("proxyFlags = %v, want no flags for a stray NO_PROXY", pairs)
	}
}

func TestProxyFlagsNoProxy(t *testing.T) {
	// Nothing set must yield no proxy flags at all.
	clearProxyEnv(t, nil)

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	if pairs := proxyFlags(cfg); pairs != nil {
		t.Errorf("proxyFlags = %v, want nil for no proxy", pairs)
	}
}

func TestLaunchReturnsProxyErrorBeforeResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup semantics differ on windows")
	}
	// launch returns the stored proxy parse error before resolveExecutable, so a
	// malformed proxy fails loudly and never connects direct. The seam: build a
	// real engine via New with a malformed proxy env, then drive launch with no
	// resolvable browser. The error must name the source variable, not the
	// resolveExecutable "no Chromium found" message, proving the guard fires first.
	// A control byte in the host forces a real url.Parse failure; the embedded
	// token stands in for a leaked credential.
	const secret = "leaktoken"
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://user:" + secret + "@host\x7f:3128"})
	// No browser is resolvable: empty flag, unset browser envs, empty PATH. The
	// guard must still surface the proxy error before any executable lookup.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("NYALA_BROWSER", "")
	t.Setenv("CHROME_PATH", "")

	e := New("")
	err := e.launch("")
	if err == nil {
		t.Fatal("launch() = nil, want the stored proxy parse error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTPS_PROXY") {
		t.Errorf("launch error %q does not name the source variable HTTPS_PROXY", msg)
	}
	if strings.Contains(msg, "no Chromium found") {
		t.Errorf("launch error %q is the resolveExecutable error; the proxy guard must fire first", msg)
	}
	if strings.Contains(msg, secret) {
		t.Errorf("launch error %q leaks the credential value", msg)
	}
}

func TestProxyAuthDecisionFirstProxyChallenge(t *testing.T) {
	// A Proxy-source EventAuthRequired, first seen for a request id, must answer
	// continueWithAuth with ProvideCredentials and the parsed user and password.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-1"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy},
	}

	action := proxyAuthDecision(ev, answered, cfg)
	if action.Kind != proxyAuthContinueWithAuth {
		t.Fatalf("Kind = %d, want proxyAuthContinueWithAuth", action.Kind)
	}
	if action.RequestID != ev.RequestID {
		t.Errorf("RequestID = %q, want %q", action.RequestID, ev.RequestID)
	}
	if action.Resp == nil {
		t.Fatal("Resp = nil, want an AuthChallengeResponse")
	}
	if action.Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Errorf("Response = %q, want ProvideCredentials", action.Resp.Response)
	}
	if action.Resp.Username != "user" || action.Resp.Password != "pass" {
		t.Errorf("credentials = %q/%q, want user/pass", action.Resp.Username, action.Resp.Password)
	}
}

func TestProxyAuthDecisionRepeatAnswersDefault(t *testing.T) {
	// A repeat challenge for the same request id must answer Default, the loop
	// guard: a second ProvideCredentials with wrong credentials would loop. The
	// transition depends on the answered map, which the first call populates.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-1"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy},
	}

	first := proxyAuthDecision(ev, answered, cfg)
	if first.Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Fatalf("first Response = %q, want ProvideCredentials", first.Resp.Response)
	}
	if !answered[ev.RequestID] {
		t.Fatal("answered map not updated after the first challenge")
	}

	repeat := proxyAuthDecision(ev, answered, cfg)
	if repeat.Kind != proxyAuthContinueWithAuth {
		t.Errorf("repeat Kind = %d, want proxyAuthContinueWithAuth", repeat.Kind)
	}
	if repeat.Resp == nil || repeat.Resp.Response != fetch.AuthChallengeResponseResponseDefault {
		t.Errorf("repeat Response = %v, want Default", repeat.Resp)
	}
	// Default must carry no credentials, so a wrong-credential loop cannot leak.
	if repeat.Resp.Username != "" || repeat.Resp.Password != "" {
		t.Errorf("repeat carries credentials %q/%q, want empty", repeat.Resp.Username, repeat.Resp.Password)
	}
}

func TestProxyAuthDecisionRequestPaused(t *testing.T) {
	// EventRequestPaused must answer continueRequest, the drain arm that releases
	// the pause enabling Fetch with handleAuthRequests creates.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventRequestPaused{RequestID: fetch.RequestID("req-2")}

	action := proxyAuthDecision(ev, answered, cfg)
	if action.Kind != proxyAuthContinueRequest {
		t.Errorf("Kind = %d, want proxyAuthContinueRequest", action.Kind)
	}
	if action.RequestID != ev.RequestID {
		t.Errorf("RequestID = %q, want %q", action.RequestID, ev.RequestID)
	}
}

func TestProxyAuthDecisionNonProxySource(t *testing.T) {
	// A non-Proxy auth source (server auth) is out of scope, so the policy answers
	// Default and lets Chromium's net stack handle it. No credentials are sent.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	answered := make(map[fetch.RequestID]bool)
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-3"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceServer},
	}

	action := proxyAuthDecision(ev, answered, cfg)
	if action.Kind != proxyAuthContinueWithAuth {
		t.Fatalf("Kind = %d, want proxyAuthContinueWithAuth", action.Kind)
	}
	if action.Resp == nil || action.Resp.Response != fetch.AuthChallengeResponseResponseDefault {
		t.Errorf("Response = %v, want Default for a server-source challenge", action.Resp)
	}
	if action.Resp.Username != "" || action.Resp.Password != "" {
		t.Errorf("server challenge carries credentials %q/%q, want empty", action.Resp.Username, action.Resp.Password)
	}
}

func TestProxyAuthDecisionTrackerFreshPerSession(t *testing.T) {
	// A fresh answered map is empty per session, so the same request id seen in a
	// new map gets ProvideCredentials again. This is what gives each rebuilt
	// session its own first-vs-repeat state, with no leak across sessions.
	cfg := ProxyConfig{User: "user", Password: "pass", HasCredentials: true}
	ev := &fetch.EventAuthRequired{
		RequestID:     fetch.RequestID("req-1"),
		AuthChallenge: &fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy},
	}

	first := make(map[fetch.RequestID]bool)
	if proxyAuthDecision(ev, first, cfg).Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Fatal("first session: want ProvideCredentials on first challenge")
	}
	if proxyAuthDecision(ev, first, cfg).Resp.Response != fetch.AuthChallengeResponseResponseDefault {
		t.Fatal("first session: want Default on repeat in the same map")
	}

	// A new session starts with a fresh, empty map: the same id is first again.
	second := make(map[fetch.RequestID]bool)
	if len(second) != 0 {
		t.Fatalf("new session map = %v, want empty", second)
	}
	if proxyAuthDecision(ev, second, cfg).Resp.Response != fetch.AuthChallengeResponseResponseProvideCredentials {
		t.Error("second session: want ProvideCredentials again with a fresh map")
	}
}

func TestSurfaceSizeNonZeroFloor(t *testing.T) {
	// A 0x0 grid (the agent shell reports no pixel size) must resolve to the
	// non-zero floor, never a blank 0x0 surface Chromium cannot lay out.
	e := &ChromiumEngine{backend: backendBlock, cellW: 9, cellH: 18}

	surfW, surfH := e.surfaceSize(0, 0)
	if surfW < surfaceFloorPx || surfH < surfaceFloorPx {
		t.Fatalf("surfaceSize(0,0) = (%d, %d), want each >= floor %d", surfW, surfH, surfaceFloorPx)
	}
	if surfW == 0 || surfH == 0 {
		t.Fatalf("surfaceSize(0,0) = (%d, %d), want a non-zero surface", surfW, surfH)
	}
}

func TestPaintedSurfaceHeightFloorNeverExceedsCap(t *testing.T) {
	// The real-geometry fault: at 158x86 cell 16x33 the surface is 2528x2838 and
	// Z = 2528/1280 = 1.975. A rounded CSS height (round(2838/1.975) = 1437) paints
	// 1437*1.975 = 2839 device rows, one over the 2838 cap, so Chromium downscales
	// every frame. The floor (1436) paints 2836 <= 2838, so no downscale.
	const surfW, surfH = 2528, 2838
	const z = 1.975
	cssW, cssH, paintW, paintH := paintedSurface(surfW, surfH, z)
	if paintW > surfW || paintH > surfH {
		t.Fatalf("paintedSurface(%d,%d,%g) painted (%d,%d) exceeds the cap (%d,%d)", surfW, surfH, z, paintW, paintH, surfW, surfH)
	}
	// The width divides exactly at the target layout width, so it lands on 1280.
	if cssW != 1280 {
		t.Errorf("cssW = %d, want 1280 (the target layout width)", cssW)
	}
	// The height floors rather than rounds, so cssH is 1436 not 1437.
	if cssH != 1436 {
		t.Errorf("cssH = %d, want 1436 (floor of 2838/1.975)", cssH)
	}
	if paintH != 2836 {
		t.Errorf("paintH = %d, want 2836 (round(1436*1.975), one row-band under the cap)", paintH)
	}
}

func TestPaintedSurfaceUnityScaleIdentity(t *testing.T) {
	// At Z == 1 (a graphics backend, or a block grid at or under the target width)
	// the painted surface must equal the input surface exactly, so the cap and the
	// compose model are unchanged from the pre-fix behaviour.
	const surfW, surfH = 1440, 720
	cssW, cssH, paintW, paintH := paintedSurface(surfW, surfH, 1)
	if cssW != surfW || cssH != surfH || paintW != surfW || paintH != surfH {
		t.Fatalf("paintedSurface(%d,%d,1) = css(%d,%d) paint(%d,%d), want all equal to the input surface", surfW, surfH, cssW, cssH, paintW, paintH)
	}
}

func TestScreencastCapNeverExceedsSurface(t *testing.T) {
	// screencastCap on the block backend must return a painted surface at or under
	// the real cell-grid surface, so the screencast cap never forces a downscale.
	const cols, rows = 158, 86
	const cellW, cellH = 16, 33
	const layoutWidth = 1280
	e := &ChromiumEngine{backend: backendBlock, cellW: cellW, cellH: cellH, cols: cols, rows: rows, layoutWidth: layoutWidth}

	surfW, surfH := e.surfaceSize(cols, rows)
	capW, capH := e.screencastCap(cols, rows)
	if capW > surfW || capH > surfH {
		t.Fatalf("screencastCap = (%d,%d) exceeds surfaceSize (%d,%d); Chromium would downscale", capW, capH, surfW, surfH)
	}
	// A graphics backend stays at Z == 1, so its cap equals the surface exactly.
	g := &ChromiumEngine{backend: backendSixel, cellW: cellW, cellH: cellH, cols: cols, rows: rows, layoutWidth: layoutWidth}
	gSurfW, gSurfH := g.surfaceSize(cols, rows)
	gCapW, gCapH := g.screencastCap(cols, rows)
	if gCapW != gSurfW || gCapH != gSurfH {
		t.Errorf("graphics screencastCap = (%d,%d), want the surface (%d,%d) at Z 1", gCapW, gCapH, gSurfW, gSurfH)
	}
}
