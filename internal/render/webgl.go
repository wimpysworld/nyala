package render

// WebGLTier is the highest WebGL context version the engine could create in
// the page. Context creation counts as support: the probe asks Chromium for a
// context and records which version answered. The tier never claims hardware
// acceleration, because a software rasteriser creates a context the same way.
type WebGLTier int

const (
	// WebGLUnknown reports that the probe did not run or failed. About shows
	// "?".
	WebGLUnknown WebGLTier = iota
	// WebGLUnavailable reports that no context could be created. About shows
	// "✗".
	WebGLUnavailable
	// WebGL1 reports that a WebGL 1 context was created. About shows "➀".
	WebGL1
	// WebGL2 reports that a WebGL 2 context was created. About shows "➁".
	WebGL2
)

// String returns the tier name for logs and tests: "unknown", "unavailable",
// "webgl1", or "webgl2". An out-of-range value reports "unknown".
func (t WebGLTier) String() string {
	switch t {
	case WebGLUnavailable:
		return "unavailable"
	case WebGL1:
		return "webgl1"
	case WebGL2:
		return "webgl2"
	default:
		return "unknown"
	}
}

// WebGLState is the WebGL support snapshot the engine probes over CDP and the
// viewer shows in the About bar. It records whether a context could be
// created and which version, not whether the GPU accelerates it.
type WebGLState struct {
	// Tier is the highest WebGL context version the probe created.
	Tier WebGLTier
	// Label is compact vendor or renderer text, already control-stripped, or
	// "" when no text is safe to show.
	Label string
}
