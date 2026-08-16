//go:build live

// This file is the WW-200 spike proof. It records two live facts about the
// pinned Chromium. First, the enable-less variant is disproven: without
// Runtime.enable, the isolated-world observer script runs but the name-scoped
// binding never appears in the world, so Runtime.bindingCalled never arrives.
// Second, with Runtime.enable retained, the isolated-world design works in
// full: the observer sees real layout shifts, the binding delivers events with
// the isolated execution context id, the page world cannot reach the observer
// state, and the enable sequence survives reload, disable and re-enable, a
// fresh session, and enabling before first navigation. Production code is
// untouched; the observer script is an adapted copy that announces its
// execution context id at install.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/chromedp"

	"github.com/chromedp/cdproto/target"
)

// layoutShiftWorldName is the isolated world the spike installs the observer
// into. The world name becomes the execution context name that the scoped
// binding matches.
const layoutShiftWorldName = "__nyala_world"

// layoutShiftDisproofWait bounds the waits that must see no event. The passing
// path delivers within milliseconds, so three seconds of silence is decisive.
const layoutShiftDisproofWait = 3 * time.Second

// layoutShiftProbePage is the local page the tests navigate to. It holds
// enough block content that a late element insertion before the first child
// moves the content down and raises a layout-shift entry above the epsilon.
const layoutShiftProbePage = `<!doctype html>
<html><body>
<div id="anchor" style="height:60px">anchor content</div>
<div style="height:120px">first filler block</div>
<div style="height:120px">second filler block</div>
<div style="height:120px">third filler block</div>
</body></html>`

// layoutShiftTriggerExpr inserts a tall block element before the first body
// child, which shifts all following content down. It returns false until the
// document is complete, because an insertion before first paint lands in the
// initial layout and records no layout-shift entry; a caller retries under a
// deadline.
const layoutShiftTriggerExpr = `(function(){
if(document.readyState!=='complete')return false;
if(!document.body||!document.body.firstChild)return false;
var d=document.createElement('div');
d.style.height='200px';
d.textContent='late inserted block';
document.body.insertBefore(d,document.body.firstChild);
return true;})()`

// buildIsolatedLayoutShiftScript returns an adapted copy of the production
// observer script for the isolated world. The one change: it calls the binding
// with 'install' as soon as the binding function exists, so the resulting
// Runtime.bindingCalled event announces the isolated execution context id.
// Shift pokes carry a 'shift:<cumulative>:<generation>' payload so the tests
// can tell the two apart.
func buildIsolatedLayoutShiftScript() string {
	return fmt.Sprintf(`(function(){
if(window.%[1]s===undefined)window.%[1]s=0;
var fn=window[%[2]q];if(typeof fn!=='function')return;
fn('install');
if(typeof PerformanceObserver!=='function')return;
var supported=PerformanceObserver.supportedEntryTypes;
if(supported&&supported.indexOf('layout-shift')===-1)return;
var cumulative=0,pending=0;
var o=new PerformanceObserver(function(list){
var entries=list.getEntries();
for(var i=0;i<entries.length;i++){var e=entries[i];
if(e.hadRecentInput)continue;
if(!(e.value>0))continue;
window.%[1]s++;
cumulative+=e.value;pending+=e.value;}
if(pending>%[3]g){pending=0;try{fn('shift:'+String(cumulative)+':'+String(window.%[1]s));}catch(err){}}
});
window.%[4]s=o;
try{o.observe({type:'layout-shift',buffered:true});}catch(err){}
})();`, layoutGenProperty, layoutShiftBinding, layoutShiftEpsilon, layoutShiftObserverHandle)
}

// layoutShiftEnumerationExpr checks, from whichever world evaluates it, whether
// the three observer names are reachable through 'in window',
// Object.getOwnPropertyNames, and typeof. It returns a JSON string.
func layoutShiftEnumerationExpr() string {
	return fmt.Sprintf(`JSON.stringify({
inBinding:%[1]q in window,
inHandle:%[2]q in window,
inGen:%[3]q in window,
own:Object.getOwnPropertyNames(window).filter(function(n){return n===%[1]q||n===%[2]q||n===%[3]q;}),
typeBinding:typeof window[%[1]q],
typeHandle:typeof window[%[2]q],
typeGen:typeof window[%[3]q]})`, layoutShiftBinding, layoutShiftObserverHandle, layoutGenProperty)
}

