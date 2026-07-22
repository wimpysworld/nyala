package viewer

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/kitty"
)

// shmState returns a Kitty-backend state resolved onto the shm transport, the
// setup every shm-path test starts from.
func shmState() State {
	s := NewState(8, 4)
	s.Backend = BackendKitty
	s.kittyTransport = KittyTransportSHM
	return s
}

// shmNameFromEscape base64-decodes the first APC envelope's payload, which on
// the t=s path is the POSIX object name the terminal reads.
func shmNameFromEscape(t *testing.T, out string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(out, "\x1b_G")
	if !ok {
		t.Fatalf("output %q does not start with a Kitty APC", out)
	}
	body, _, ok := strings.Cut(rest, "\x1b\\")
	if !ok {
		t.Fatal("APC envelope missing its String Terminator")
	}
	_, payload, ok := strings.Cut(body, ";")
	if !ok {
		t.Fatal("APC envelope missing the options/payload separator")
	}
	name, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode shm object name: %v", err)
	}
	return string(name)
}

// renderKittySHMFrame draws one plane frame to a fresh buffer and returns the
// raw bytes plus the APC option strings, mirroring renderKittyFrame for the
// shm path.
func renderKittySHMFrame(t *testing.T, s *State, img *image.RGBA, release func()) (string, []string) {
	t.Helper()
	var buf bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: img, Release: release}, &buf); err != nil {
		t.Fatalf("render kitty shm frame: %v", err)
	}
	return buf.String(), kittyCommands(buf.String())
}

// TestKittySHMTransmitAlternatesIDs asserts the shm path emits the single
// unchunked f=32,t=s escape, alternates the double-buffer image id with the
// delete-previous ordering, releases each plane after the copy, and unlinks
// unconsumed /dev/shm objects on reset.
func TestKittySHMTransmitAlternatesIDs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("kitty shm transport is Linux-only")
	}
	s := shmState()
	first := solidImageSize(6, 4, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	second := solidImageSize(6, 4, color.RGBA{R: 0xc0, G: 0x40, B: 0x80, A: 0xff})

	released := 0
	out1, f1 := renderKittySHMFrame(t, &s, first, func() { released++ })
	if released != 1 {
		t.Fatalf("frame 1 released the plane %d times, want 1", released)
	}
	if len(f1) != 1 {
		t.Fatalf("frame 1 emitted %d commands, want 1 (transmit only)", len(f1))
	}
	assertTransmit(t, f1[0], "1")
	for _, opt := range []string{"f=32", "t=s", "s=6", "v=4", "q=0"} {
		if !hasOpt(f1[0], opt) {
			t.Errorf("shm transmit %q missing %s", f1[0], opt)
		}
	}
	if strings.Contains(f1[0], "m=") {
		t.Fatalf("shm transmit %q is chunked, want a single unchunked escape", f1[0])
	}
	name1 := shmNameFromEscape(t, out1)
	pix, err := os.ReadFile("/dev/shm/" + strings.TrimPrefix(name1, "/"))
	if err != nil {
		t.Fatalf("read shm object: %v", err)
	}
	if !bytes.Equal(pix, first.Pix) {
		t.Fatal("shm object bytes differ from the RGBA plane")
	}

	out2, f2 := renderKittySHMFrame(t, &s, second, nil)
	if len(f2) != 2 {
		t.Fatalf("frame 2 emitted %d commands, want 2 (transmit then delete)", len(f2))
	}
	assertTransmit(t, f2[0], "2")
	assertDelete(t, f2[1], "1")
	name2 := shmNameFromEscape(t, out2)

	// No terminal consumed the objects, so both still exist until the reset
	// closes the writer and unlinks them.
	var reset bytes.Buffer
	if err := s.resetKitty(&reset); err != nil {
		t.Fatalf("resetKitty: %v", err)
	}
	for _, name := range []string{name1, name2} {
		if _, err := os.Stat("/dev/shm/" + strings.TrimPrefix(name, "/")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("shm object %s outlived resetKitty (stat err %v)", name, err)
		}
	}
}

// failSHMWriter is a kittySHMWriter stub whose Create always fails, forcing
// the runtime demotion path without a broken /dev/shm.
type failSHMWriter struct{ closed bool }

func (w *failSHMWriter) Create(int, int) (*kitty.SHMObject, error) {
	return nil, errors.New("forced shm failure")
}

func (w *failSHMWriter) Close() error {
	w.closed = true
	return nil
}

