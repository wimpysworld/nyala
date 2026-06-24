package engine

import (
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/chromedp/cdproto/page"
)

// captureEventType is the discriminator for one recorded CDP capture event. Only
// the whitelisted values below reach the event stream; any other value is
// dropped by the recorder so unintended event types never land in the bundle.
type captureEventType string

const (
	// captureEventScreencastFrame is one screencast frame's metadata. The image
	// bytes are never carried: only the metadata travels, so the stream holds no
	// base64 payload.
	captureEventScreencastFrame captureEventType = "screencast_frame"
	// captureEventTargetCrashed marks the page target crashing.
	captureEventTargetCrashed captureEventType = "target_crashed"
	// captureEventNavigate marks a navigation dispatch and its result.
	captureEventNavigate captureEventType = "navigate"
	// captureEventDOMSnapshot marks a DOM snapshot capture.
	captureEventDOMSnapshot captureEventType = "dom_snapshot"
	// captureEventScreencastControl marks a screencast lifecycle change: start,
	// stop, or restart. The Action field names which.
	captureEventScreencastControl captureEventType = "screencast_control"
	// captureEventFrameAck marks one screencast frame acknowledgement.
	captureEventFrameAck captureEventType = "frame_ack"
)

// captureWhitelist is the set of event types the recorder accepts. A type absent
// here is dropped, never written.
var captureWhitelist = map[captureEventType]bool{
	captureEventScreencastFrame:   true,
	captureEventTargetCrashed:     true,
	captureEventNavigate:          true,
	captureEventDOMSnapshot:       true,
	captureEventScreencastControl: true,
	captureEventFrameAck:          true,
}

// captureEvent is one event offered to the recorder. Type is the discriminator;
// the optional fields carry the per-event metadata. Image bytes are never a
// field, so the frame payload cannot enter the stream. Meta carries the
// screencast frame metadata for a screencast_frame event and is nil otherwise.
type captureEvent struct {
	// Type is the event discriminator. The recorder drops any event whose Type is
	// not whitelisted.
	Type captureEventType `json:"type"`
	// Meta is the screencast frame metadata for a screencast_frame event. It
	// carries scroll offset and page scale only, never the frame image bytes.
	Meta *page.ScreencastFrameMetadata `json:"meta,omitempty"`
	// Action names the screencast_control change: "start", "stop", or "restart".
	Action string `json:"action,omitempty"`
	// URL is the navigation target for a navigate event.
	URL string `json:"url,omitempty"`
	// Result is the navigation outcome for a navigate event: "ok" or the error
	// text.
	Result string `json:"result,omitempty"`
	// SessionID is the screencast frame session id for a screencast_frame or
	// frame_ack event.
	SessionID int64 `json:"session_id,omitempty"`
}

// captureRecorder is the one-method sink the engine emits capture events through.
// Production leaves it nil, so the call sites pay zero cost behind a nil check,
// mirroring the enableProxyAuth no-op-when-unset gating. The capture path sets a
// concrete recorder that serialises NDJSON.
type captureRecorder interface {
	// Record offers one event to the recorder. The recorder drops any event whose
	// Type is not whitelisted; a whitelisted event is appended as one NDJSON line.
	Record(event captureEvent)
}

// ndjsonRecorder writes each accepted captureEvent as one NDJSON line to an
// io.Writer. It drops any non-whitelisted event. It is safe for concurrent use:
// the engine emits events from the listener goroutine and the supervisor
// goroutine, so each Record holds mu across the single write.
type ndjsonRecorder struct {
	mu sync.Mutex
	w  io.Writer
}

// Ensure ndjsonRecorder satisfies captureRecorder.
var _ captureRecorder = (*ndjsonRecorder)(nil)

// newNDJSONRecorder returns a recorder that appends each accepted event as one
// NDJSON line to w. The caller owns w and closes it after capture finishes.
func newNDJSONRecorder(w io.Writer) *ndjsonRecorder {
	return &ndjsonRecorder{w: w}
}