// layoutShiftEnumeration is the decoded result of layoutShiftEnumerationExpr.
type layoutShiftEnumeration struct {
	InBinding   bool     `json:"inBinding"`
	InHandle    bool     `json:"inHandle"`
	InGen       bool     `json:"inGen"`
	Own         []string `json:"own"`
	TypeBinding string   `json:"typeBinding"`
	TypeHandle  string   `json:"typeHandle"`
	TypeGen     string   `json:"typeGen"`
}

// hidden reports whether every reachability probe came back negative.
func (e layoutShiftEnumeration) hidden() bool {
	return !e.InBinding && !e.InHandle && !e.InGen && len(e.Own) == 0 &&
		e.TypeBinding == "undefined" && e.TypeHandle == "undefined" && e.TypeGen == "undefined"
}

// layoutShiftBindingEvent is the decoded Runtime.bindingCalled payload the
// tests wait for.
type layoutShiftBindingEvent struct {
	Name               string `json:"name"`
	Payload            string `json:"payload"`
	ExecutionContextID int64  `json:"executionContextId"`
	sessionID          target.SessionID
}

// layoutShiftCDPClient is a raw CDP client with a dedicated reader goroutine,
// so the tests can run bounded waits on events while commands stay
// synchronous. The runRuntimeEnableProbe client reads inline and drops events,
// which this spike cannot afford.
type layoutShiftCDPClient struct {
	conn   *chromedp.Conn
	nextID int64

	mu        sync.Mutex
	pending   map[int64]chan *cdproto.Message
	forbidden []string

	events chan *cdproto.Message
	done   chan struct{}
}

// newLayoutShiftCDPClient wraps the connection and starts the reader.
func newLayoutShiftCDPClient(conn *chromedp.Conn) *layoutShiftCDPClient {
	c := &layoutShiftCDPClient{
		conn:    conn,
		pending: map[int64]chan *cdproto.Message{},
		events:  make(chan *cdproto.Message, 256),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// readLoop routes command responses to their waiters and events to the event
// channel. It records any Runtime.executionContextCreated or
// Runtime.consoleAPICalled event, which must never arrive on the enable-less
// path. It exits when the connection closes.
func (c *layoutShiftCDPClient) readLoop() {
	defer close(c.done)
	for {
		message := new(cdproto.Message)
		if err := c.conn.Read(context.Background(), message); err != nil {
			return
		}
		if message.Method != "" {
			method := string(message.Method)
			if method == "Runtime.executionContextCreated" || method == "Runtime.consoleAPICalled" {
				c.mu.Lock()
				c.forbidden = append(c.forbidden, method)
				c.mu.Unlock()
			}
			select {
			case c.events <- message:
			default:
			}
			continue
		}
		c.mu.Lock()
		waiter := c.pending[message.ID]
		delete(c.pending, message.ID)
		c.mu.Unlock()
		if waiter != nil {
			waiter <- message
		}
	}
}

// call sends one command on the optional session and waits, bounded by the
// context, for its response, decoding the result when asked.
func (c *layoutShiftCDPClient) call(ctx context.Context, sessionID target.SessionID, method string, params, result any) error {
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
		message.Params = raw
	}
	waiter := make(chan *cdproto.Message, 1)
	c.mu.Lock()
	c.pending[message.ID] = waiter
	c.mu.Unlock()
	if err := c.conn.Write(ctx, &message); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}
	select {
	case response := <-waiter:
		if response.Error != nil {
			return fmt.Errorf("%s: %w", method, response.Error)
		}
		if result != nil {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decode %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("%s: connection closed", method)
	}
}

// waitBindingCalled waits, bounded by the timeout, for a Runtime.bindingCalled
// event whose payload starts with the given prefix, and fails the test when
// none arrives.
func (c *layoutShiftCDPClient) waitBindingCalled(t *testing.T, payloadPrefix string, timeout time.Duration) layoutShiftBindingEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case message := <-c.events:
			if string(message.Method) != "Runtime.bindingCalled" {
				continue
			}
			var event layoutShiftBindingEvent
			if err := json.Unmarshal(message.Params, &event); err != nil {
				t.Fatalf("decode Runtime.bindingCalled: %v", err)
			}
			event.sessionID = message.SessionID
			if event.Name != layoutShiftBinding {
				t.Fatalf("Runtime.bindingCalled name = %q, want %q", event.Name, layoutShiftBinding)
			}
			if !strings.HasPrefix(event.Payload, payloadPrefix) {
				continue
			}
			if event.ExecutionContextID == 0 {
				t.Fatalf("Runtime.bindingCalled payload %q carried executionContextId 0, want non-zero", event.Payload)
			}
			return event
		case <-deadline:
			t.Fatalf("no Runtime.bindingCalled with payload prefix %q within %v", payloadPrefix, timeout)
		case <-c.done:
			t.Fatal("CDP connection closed while waiting for Runtime.bindingCalled")
		}
	}
}

