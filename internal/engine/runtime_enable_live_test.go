//go:build live

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const runtimeEnableProbePage = `<!doctype html>
<script>
const beacon = probe => navigator.sendBeacon('/hit?probe=' + probe);

Object.defineProperty(String.prototype, 'stack', {
  configurable: true,
  get() { beacon('primitive'); return 'primitive stack'; }
});
console.log('primitive control');

const own = new Error('own stack control');
Object.defineProperty(own, 'stack', {
  configurable: true,
  get() { beacon('own'); return 'own stack'; }
});
console.error(own);

const inherited = new Error('inherited stack probe');
delete inherited.stack;
const inheritedPrototype = Object.create(Error.prototype);
Object.defineProperty(inheritedPrototype, 'stack', {
  configurable: true,
  get() { beacon('inherited'); return 'inherited stack'; }
});
Object.setPrototypeOf(inherited, inheritedPrototype);
console.error(inherited);

fetch('/ready', { method: 'POST' });
</script>`

type runtimeEnableHits struct {
	primitive atomic.Int32
	own       atomic.Int32
	inherited atomic.Int32
}

type runtimeEnableResult struct {
	product  string
	revision string
	methods  []string
	hits     [3]int32
}

type rawCDPClient struct {
	conn    *chromedp.Conn
	nextID  int64
	methods []string
}

func (c *rawCDPClient) call(ctx context.Context, sessionID target.SessionID, method string, params, result any) error {
	closeDone := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() {
		_ = c.conn.Close()
		close(closeDone)
	})
	defer func() {
		if !stopClose() {
			<-closeDone
		}
	}()

	c.nextID++
	message := cdproto.Message{
		ID:        c.nextID,
		SessionID: sessionID,
		Method:    cdproto.MethodType(method),
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal %s parameters: %w", method, err)
		}
		message.Params = append(message.Params, raw...)
	}
	c.methods = append(c.methods, method)
	if err := c.conn.Write(ctx, &message); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}
	for {
		var response cdproto.Message
		if err := c.conn.Read(ctx, &response); err != nil {
			return fmt.Errorf("read %s response: %w", method, err)
		}
		if response.ID != message.ID {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("%s: %w", method, response.Error)
		}
		if result != nil {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decode %s response: %w", method, err)
			}
		}
		return nil
	}
}

func TestLiveRuntimeEnableInheritedStackGetter(t *testing.T) {
	const repetitions = 3

	var wantProduct, wantRevision string
	for _, enableRuntime := range []bool{false, true} {
		caseName := "without_Runtime.enable"
		if enableRuntime {
			caseName = "with_Runtime.enable"
		}
		t.Run(caseName, func(t *testing.T) {
			for repetition := range repetitions {
				result := runRuntimeEnableProbe(t, enableRuntime)
				if wantProduct == "" {
					wantProduct, wantRevision = result.product, result.revision
				}
				if result.product != wantProduct || result.revision != wantRevision {
					t.Fatalf("Browser.getVersion = %q revision %q, want %q revision %q", result.product, result.revision, wantProduct, wantRevision)
				}
				wantMethods := []string{"Browser.getVersion", "Target.createTarget", "Target.attachToTarget"}
				if enableRuntime {
					wantMethods = append(wantMethods, "Runtime.enable")
				}
				if !slices.Equal(result.methods, wantMethods) {
					t.Fatalf("outbound methods = %v, want exact ordered sequence %v", result.methods, wantMethods)
				}
				wantHits := [3]int32{}
				if enableRuntime {
					wantHits = [3]int32{0, 0, 1}
				}
				if result.hits != wantHits {
					t.Fatalf("getter hits [primitive own inherited] = %v, want %v", result.hits, wantHits)
				}
				primitive, own, inherited := result.hits[0], result.hits[1], result.hits[2]
				t.Logf("repetition %d: Browser.getVersion product=%q revision=%q, methods=%v, hits=primitive:%d own:%d inherited:%d", repetition+1, result.product, result.revision, result.methods, primitive, own, inherited)
			}
		})
	}
}

func runRuntimeEnableProbe(t *testing.T, enableRuntime bool) runtimeEnableResult {
	t.Helper()

	executable, err := resolveExecutable("")
	if err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	var hits runtimeEnableHits
	ready := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(runtimeEnableProbePage))
		case "/hit":
			switch r.URL.Query().Get("probe") {
			case "primitive":
				hits.primitive.Add(1)
			case "own":
				hits.own.Add(1)
			case "inherited":
				hits.inherited.Add(1)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/ready":
			select {
			case ready <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	profileDir, err := os.MkdirTemp("", "nyala-runtime-enable-")
	if err != nil {
		t.Fatalf("create Chromium profile: %v", err)
	}
	defer func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(profileDir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove Chromium profile: %v", err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(executable),
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("enable-automation", false),
	)
	allocatorContext, allocatorCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocatorCancel()
	browserContext, browserCancel := chromedp.NewContext(allocatorContext)
	defer browserCancel()

	chromedpContext := chromedp.FromContext(browserContext)
	browser, err := chromedpContext.Allocator.Allocate(browserContext)
	if err != nil {
		t.Fatalf("launch Chromium: %v", err)
	}
	chromedpContext.Browser = browser

	webSocketURL := readRawCDPWebSocketURL(t, profileDir)
	connection, err := chromedp.DialContext(browserContext, webSocketURL)
	if err != nil {
		t.Fatalf("connect to raw CDP endpoint: %v", err)
	}
	defer connection.Close()
	client := &rawCDPClient{conn: connection}

	callContext, cancelCall := context.WithTimeout(browserContext, 10*time.Second)
	defer cancelCall()
	var version struct {
		Product  string `json:"product"`
		Revision string `json:"revision"`
	}
	if err := client.call(callContext, "", "Browser.getVersion", nil, &version); err != nil {
		t.Fatal(err)
	}
	var created struct {
		TargetID target.ID `json:"targetId"`
	}
	if err := client.call(callContext, "", "Target.createTarget", map[string]any{"url": server.URL}, &created); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-callContext.Done():
		t.Fatal("probe page did not report readiness before CDP attachment")
	}
	time.Sleep(100 * time.Millisecond)
	if initial := [3]int32{hits.primitive.Load(), hits.own.Load(), hits.inherited.Load()}; initial != [3]int32{} {
		t.Fatalf("getter hits before CDP attachment = %v, want all zero", initial)
	}

	var attached struct {
		SessionID target.SessionID `json:"sessionId"`
	}
	if err := client.call(callContext, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	if enableRuntime {
		if err := client.call(callContext, attached.SessionID, "Runtime.enable", nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return runtimeEnableResult{
		product:  version.Product,
		revision: version.Revision,
		methods:  append([]string(nil), client.methods...),
		hits: [3]int32{
			hits.primitive.Load(),
			hits.own.Load(),
			hits.inherited.Load(),
		},
	}
}

func readRawCDPWebSocketURL(t *testing.T, profileDir string) string {
	t.Helper()

	path := filepath.Join(profileDir, "DevToolsActivePort")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
			if len(lines) == 2 {
				if _, err := strconv.ParseUint(lines[0], 10, 16); err == nil && strings.HasPrefix(lines[1], "/devtools/browser/") {
					return "ws://127.0.0.1:" + lines[0] + lines[1]
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Chromium did not write a valid %s", path)
	return ""
}