// TestKittySHMDemotesOnCreateFailure asserts an shm create failure demotes the
// stint to the direct path: the failing plane-only frame drops cleanly with
// its plane released, the demotion error surfaces exactly once, and the next
// retained-payload frame paints via direct PNG.
func TestKittySHMDemotesOnCreateFailure(t *testing.T) {
	s := shmState()
	stub := &failSHMWriter{}
	s.kittySHM = stub

	released := 0
	var buf bytes.Buffer
	frame := render.Frame{Img: solidImageSize(6, 4, color.RGBA{A: 0xff}), Release: func() { released++ }}
	if err := s.RenderStreamFrame(frame, &buf); err != nil {
		t.Fatalf("failing shm frame must demote, not error: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("dropped plane-only frame wrote %d bytes, want 0", buf.Len())
	}
	if released != 1 {
		t.Fatalf("dropped frame released the plane %d times, want 1", released)
	}
	if s.kittySHMLive() {
		t.Fatal("shm transport still live after a create failure")
	}
	if !stub.closed {
		t.Fatal("demotion did not close the shm writer")
	}
	if err := s.takeKittySHMDemotion(); err == nil {
		t.Fatal("demotion error not recorded for the one-shot log")
	}
	if err := s.takeKittySHMDemotion(); err != nil {
		t.Fatalf("demotion error surfaced twice: %v", err)
	}

	direct := renderKittyFrame(t, &s, solidImage(color.RGBA{A: 0xff}))
	if len(direct) != 1 {
		t.Fatalf("post-demotion direct frame emitted %d commands, want 1", len(direct))
	}
	assertTransmit(t, direct[0], "1")
	if !hasOpt(direct[0], "f=100") || !hasOpt(direct[0], "t=d") {
		t.Fatalf("post-demotion frame %q is not a direct PNG transmit", direct[0])
	}
}

// TestKittySHMEncodedPNGFallsBackDirect asserts a retained-payload frame that
// arrives while the shm transport is live (a mode-change race) paints via the
// direct encode for that frame instead of erroring or dropping.
func TestKittySHMEncodedPNGFallsBackDirect(t *testing.T) {
	s := shmState()
	cmds := renderKittyFrame(t, &s, solidImage(color.RGBA{A: 0xff}))
	if len(cmds) != 1 {
		t.Fatalf("payload frame in shm mode emitted %d commands, want 1", len(cmds))
	}
	assertTransmit(t, cmds[0], "1")
	if !hasOpt(cmds[0], "t=d") {
		t.Fatalf("payload frame in shm mode %q did not take the direct path", cmds[0])
	}
	if s.takeKittySHMWrote() {
		t.Fatal("direct fallback frame flagged as an shm transmit")
	}
	if !s.kittySHMLive() {
		t.Fatal("direct fallback for one frame must not demote the transport")
	}
}

// TestNextEventAPCResponse asserts the key reader turns a Kitty graphics APC
// response into an apc event, drops non-graphics APCs, and keeps later keys
// intact.
func TestNextEventAPCResponse(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\x1b_Gi=1;OK\x1b\\\x1b_Xother\x1b\\q"))
	first, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	ev, emit := nextEvent(br, first)
	if !emit || ev.apc != "i=1;OK" {
		t.Fatalf("graphics APC event = %+v emit %v, want apc %q", ev, emit, "i=1;OK")
	}

	next, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read next byte: %v", err)
	}
	if _, emit := nextEvent(br, next); emit {
		t.Fatal("non-graphics APC must be dropped")
	}

	last, err := br.ReadByte()
	if err != nil {
		t.Fatalf("read trailing byte: %v", err)
	}
	ev, emit = nextEvent(br, last)
	if !emit || ev.b != 'q' || ev.apc != "" {
		t.Fatalf("trailing key = %+v emit %v, want byte q", ev, emit)
	}
}

// TestDrainFramesReleasesStale asserts the latest-wins coalesce returns each
// superseded frame's pooled plane to the engine and keeps only the newest.
func TestDrainFramesReleasesStale(t *testing.T) {
	frames := make(chan render.Frame, 2)
	released := make([]int, 3)
	mk := func(i int) render.Frame {
		return render.Frame{
			Img:     image.NewRGBA(image.Rect(0, 0, 1, 1)),
			Release: func() { released[i]++ },
		}
	}
	frames <- mk(1)
	frames <- mk(2)

	latest, ok := drainFrames(frames, mk(0))
	if !ok {
		t.Fatal("drainFrames reported a closed channel")
	}
	if released[0] != 1 || released[1] != 1 {
		t.Fatalf("superseded frames released %v times, want 1 each", released[:2])
	}
	if released[2] != 0 {
		t.Fatal("the kept newest frame was released")
	}
	latest.Release()
	if released[2] != 1 {
		t.Fatal("kept frame's release hook is not the newest frame's")
	}
}

// TestKittySHMPacingGate asserts the one-in-flight gate arms on an shm
// handoff, opens on any terminal response or its deadline, and demotes on a
// failure response.
func TestKittySHMPacingGate(t *testing.T) {
	st := shmState()
	s := &streamSession{st: &st, eng: &fakeEngine{}}

	if s.kittySHMGated() {
		t.Fatal("gate armed before any shm handoff")
	}
	s.assembledSHM = true
	s.markKittySHMHandoff()
	if !s.kittySHMGated() {
		t.Fatal("gate not armed after an shm handoff")
	}
	s.handleKittyResponse("i=1;OK")
	if s.kittySHMGated() {
		t.Fatal("gate still armed after the OK response")
	}

	s.assembledSHM = true
	s.markKittySHMHandoff()
	s.kittyShmDeadline = time.Now().Add(-time.Millisecond)
	if s.kittySHMGated() {
		t.Fatal("gate held past its deadline")
	}

	s.assembledSHM = true
	s.markKittySHMHandoff()
	s.handleKittyResponse("i=2;EBADF:could not open shared memory")
	if s.kittySHMGated() {
		t.Fatal("gate still armed after a failure response")
	}
	if st.kittySHMLive() {
		t.Fatal("failure response did not demote the shm transport")
	}
}