// waitEvent waits, bounded by the timeout, for one event with the given
// method, and fails the test when none arrives. The tests use it for
// Page.loadEventFired: a layout-shift entry needs elements with rendered
// positions from an earlier frame, so a shift triggered before first paint
// records nothing.
func (c *layoutShiftCDPClient) waitEvent(t *testing.T, method string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case message := <-c.events:
			if string(message.Method) == method {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event within %v", method, timeout)
		case <-c.done:
			t.Fatalf("CDP connection closed while waiting for %s", method)
		}
	}
}

// assertNoBindingCalled drains events for the given duration and fails the
// test when any Runtime.bindingCalled arrives.
func (c *layoutShiftCDPClient) assertNoBindingCalled(t *testing.T, wait time.Duration) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case message := <-c.events:
			if string(message.Method) == "Runtime.bindingCalled" {
				t.Fatalf("unexpected Runtime.bindingCalled: params=%s", message.Params)
			}
		case <-deadline:
			return
		}
	}
}

// assertNoForbiddenRuntimeEvents fails the test when the enable-less session
// received Runtime.executionContextCreated or Runtime.consoleAPICalled.
func (c *layoutShiftCDPClient) assertNoForbiddenRuntimeEvents(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	forbidden := append([]string(nil), c.forbidden...)
	c.mu.Unlock()
	if len(forbidden) != 0 {
		t.Fatalf("Runtime events arrived without Runtime.enable: %v", forbidden)
	}
}

// evaluate runs one expression through Runtime.evaluate with returnByValue.
// A zero context id evaluates in the page main world; a non-zero id targets
// the isolated world.
func (c *layoutShiftCDPClient) evaluate(ctx context.Context, sessionID target.SessionID, expression string, contextID int64, value any) error {
	params := map[string]any{"expression": expression, "returnByValue": true}
	if contextID != 0 {
		params["contextId"] = contextID
	}
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call(ctx, sessionID, "Runtime.evaluate", params, &result); err != nil {
		return err
	}
	if result.ExceptionDetails != nil {
		return fmt.Errorf("Runtime.evaluate threw: %s", result.ExceptionDetails.Text)
	}
	if value != nil && result.Result.Value != nil {
		return json.Unmarshal(result.Result.Value, value)
	}
	return nil
}