// Record appends event as one NDJSON line when its Type is whitelisted, and
// drops it otherwise. A screencast_frame event carries metadata only, so the
// written line never holds the frame image bytes. A marshal error is dropped so
// one bad event cannot stall the stream.
func (r *ndjsonRecorder) Record(event captureEvent) {
	if !captureWhitelist[event.Type] {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	line = append(line, '\n')
	r.mu.Lock()
	_, _ = r.w.Write(line)
	r.mu.Unlock()
}

// Recorder is the exported capture sink the viewer wires into one engine run. It
// wraps the internal NDJSON event recorder and also holds the cdp-session.json
// bytes the engine assembles after navigation. It is the single seam for
// engine-to-capture coupling: the viewer constructs it over the bundle's
// cdp-events.ndjson sink with NewCaptureRecorder, hands it to EnableCapture
// before Navigate, then reads SessionJSON after Navigate to write
// cdp-session.json into the bundle. It is safe for concurrent use.
type Recorder struct {
	events *ndjsonRecorder

	mu      sync.Mutex
	session []byte
}

// Ensure Recorder satisfies captureRecorder.
var _ captureRecorder = (*Recorder)(nil)

// NewCaptureRecorder returns a Recorder that appends each accepted capture event
// as one NDJSON line to ndjson, the bundle's cdp-events.ndjson sink. The caller
// owns ndjson and closes it after capture finishes. The returned Recorder holds
// no session bytes until the engine fills them during navigation; read them with
// SessionJSON after Navigate returns.
func NewCaptureRecorder(ndjson io.Writer) *Recorder {
	return &Recorder{events: newNDJSONRecorder(ndjson)}
}

// Record forwards event to the wrapped NDJSON recorder, which drops any
// non-whitelisted event and appends a whitelisted one as one line. A
// screencast_frame event carries metadata only, so the written line never holds
// the frame image bytes.
func (r *Recorder) Record(event captureEvent) {
	r.events.Record(event)
}

// setSession stores the cdp-session.json bytes the engine assembled after
// navigation. The engine calls it once from the navigate path; the viewer reads
// the bytes with SessionJSON. It holds mu across the store so the navigate
// goroutine and the viewer goroutine do not race.
func (r *Recorder) setSession(data []byte) {
	r.mu.Lock()
	r.session = data
	r.mu.Unlock()
}

// SessionJSON returns the cdp-session.json bytes the engine assembled during
// navigation, or nil if navigation has not filled them. The viewer reads it
// after Navigate returns and writes it into the bundle. The returned slice is
// the stored bytes; the caller does not mutate them.
func (r *Recorder) SessionJSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

// cdpSession is the run-metadata view serialised to cdp-session.json. It records
// the browser version, the executable path source, the target id, the capture
// pixel size, the device scale factor (pinned 1), the screencast config, the
// navigation URL and result, and the launch flags. The launch flags are
// credential-free: they derive from proxyFlags, which carries only the
// credential-free Server, so the proxy user and password never reach this file.
type cdpSession struct {
	// BrowserVersion is the product version string Chromium reported at launch.
	BrowserVersion string `json:"browser_version"`
	// ExecutablePathSource names where the executable path came from: "flag",
	// "env", or "path".
	ExecutablePathSource string `json:"executable_path_source"`
	// TargetID is the CDP target id of the driven page.
	TargetID string `json:"target_id"`
	// CapturePx is the source-pixel capture extent for the active backend.
	CapturePx cdpSessionPx `json:"capture_px"`
	// DeviceScaleFactor is the pinned device scale factor; the engine pins it to 1.
	DeviceScaleFactor float64 `json:"device_scale_factor"`
	// Screencast is the screencast configuration the engine started.
	Screencast cdpSessionScreencast `json:"screencast"`
	// NavigationURL is the URL the engine navigated to.
	NavigationURL string `json:"navigation_url"`
	// NavigationResult is the navigation outcome: "ok" or the error text.
	NavigationResult string `json:"navigation_result"`
	// LaunchFlags is the credential-free launch flag set, name to value, derived
	// from proxyFlags. It never carries the proxy user or password.
	LaunchFlags map[string]string `json:"launch_flags"`
}

// cdpSessionPx is a pixel extent serialised inside cdpSession.
type cdpSessionPx struct {
	// Width is the capture width in source pixels.
	Width int `json:"width"`
	// Height is the capture height in source pixels.
	Height int `json:"height"`
}

// cdpSessionScreencast is the screencast configuration serialised inside
// cdpSession.
type cdpSessionScreencast struct {
	// Format is the screencast image format.
	Format string `json:"format"`
	// Quality is the JPEG compression quality.
	Quality int `json:"quality"`
	// MaxWidth is the screencast max width in source pixels.
	MaxWidth int `json:"max_width"`
	// MaxHeight is the screencast max height in source pixels.
	MaxHeight int `json:"max_height"`
}

// executablePathSource names where resolveExecutable took the executable path
// from, given the --browser flag value and the environment. It mirrors
// resolveExecutable's precedence: a non-empty flag is "flag", then the
// NYALA_BROWSER or CHROME_PATH environment variable is "env", else a PATH lookup
// is "path". It reads the environment but launches nothing.
func executablePathSource(flag string) string {
	if flag != "" {
		return "flag"
	}
	if v := os.Getenv("NYALA_BROWSER"); v != "" {
		return "env"
	}
	if v := os.Getenv("CHROME_PATH"); v != "" {
		return "env"
	}
	return "path"
}

// buildCDPSession assembles the cdp-session.json view from the run metadata. It
// derives the launch flags from proxyFlags so the credential-free Server is
// recorded and the proxy user and password are never included. The browser
// version, target id, navigation URL, and navigation result come from the live
// session; the capture size and screencast config come from engine geometry.
func buildCDPSession(
	browserVersion, browserFlag, targetID, navURL, navResult string,
	capW, capH int,
	proxy ProxyConfig,
) cdpSession {
	flags := map[string]string{}
	for _, pair := range proxyFlags(proxy) {
		flags[pair[0]] = pair[1]
	}
	return cdpSession{
		BrowserVersion:       browserVersion,
		ExecutablePathSource: executablePathSource(browserFlag),
		TargetID:             targetID,
		CapturePx:            cdpSessionPx{Width: capW, Height: capH},
		DeviceScaleFactor:    deviceScaleFactor,
		Screencast: cdpSessionScreencast{
			Format:    string(page.ScreencastFormatJpeg),
			Quality:   screencastQuality,
			MaxWidth:  capW,
			MaxHeight: capH,
		},
		NavigationURL:    navURL,
		NavigationResult: navResult,
		LaunchFlags:      flags,
	}
}