// TestKittySHMLateErrorAfterTabAwayDoesNotDemote reproduces the Ghostty
// switch-window race: an shm transmit is in flight when Tab cycles away from
// Kitty, so resetKitty unlinks the object before the busy terminal reads the
// escape, and the terminal's ENOENT reply lands after the switch. That failure
// is self-inflicted, not a transport fault, so it must not demote the shm
// transport; the next Tab back to Kitty must run shm again with the engine
// decode on and the stale pacing gate dropped.
func TestKittySHMLateErrorAfterTabAwayDoesNotDemote(t *testing.T) {
	st := shmState()
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	st.kittyID = 1
	eng := &decodeFakeEngine{}
	done := make(chan struct{})
	defer close(done)
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}

	// One shm transmit awaits its acknowledgement when the user hits Tab.
	s.assembledSHM = true
	s.markKittySHMHandoff()
	s.handleKey(keyEvent{b: keyTab})
	if st.Backend != BackendBlock {
		t.Fatalf("backend after Tab = %v, want Block", st.Backend)
	}
	if s.kittyShmAwait {
		t.Fatal("Tab away from Kitty left the stale shm pacing gate armed")
	}

	// The busy terminal processes the escape after the unlink and replies with
	// a failure. The transport itself is healthy.
	s.handleKittyResponse("i=2;ENOENT:failed to open shared memory object")
	if !st.kittySHMLive() {
		t.Fatal("late failure response after Tab away falsely demoted the shm transport")
	}

	s.handleKey(keyEvent{b: keyTab})
	if st.Backend != BackendKitty {
		t.Fatalf("backend after second Tab = %v, want Kitty", st.Backend)
	}
	if !st.kittySHMLive() {
		t.Fatal("shm transport not live after returning to Kitty")
	}
	if !eng.kittyDecode {
		t.Fatal("engine kitty-shm decode not re-enabled on return to Kitty")
	}
	if s.kittySHMGated() {
		t.Fatal("stale pacing gate held the new Kitty stint's first frame")
	}
}

// TestKittySHMDemotionRearmsOnReturnToKitty asserts a runtime demotion lasts
// one Kitty stint, not the whole session: the startup probe proved the
// transport, so Tab back to Kitty re-arms shm and re-enables the engine
// decode. A genuinely broken transport demotes again on its next failure.
func TestKittySHMDemotionRearmsOnReturnToKitty(t *testing.T) {
	st := shmState()
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	eng := &decodeFakeEngine{}
	done := make(chan struct{})
	defer close(done)
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}

	// A failure response while on Kitty still demotes the stint.
	s.assembledSHM = true
	s.markKittySHMHandoff()
	s.handleKittyResponse("i=1;EBADF:could not open shared memory")
	if st.kittySHMLive() {
		t.Fatal("failure response on Kitty did not demote the shm transport")
	}
	if eng.kittyDecode {
		t.Fatal("demotion did not disable the engine kitty-shm decode")
	}

	s.handleKey(keyEvent{b: keyTab})
	s.handleKey(keyEvent{b: keyTab})
	if st.Backend != BackendKitty {
		t.Fatalf("backend after two Tabs = %v, want Kitty", st.Backend)
	}
	if !st.kittySHMLive() {
		t.Fatal("returning to Kitty did not re-arm the demoted shm transport")
	}
	if !eng.kittyDecode {
		t.Fatal("returning to Kitty did not re-enable the engine kitty-shm decode")
	}
}

// decodeFakeEngine extends the fake engine with the optional kitty-shm decode
// capability, recording the last toggle.
type decodeFakeEngine struct {
	fakeEngine
	kittyDecode bool
}

func (e *decodeFakeEngine) SetKittyDecode(on bool) { e.kittyDecode = on }

// TestSyncEngineKittyDecode asserts the engine decode mode follows the
// backend and transport: on only for Kitty plus a live shm transport, off
// after a cycle away or a demotion, and a no-op for engines without the
// capability.
func TestSyncEngineKittyDecode(t *testing.T) {
	st := shmState()
	eng := &decodeFakeEngine{}

	syncEngineKittyDecode(eng, &st)
	if !eng.kittyDecode {
		t.Fatal("kitty backend with live shm transport must enable the decode")
	}

	st.Backend = BackendBlock
	syncEngineKittyDecode(eng, &st)
	if eng.kittyDecode {
		t.Fatal("leaving kitty must disable the decode")
	}

	st.Backend = BackendKitty
	st.demoteKittySHM(errors.New("forced"))
	syncEngineKittyDecode(eng, &st)
	if eng.kittyDecode {
		t.Fatal("a demoted transport must keep the decode off")
	}

	syncEngineKittyDecode(&fakeEngine{}, &st) // must not panic without the capability
}