// triggerLayoutShift retries the insertion expression in the page main world
// until it reports the body exists, bounded by the timeout.
func (c *layoutShiftCDPClient) triggerLayoutShift(t *testing.T, ctx context.Context, sessionID target.SessionID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var inserted bool
		if err := c.evaluate(ctx, sessionID, layoutShiftTriggerExpr, 0, &inserted); err == nil && inserted {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("could not insert the late block element within %v", timeout)
}

// enableIsolatedLayoutShift runs the production-shaped enable sequence:
// binding first, scoped to the world name, then the on-new-document observer
// script in that world with runImmediately. It returns the script identifier.
func (c *layoutShiftCDPClient) enableIsolatedLayoutShift(ctx context.Context, sessionID target.SessionID) (string, error) {
	if err := c.call(ctx, sessionID, "Runtime.addBinding", map[string]any{
		"name":                 layoutShiftBinding,
		"executionContextName": layoutShiftWorldName,
	}, nil); err != nil {
		return "", err
	}
	var added struct {
		Identifier string `json:"identifier"`
	}
	if err := c.call(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source":         buildIsolatedLayoutShiftScript(),
		"worldName":      layoutShiftWorldName,
		"runImmediately": true,
	}, &added); err != nil {
		return "", err
	}
	return added.Identifier, nil
}

// layoutShiftSession is one launched Chromium with a raw CDP client and the
// local probe server.
type layoutShiftSession struct {
	client *layoutShiftCDPClient
	server *httptest.Server
	ctx    context.Context
}

// startLayoutShiftSession launches a fresh-profile Chromium, connects the raw
// CDP client, records the browser identity in the test log, and registers all
// cleanup. It skips the test when no Chromium resolves.
func startLayoutShiftSession(t *testing.T) *layoutShiftSession {
	t.Helper()

	executable, err := resolveExecutable("")
	if err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(layoutShiftProbePage))
	}))
	t.Cleanup(server.Close)

	profileDir, err := os.MkdirTemp("", "nyala-layoutshift-")
	if err != nil {
		t.Fatalf("create Chromium profile: %v", err)
	}
	t.Cleanup(func() {
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
	})

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(executable),
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("enable-automation", false),
	)
	allocatorContext, allocatorCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(allocatorCancel)
	browserContext, browserCancel := chromedp.NewContext(allocatorContext)
	t.Cleanup(browserCancel)

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
	t.Cleanup(func() { _ = connection.Close() })
	client := newLayoutShiftCDPClient(connection)

	callContext, cancelCall := context.WithTimeout(browserContext, 90*time.Second)
	t.Cleanup(cancelCall)

	var version struct {
		Product  string `json:"product"`
		Revision string `json:"revision"`
	}
	if err := client.call(callContext, "", "Browser.getVersion", nil, &version); err != nil {
		t.Fatal(err)
	}
	t.Logf("Browser.getVersion product=%q revision=%q", version.Product, version.Revision)

	return &layoutShiftSession{client: client, server: server, ctx: callContext}
}

