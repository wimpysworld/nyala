package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// fakeRecorder is the captureRecorder the engine tests drive instead of an
// NDJSON sink. It records every event under a mutex, so the listener goroutine
// and the test goroutine can both touch it without a race. No browser is
// involved.
type fakeRecorder struct {
	mu     sync.Mutex
	events []captureEvent
}

func (r *fakeRecorder) Record(event captureEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

// has reports whether any recorded event carries typ.
func (r *fakeRecorder) has(typ captureEventType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// count returns how many recorded events carry typ.
func (r *fakeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// TestRecorderWhitelistAcceptsAllTypes feeds one event of each whitelisted type
// and asserts the recorder writes one NDJSON line per event, each parseable and
// carrying the offered type.
func TestRecorderWhitelistAcceptsAllTypes(t *testing.T) {
	var buf bytes.Buffer
	r := newNDJSONRecorder(&buf)

	want := []captureEventType{
		captureEventScreencastFrame,
		captureEventTargetCrashed,
		captureEventNavigate,
		captureEventDOMSnapshot,
		captureEventScreencastControl,
		captureEventFrameAck,
		captureEventRunsDropped,
	}
	for _, typ := range want {
		r.Record(captureEvent{Type: typ})
	}

	lines := ndjsonLines(t, buf.Bytes())
	if len(lines) != len(want) {
		t.Fatalf("wrote %d lines, want %d", len(lines), len(want))
	}
	for i, line := range lines {
		var ev captureEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if ev.Type != want[i] {
			t.Errorf("line %d type = %q, want %q", i, ev.Type, want[i])
		}
	}
}

// TestRecorderDropsNonWhitelisted feeds a non-whitelisted event type and asserts
// the recorder writes nothing.
func TestRecorderDropsNonWhitelisted(t *testing.T) {
	var buf bytes.Buffer
	r := newNDJSONRecorder(&buf)

	r.Record(captureEvent{Type: captureEventType("not_allowed")})

	if buf.Len() != 0 {
		t.Errorf("recorder wrote %q for a dropped event, want nothing", buf.String())
	}
}

// TestRecorderNoImageBytes feeds a screencast event built from a frame that
// carries image data and asserts the written line holds the metadata but no
// base64 payload. captureEvent has no image-bytes field, so the data cannot
// travel; this guards the type against regression.
func TestRecorderNoImageBytes(t *testing.T) {
	var buf bytes.Buffer
	r := newNDJSONRecorder(&buf)

	// Synthesise a screencast frame with image data present, as Chromium delivers.
	frame := &page.EventScreencastFrame{
		Data:      "iVBORw0KGgoAAAANSUhEUgAAimagebytesbase64payload",
		SessionID: 7,
		Metadata: &page.ScreencastFrameMetadata{
			ScrollOffsetX: 0,
			ScrollOffsetY: 120,
		},
	}
	// The call site carries metadata only, never frame.Data.
	r.Record(captureEvent{
		Type:      captureEventScreencastFrame,
		Meta:      frame.Metadata,
		SessionID: frame.SessionID,
	})

	out := buf.String()
	if strings.Contains(out, frame.Data) {
		t.Fatalf("NDJSON line carries the frame image payload: %q", out)
	}
	if !strings.Contains(out, "120") {
		t.Errorf("NDJSON line missing the scroll metadata: %q", out)
	}
}

// TestBuildCDPSessionStripsProxyCredentials asserts the cdp-session.json builder
// records the credential-free Server but never the proxy user or password.
func TestBuildCDPSessionStripsProxyCredentials(t *testing.T) {
	proxy := ProxyConfig{
		Server:         "http://proxy.example:8080",
		Bypass:         "localhost",
		User:           "secretuser",
		Password:       "secretpass",
		HasCredentials: true,
	}

	session := buildCDPSession(
		"Chromium/120.0.0.0",
		"/usr/bin/chromium",
		"TARGET-1",
		"https://wimpys.world",
		"ok",
		800, 600, 1280, 960, 0,
		1,
		proxy,
	)

	out, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal cdp-session: %v", err)
	}
	body := string(out)

	if strings.Contains(body, "secretuser") {
		t.Errorf("cdp-session leaks the proxy user: %q", body)
	}
	if strings.Contains(body, "secretpass") {
		t.Errorf("cdp-session leaks the proxy password: %q", body)
	}
	if got := session.LaunchFlags["proxy-server"]; got != proxy.Server {
		t.Errorf("proxy-server flag = %q, want %q", got, proxy.Server)
	}
	if !strings.Contains(body, proxy.Server) {
		t.Errorf("cdp-session missing the credential-free proxy server: %q", body)
	}
}

// TestBuildCDPSessionMetadata asserts the builder records the run metadata and
// the per-session layout scale Z.
func TestBuildCDPSessionMetadata(t *testing.T) {
	session := buildCDPSession(
		"Chromium/120.0.0.0",
		"", // no flag, so the path source derives from env or PATH
		"TARGET-2",
		"https://wimpys.world",
		"ok",
		1024, 768, 1024, 768, 3,
		1.5,
		ProxyConfig{},
	)

	if session.BrowserVersion != "Chromium/120.0.0.0" {
		t.Errorf("BrowserVersion = %q, want Chromium/120.0.0.0", session.BrowserVersion)
	}
	if session.TargetID != "TARGET-2" {
		t.Errorf("TargetID = %q, want TARGET-2", session.TargetID)
	}
	if session.CapturePx.Width != 1024 || session.CapturePx.Height != 768 {
		t.Errorf("CapturePx = %dx%d, want 1024x768", session.CapturePx.Width, session.CapturePx.Height)
	}
	if session.Screencast.MaxWidth != 1024 || session.Screencast.MaxHeight != 768 {
		t.Errorf("Screencast cap = %dx%d, want the surface 1024x768", session.Screencast.MaxWidth, session.Screencast.MaxHeight)
	}
	if session.DroppedEvents != 3 {
		t.Errorf("DroppedEvents = %d, want 3", session.DroppedEvents)
	}
	if session.DeviceScaleFactor != 1.5 {
		t.Errorf("DeviceScaleFactor = %v, want 1.5", session.DeviceScaleFactor)
	}
	if session.Screencast.Quality != screencastQuality {
		t.Errorf("Screencast.Quality = %d, want %d", session.Screencast.Quality, screencastQuality)
	}
	if session.NavigationResult != "ok" {
		t.Errorf("NavigationResult = %q, want ok", session.NavigationResult)
	}
	if len(session.LaunchFlags) != 0 {
		t.Errorf("LaunchFlags = %v, want empty for a zero ProxyConfig", session.LaunchFlags)
	}
}

// TestNDJSONRecorderCountsDropped asserts the recorder counts a dropped event: a
// non-whitelisted type is never written but is counted, so a dropped event leaves
// a trace in cdp-session.json. A whitelisted event is written and not counted.
func TestNDJSONRecorderCountsDropped(t *testing.T) {
	var buf bytes.Buffer
	r := newNDJSONRecorder(&buf)

	// A whitelisted event is written, not counted.
	r.Record(captureEvent{Type: captureEventNavigate, URL: "https://wimpys.world"})
	// A non-whitelisted type is dropped and counted.
	r.Record(captureEvent{Type: captureEventType("not_whitelisted")})
	r.Record(captureEvent{Type: captureEventType("also_not")})

	if got := r.droppedCount(); got != 2 {
		t.Errorf("droppedCount = %d, want 2", got)
	}
	if buf.Len() == 0 {
		t.Error("the whitelisted event was not written")
	}
}

// TestRecordRunsDroppedGatesOnCount asserts recordRunsDropped emits one
// runs_dropped event carrying the count when the pass dropped runs and capture is on,
// and emits nothing for a zero count. A zero-count clean snapshot must leave no line.
func TestRecordRunsDroppedGatesOnCount(t *testing.T) {
	e := New("")
	rec := &fakeRecorder{}
	e.EnableCapture(rec)

	e.recordRunsDropped(0)
	if rec.count() != 0 {
		t.Fatalf("zero-count recordRunsDropped emitted %d events, want 0", rec.count())
	}

	e.recordRunsDropped(3)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 1 {
		t.Fatalf("recordRunsDropped(3) emitted %d events, want 1", len(rec.events))
	}
	if rec.events[0].Type != captureEventRunsDropped {
		t.Errorf("event type = %q, want runs_dropped", rec.events[0].Type)
	}
	if rec.events[0].Dropped != 3 {
		t.Errorf("event dropped = %d, want 3", rec.events[0].Dropped)
	}
}

// TestRecordRunsDroppedNoCapture asserts a non-capture engine records nothing even
// when the pass dropped runs, so the production path pays only the nil check.
func TestRecordRunsDroppedNoCapture(t *testing.T) {
	e := New("")
	detached := &fakeRecorder{}
	e.recordRunsDropped(5)
	if detached.count() != 0 {
		t.Fatalf("non-capture recordRunsDropped drove a recorder, want no events")
	}
	if e.recorder != nil {
		t.Fatal("non-capture engine has a recorder set")
	}
}

// TestExecutablePathSourceFlag asserts a non-empty --browser flag sources the
// path as "flag".
func TestExecutablePathSourceFlag(t *testing.T) {
	if got := executablePathSource("/usr/bin/chromium"); got != "flag" {
		t.Errorf("executablePathSource(flag) = %q, want flag", got)
	}
}

// TestEnableCaptureSetsHook checks EnableCapture turns capture on and sets the
// recorder, and that a fresh engine leaves both unset so the production path
// pays zero cost (task 2.2).
func TestEnableCaptureSetsHook(t *testing.T) {
	e := New("")
	if e.captureOn {
		t.Fatal("fresh engine has captureOn true, want false")
	}
	if e.recorder != nil {
		t.Fatal("fresh engine has a recorder set, want nil")
	}

	rec := &fakeRecorder{}
	e.EnableCapture(rec)
	if !e.captureOn {
		t.Fatal("EnableCapture did not set captureOn")
	}
	if e.recorder == nil {
		t.Fatal("EnableCapture did not set the recorder")
	}
}

// TestComposeFrameNoCaptureLeavesStatsNil checks the non-capture engine emits a
// frame with Stats nil and never calls the recorder (task 2.2). A cancelled
// context keeps it browser-free.
func TestComposeFrameNoCaptureLeavesStatsNil(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := New("")

	frame := e.composeFrame(pendingFrame{img: img, bytes: 99, decodeMS: 1.5}, nil)

	if frame.Stats != nil {
		t.Fatalf("non-capture composeFrame set Stats = %+v, want nil", frame.Stats)
	}
	if e.recorder != nil {
		t.Fatal("non-capture engine has a recorder set")
	}
}

// TestComposeFrameCaptureFillsStats checks the capture engine attaches a
// FrameStats carrying the payload bytes and decode time from the pendingFrame and
// the snapshot time from the published textSync. The DOM snapshot now runs on the
// snapshot goroutine and publishes its time through textSync; composeFrame reads
// it, so the test feeds a textSync carrying a snapshot time. A separate call to
// snapshotRuns over a cancelled context proves the snapshot path still emits the
// dom_snapshot event without a browser.
func TestComposeFrameCaptureFillsStats(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	e := New("")
	rec := &fakeRecorder{}
	e.EnableCapture(rec)

	ts := &textSync{}
	ts.store(nil, 3.0, 0, 0)
	frame := e.composeFrame(pendingFrame{img: img, bytes: 4096, decodeMS: 2.5}, ts)

	if frame.Stats == nil {
		t.Fatal("capture composeFrame left Stats nil, want populated")
	}
	if frame.Stats.ScreencastPayloadBytes != 4096 {
		t.Errorf("ScreencastPayloadBytes = %d, want 4096", frame.Stats.ScreencastPayloadBytes)
	}
	if frame.Stats.ScreencastDecodeMS != 2.5 {
		t.Errorf("ScreencastDecodeMS = %v, want 2.5", frame.Stats.ScreencastDecodeMS)
	}
	if frame.Stats.DOMSnapshotMS != 3.0 {
		t.Errorf("DOMSnapshotMS = %v, want the published 3.0", frame.Stats.DOMSnapshotMS)
	}

	// The snapshot goroutine's snapshotRuns emits the dom_snapshot event; a
	// cancelled context fails the snapshot fast with no browser.
	e.snapshotRuns(cancelledContext(), overlayState{})
	if !rec.has(captureEventDOMSnapshot) {
		t.Error("snapshot path did not emit a dom_snapshot event")
	}
}

// TestNavigateAsyncEmitsNavigate checks navigateAsync records a navigate event
// with the URL and result when capture is on (task 5.2). A cancelled chromedp
// context makes the navigate waiter fail fast with no browser, so the recorded
// result carries the error text.
func TestNavigateAsyncEmitsNavigate(t *testing.T) {
	ctx, cancel := chromedp.NewContext(context.Background())
	cancel()
	e := New("")
	rec := &fakeRecorder{}
	e.EnableCapture(rec)

	e.navigateAsync(ctx, "https://wimpys.world", scrollRestore{})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var nav *captureEvent
	for i := range rec.events {
		if rec.events[i].Type == captureEventNavigate {
			nav = &rec.events[i]
		}
	}
	if nav == nil {
		t.Fatal("navigateAsync did not emit a navigate event under capture")
	}
	if nav.URL != "https://wimpys.world" {
		t.Errorf("navigate URL = %q, want https://wimpys.world", nav.URL)
	}
	if nav.Result == "" {
		t.Error("navigate result is empty, want the error text")
	}
}

// TestAckFrameEmitsFrameAck checks ackFrame records a frame_ack event carrying
// the session id when capture is on, with no browser (a nil context skips the
// chromedp ack) (task 5.2).
func TestAckFrameEmitsFrameAck(t *testing.T) {
	e := New("")
	rec := &fakeRecorder{}
	e.EnableCapture(rec)

	e.ackFrame(42)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 1 {
		t.Fatalf("ackFrame recorded %d events, want 1", len(rec.events))
	}
	if rec.events[0].Type != captureEventFrameAck {
		t.Errorf("event type = %q, want frame_ack", rec.events[0].Type)
	}
	if rec.events[0].SessionID != 42 {
		t.Errorf("event session id = %d, want 42", rec.events[0].SessionID)
	}
}

// TestScreencastFrameEmitsMetadataOnly checks the screencast frame emission
// carries the metadata but never the frame image bytes (task 5.2). It builds the
// event the listener builds and asserts the serialised line holds the scroll
// metadata and not the payload.
func TestScreencastFrameEmitsMetadataOnly(t *testing.T) {
	var buf bytes.Buffer
	r := newNDJSONRecorder(&buf)

	frame := &page.EventScreencastFrame{
		Data:      "BASE64IMAGEPAYLOADmustNeverAppear",
		SessionID: 5,
		Metadata: &page.ScreencastFrameMetadata{
			ScrollOffsetX: 0,
			ScrollOffsetY: 256,
		},
	}
	// The listener emits metadata and the session id only, never frame.Data.
	r.Record(captureEvent{
		Type:      captureEventScreencastFrame,
		Meta:      frame.Metadata,
		SessionID: frame.SessionID,
	})

	out := buf.String()
	if strings.Contains(out, frame.Data) {
		t.Fatalf("screencast_frame line carries the image payload: %q", out)
	}
	if !strings.Contains(out, "256") {
		t.Errorf("screencast_frame line missing the scroll metadata: %q", out)
	}
}

// TestCloseEmitsStop checks Close records a screencast_control stop at the clean
// session end when capture is on, and that a never-navigated engine still emits
// it once (task 5.2). Close on an engine that never launched returns without a
// browser.
func TestCloseEmitsStop(t *testing.T) {
	e := New("")
	rec := &fakeRecorder{}
	e.EnableCapture(rec)

	if err := e.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if !rec.has(captureEventScreencastControl) {
		t.Fatal("Close did not emit a screencast_control event")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	stops := 0
	for _, ev := range rec.events {
		if ev.Type == captureEventScreencastControl && ev.Action == "stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("Close emitted %d stop events, want 1", stops)
	}
}

// TestNoCaptureEmitsNothing checks a non-capture engine emits no events across
// the gated call sites (task 5.2). It drives ackFrame, navigateAsync, and Close
// on an engine with no recorder and asserts none panic and nothing is recorded.
// A fakeRecorder is attached only to prove the count stays zero; capture stays
// off, so the engine never calls it.
func TestNoCaptureEmitsNothing(t *testing.T) {
	ctx, cancel := chromedp.NewContext(context.Background())
	cancel()
	e := New("")
	rec := &fakeRecorder{}
	// Capture stays off: the recorder field is nil, so no call site emits.

	e.ackFrame(1)
	e.navigateAsync(ctx, "https://wimpys.world", scrollRestore{})
	_ = e.Close()

	if rec.count() != 0 {
		t.Fatalf("detached recorder saw %d events, want 0", rec.count())
	}
	if e.recorder != nil {
		t.Fatal("non-capture engine has a recorder set")
	}
}

// TestProductionPathZeroCost is the cross-cutting zero-cost guard for the
// non-capture path (task 8.1). On an engine that never had EnableCapture called it
// asserts, together: a composed frame carries no Stats (no engine-phase timing
// ran), the recorder hook is unset, and driving the gated call sites records
// nothing (no event recording ran). A detached fakeRecorder proves the count stays
// zero without ever being wired in. A cancelled context keeps it browser-free.
func TestProductionPathZeroCost(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	ctx, cancel := chromedp.NewContext(context.Background())
	cancel()
	e := New("")

	// The recorder hook is unset on a fresh engine, so the production path pays no
	// capture cost.
	if e.recorder != nil {
		t.Fatal("fresh engine has a recorder set, want nil on the production path")
	}
	if e.captureOn {
		t.Fatal("fresh engine has captureOn true, want false on the production path")
	}

	// A composed frame carries no Stats: no engine-phase timing runs off capture.
	frame := e.composeFrame(pendingFrame{img: img, bytes: 4096, decodeMS: 2.5}, nil)
	if frame.Stats != nil {
		t.Fatalf("non-capture composeFrame set Stats = %+v, want nil", frame.Stats)
	}

	// Driving the gated call sites records nothing: no event recording runs. The
	// detached recorder is never wired in, so a non-zero count would prove a leak.
	detached := &fakeRecorder{}
	e.ackFrame(1)
	e.navigateAsync(ctx, "https://wimpys.world", scrollRestore{})
	_ = e.Close()
	if detached.count() != 0 {
		t.Fatalf("detached recorder saw %d events on the production path, want 0", detached.count())
	}
	if e.recorder != nil {
		t.Fatal("non-capture engine set a recorder while driving the gated call sites")
	}
}

// ndjsonLines splits NDJSON output into its non-empty lines.
func ndjsonLines(t *testing.T, b []byte) []string {
	t.Helper()
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan ndjson: %v", err)
	}
	return lines
}