// createAndAttach creates a page target at the given URL, attaches a
// flattened session to it, and enables the Page domain. The Page domain is not
// the Runtime domain: on-new-document scripts run only while Page is enabled,
// and production keeps it enabled for the screencast, so every variant here,
// including the enable-less disproof, matches that.
func (s *layoutShiftSession) createAndAttach(t *testing.T, url string) (target.ID, target.SessionID) {
	t.Helper()
	var created struct {
		TargetID target.ID `json:"targetId"`
	}
	if err := s.client.call(s.ctx, "", "Target.createTarget", map[string]any{"url": url}, &created); err != nil {
		t.Fatal(err)
	}
	var attached struct {
		SessionID target.SessionID `json:"sessionId"`
	}
	if err := s.client.call(s.ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	if err := s.client.call(s.ctx, attached.SessionID, "Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	return created.TargetID, attached.SessionID
}

// enableRuntime turns the Runtime domain on for the session, the trade-off the
// enable-less disproof forces the working variant to retain.
func (s *layoutShiftSession) enableRuntime(t *testing.T, sessionID target.SessionID) {
	t.Helper()
	if err := s.client.call(s.ctx, sessionID, "Runtime.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// isolatedWorldEnumeration reads the observer state straight out of the named
// isolated world. Page.createIsolatedWorld returns the existing world's
// execution context id when the named world already exists, so this tells a
// script that never ran apart from a binding that never appeared.
func (s *layoutShiftSession) isolatedWorldEnumeration(t *testing.T, sessionID target.SessionID) layoutShiftEnumeration {
	t.Helper()
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := s.client.call(s.ctx, sessionID, "Page.getFrameTree", nil, &tree); err != nil {
		t.Fatalf("Page.getFrameTree: %v", err)
	}
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := s.client.call(s.ctx, sessionID, "Page.createIsolatedWorld", map[string]any{
		"frameId":   tree.FrameTree.Frame.ID,
		"worldName": layoutShiftWorldName,
	}, &world); err != nil {
		t.Fatalf("Page.createIsolatedWorld: %v", err)
	}
	var raw string
	if err := s.client.evaluate(s.ctx, sessionID, layoutShiftEnumerationExpr(), world.ExecutionContextID, &raw); err != nil {
		t.Fatalf("evaluate in isolated context %d: %v", world.ExecutionContextID, err)
	}
	var enumeration layoutShiftEnumeration
	if err := json.Unmarshal([]byte(raw), &enumeration); err != nil {
		t.Fatalf("decode isolated enumeration result %q: %v", raw, err)
	}
	return enumeration
}

// assertEnableLessDisproof proves the observed enable-less behaviour on the
// current document: no Runtime.bindingCalled within the bounded wait, the
// observer script ran in the isolated world (the generation counter exists),
// and the name-scoped binding stayed undefined, so the script's guard
// returned. Delivery within the wait, a missing counter, or a present binding
// each fail the test, because any of the three would overturn the disproof.
func (s *layoutShiftSession) assertEnableLessDisproof(t *testing.T, sessionID target.SessionID, phase string) {
	t.Helper()
	s.client.assertNoBindingCalled(t, layoutShiftDisproofWait)
	enumeration := s.isolatedWorldEnumeration(t, sessionID)
	if enumeration.TypeGen != "number" {
		t.Fatalf("%s: isolated world generation counter typeof = %q, want \"number\" (the observer script did not run)", phase, enumeration.TypeGen)
	}
	if enumeration.TypeBinding != "undefined" {
		t.Fatalf("%s: isolated world binding typeof = %q, want \"undefined\" (the scoped binding appeared without Runtime.enable, which overturns the disproof)", phase, enumeration.TypeBinding)
	}
	s.client.assertNoForbiddenRuntimeEvents(t)
	t.Logf("%s: enable-less disproof holds: script ran (typeof gen=%q), binding absent (typeof=%q), no bindingCalled within %v", phase, enumeration.TypeGen, enumeration.TypeBinding, layoutShiftDisproofWait)
}

// assertPageWorldHidden evaluates the enumeration expression in the page main
// world and fails the test when any observer name is reachable.
func (s *layoutShiftSession) assertPageWorldHidden(t *testing.T, sessionID target.SessionID, phase string) {
	t.Helper()
	var raw string
	if err := s.client.evaluate(s.ctx, sessionID, layoutShiftEnumerationExpr(), 0, &raw); err != nil {
		t.Fatalf("%s: page-world enumeration evaluate: %v", phase, err)
	}
	var enumeration layoutShiftEnumeration
	if err := json.Unmarshal([]byte(raw), &enumeration); err != nil {
		t.Fatalf("%s: decode enumeration result %q: %v", phase, raw, err)
	}
	if !enumeration.hidden() {
		t.Fatalf("%s: page world can reach isolated observer state: %s", phase, raw)
	}
}

// TestLayoutShiftEnableLessDisproof proves the enable-less variant does not
// work in the pinned Chromium: without Runtime.enable the on-new-document
// isolated-world script runs (with runImmediately on the current document and
// again after reload), but Runtime.addBinding with executionContextName never
// exposes the binding function in the world, so no Runtime.bindingCalled can
// arrive. The trade-off follows: isolated-world hiding must retain
// Runtime.enable.
func TestLayoutShiftEnableLessDisproof(t *testing.T) {
	session := startLayoutShiftSession(t)
	client := session.client

	_, sessionID := session.createAndAttach(t, session.server.URL)
	if _, err := client.enableIsolatedLayoutShift(session.ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	session.assertEnableLessDisproof(t, sessionID, "initial document")

	// A fresh document takes the on-new-document route rather than
	// runImmediately, and the binding stays absent there too.
	if err := client.call(session.ctx, sessionID, "Page.reload", nil, nil); err != nil {
		t.Fatal(err)
	}
	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	session.assertEnableLessDisproof(t, sessionID, "after reload with a real shift")
}

// TestLayoutShiftEnableLessFreshSession proves the enable-less variant also
// fails on a fresh target and session, the crash-recovery analogue: the
// re-armed enable sequence on the new session still exposes no binding and
// delivers no event, even for a real layout shift.
func TestLayoutShiftEnableLessFreshSession(t *testing.T) {
	session := startLayoutShiftSession(t)
	client := session.client

	targetID, sessionID := session.createAndAttach(t, session.server.URL)
	if _, err := client.enableIsolatedLayoutShift(session.ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := client.call(session.ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil); err != nil {
		t.Fatal(err)
	}

	_, freshSessionID := session.createAndAttach(t, session.server.URL)
	if _, err := client.enableIsolatedLayoutShift(session.ctx, freshSessionID); err != nil {
		t.Fatal(err)
	}
	client.triggerLayoutShift(t, session.ctx, freshSessionID, 10*time.Second)
	session.assertEnableLessDisproof(t, freshSessionID, "fresh session with a real shift")
}

// TestLayoutShiftEnableLessInitialLaunch proves the enable-less variant also
// fails when the sequence runs before first navigation, the launch analogue:
// the first real document runs the observer script but never sees the binding,
// even for a real layout shift.
func TestLayoutShiftEnableLessInitialLaunch(t *testing.T) {
	session := startLayoutShiftSession(t)
	client := session.client

	_, sessionID := session.createAndAttach(t, "about:blank")
	if _, err := client.enableIsolatedLayoutShift(session.ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := client.call(session.ctx, sessionID, "Page.navigate", map[string]any{"url": session.server.URL}, nil); err != nil {
		t.Fatal(err)
	}
	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	session.assertEnableLessDisproof(t, sessionID, "first document with a real shift")
}

// TestLayoutShiftIsolatedWorldObserver proves the working variant, with
// Runtime.enable retained, on one session: the isolated observer sees a real
// layout shift and delivers Runtime.bindingCalled with the announced isolated
// execution context id, the page world cannot reach the three observer names
// before or after reload, reload re-installs the observer, and the
// disable-then-re-enable sequence (the graphics-to-Block re-entry analogue)
// stops and restores delivery on the same document.
func TestLayoutShiftIsolatedWorldObserver(t *testing.T) {
	session := startLayoutShiftSession(t)
	client := session.client

	_, sessionID := session.createAndAttach(t, session.server.URL)
	session.enableRuntime(t, sessionID)
	scriptID, err := client.enableIsolatedLayoutShift(session.ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	install := client.waitBindingCalled(t, "install", 10*time.Second)
	t.Logf("install announce: payload=%q executionContextId=%d", install.Payload, install.ExecutionContextID)

	session.assertPageWorldHidden(t, sessionID, "before reload")

	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	shift := client.waitBindingCalled(t, "shift:", 10*time.Second)
	if shift.ExecutionContextID != install.ExecutionContextID {
		t.Fatalf("shift executionContextId = %d, install announced %d", shift.ExecutionContextID, install.ExecutionContextID)
	}
	t.Logf("first shift: payload=%q executionContextId=%d", shift.Payload, shift.ExecutionContextID)

	session.assertPageWorldHidden(t, sessionID, "after first shift")

	if err := client.call(session.ctx, sessionID, "Page.reload", nil, nil); err != nil {
		t.Fatal(err)
	}
	reloadInstall := client.waitBindingCalled(t, "install", 10*time.Second)
	client.waitEvent(t, "Page.loadEventFired", 10*time.Second)
	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	reloadShift := client.waitBindingCalled(t, "shift:", 10*time.Second)
	t.Logf("post-reload: install context=%d, shift payload=%q", reloadInstall.ExecutionContextID, reloadShift.Payload)

	session.assertPageWorldHidden(t, sessionID, "after reload")

	// Disable: remove the on-new-document script, disconnect the observer in
	// the isolated world, and remove the binding, then prove a real shift no
	// longer raises an event.
	if err := client.call(session.ctx, sessionID, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": scriptID}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.evaluate(session.ctx, sessionID, buildLayoutShiftRemovalScript(), reloadInstall.ExecutionContextID, nil); err != nil {
		t.Fatalf("run removal script in the isolated world: %v", err)
	}
	if err := client.call(session.ctx, sessionID, "Runtime.removeBinding", map[string]any{"name": layoutShiftBinding}, nil); err != nil {
		t.Fatal(err)
	}
	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	client.assertNoBindingCalled(t, layoutShiftDisproofWait)

	// Re-enable on the same document and prove delivery returns.
	if _, err := client.enableIsolatedLayoutShift(session.ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	reenableInstall := client.waitBindingCalled(t, "install", 10*time.Second)
	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	reenableShift := client.waitBindingCalled(t, "shift:", 10*time.Second)
	t.Logf("re-enable: install context=%d, shift payload=%q", reenableInstall.ExecutionContextID, reenableShift.Payload)
}

// TestLayoutShiftIsolatedWorldFreshSession proves the working variant's
// crash-recovery analogue: close the target, create a fresh one, re-run
// Runtime.enable plus the enable sequence on the new session, and delivery
// returns, mirroring the startSession re-arm in recovery.go.
func TestLayoutShiftIsolatedWorldFreshSession(t *testing.T) {
	session := startLayoutShiftSession(t)
	client := session.client

	targetID, sessionID := session.createAndAttach(t, session.server.URL)
	session.enableRuntime(t, sessionID)
	if _, err := client.enableIsolatedLayoutShift(session.ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	first := client.waitBindingCalled(t, "install", 10*time.Second)
	t.Logf("first session install: executionContextId=%d", first.ExecutionContextID)

	if err := client.call(session.ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil); err != nil {
		t.Fatal(err)
	}

	_, freshSessionID := session.createAndAttach(t, session.server.URL)
	session.enableRuntime(t, freshSessionID)
	if _, err := client.enableIsolatedLayoutShift(session.ctx, freshSessionID); err != nil {
		t.Fatal(err)
	}
	install := client.waitBindingCalled(t, "install", 10*time.Second)
	if install.sessionID != freshSessionID {
		t.Fatalf("install arrived on session %q, want fresh session %q", install.sessionID, freshSessionID)
	}
	client.triggerLayoutShift(t, session.ctx, freshSessionID, 10*time.Second)
	shift := client.waitBindingCalled(t, "shift:", 10*time.Second)
	t.Logf("fresh session shift: payload=%q executionContextId=%d", shift.Payload, shift.ExecutionContextID)
}

// TestLayoutShiftIsolatedWorldInitialLaunch proves the working variant's
// launch analogue: run Runtime.enable plus the enable sequence on an
// about:blank target before the first navigation, as launch.go arms forcing
// before the first page loads, then navigate, and the first real document
// installs the observer, hides its state from the page world, and delivers on
// a real shift.
func TestLayoutShiftIsolatedWorldInitialLaunch(t *testing.T) {
	session := startLayoutShiftSession(t)
	client := session.client

	_, sessionID := session.createAndAttach(t, "about:blank")
	session.enableRuntime(t, sessionID)
	if _, err := client.enableIsolatedLayoutShift(session.ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	blankInstall := client.waitBindingCalled(t, "install", 10*time.Second)
	t.Logf("about:blank install: executionContextId=%d", blankInstall.ExecutionContextID)

	if err := client.call(session.ctx, sessionID, "Page.navigate", map[string]any{"url": session.server.URL}, nil); err != nil {
		t.Fatal(err)
	}
	install := client.waitBindingCalled(t, "install", 10*time.Second)
	client.waitEvent(t, "Page.loadEventFired", 10*time.Second)
	client.triggerLayoutShift(t, session.ctx, sessionID, 10*time.Second)
	shift := client.waitBindingCalled(t, "shift:", 10*time.Second)
	t.Logf("first document: install context=%d, shift payload=%q", install.ExecutionContextID, shift.Payload)
	session.assertPageWorldHidden(t, sessionID, "first document")
}
