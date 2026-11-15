package viewer

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"io"
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
		t.Skip("kitty shm transport test requires Linux /dev/shm")
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

// testSHMWriter delegates object creation and cleanup to kitty.SHMWriter while
// exposing the viewer-local failure points.
type testSHMWriter struct {
	writer         *kitty.SHMWriter
	createErr      error
	objectCloseErr error
	names          []string
	closed         bool
}

func (w *testSHMWriter) Create(id, size int) (kittySHMObject, error) {
	if w.createErr != nil {
		return nil, w.createErr
	}
	obj, err := w.writer.Create(id, size)
	if err != nil {
		return nil, err
	}
	w.names = append(w.names, obj.Name())
	if w.objectCloseErr != nil {
		return &testSHMObject{kittySHMObject: obj, closeErr: w.objectCloseErr}, nil
	}
	return obj, nil
}

func (w *testSHMWriter) Close() error {
	w.closed = true
	return w.writer.Close()
}

type testSHMObject struct {
	kittySHMObject
	closeErr error
}

func (o *testSHMObject) Close() error {
	return errors.Join(o.kittySHMObject.Close(), o.closeErr)
}

type failKittyAPCWriter struct {
	bytes.Buffer
	err error
}

func (w *failKittyAPCWriter) Write(p []byte) (int, error) {
	_, _ = w.Buffer.Write(p)
	return 0, w.err
}

// skipWithoutKittySHM skips a test that needs a real shm object where the kitty
// package builds its unsupported stub.
func skipWithoutKittySHM(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("kitty shm transport is unsupported on %s", runtime.GOOS)
	}
}

func newTestSHMWriter() *testSHMWriter {
	return &testSHMWriter{writer: kitty.NewSHMWriter()}
}

func assertKittySHMEncodeError(t *testing.T, s *State, shm *testSHMWriter, frame render.Frame, w io.Writer, want error, output func() string) {
	t.Helper()
	released := 0
	frame.Release = func() { released++ }
	err := s.RenderStreamFrame(frame, w)
	if !errors.Is(err, want) {
		t.Fatalf("RenderStreamFrame() error = %v, want %v", err, want)
	}
	if released != 1 {
		t.Fatalf("failed frame released the plane %d times, want 1", released)
	}
	if strings.Contains(output(), "t=d") {
		t.Fatalf("failed shm frame fell through to direct PNG: %q", output())
	}
	if !s.kittySHMLive() {
		t.Fatal("synchronous encode failure changed the selected shm transport")
	}
	if !shm.closed || s.kittySHM != nil {
		t.Fatal("synchronous encode failure did not close the shm writer")
	}
	if runtime.GOOS != "linux" {
		return
	}
	for _, name := range shm.names {
		path := "/dev/shm/" + strings.TrimPrefix(name, "/")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("shm object %s survived failed encode cleanup (stat err %v)", name, err)
		}
	}
}

// TestKittySHMCreateMapErrorReturns asserts a create or map/setup failure is
// returned for a plane-only frame without changing the selected transport.
func TestKittySHMCreateMapErrorReturns(t *testing.T) {
	want := errors.New("forced shm create/map failure")
	s := shmState()
	shm := newTestSHMWriter()
	shm.createErr = want
	s.kittySHM = shm
	var out bytes.Buffer
	frame := render.Frame{Img: solidImageSize(6, 4, color.RGBA{A: 0xff})}
	assertKittySHMEncodeError(t, &s, shm, frame, &out, want, out.String)
}

// TestKittySHMObjectCloseErrorReturns asserts a mapped-object close failure is
// returned and cleanup unlinks the object without selecting retained PNG.
func TestKittySHMObjectCloseErrorReturns(t *testing.T) {
	skipWithoutKittySHM(t)
	want := errors.New("forced shm object close failure")
	s := shmState()
	shm := newTestSHMWriter()
	shm.objectCloseErr = want
	s.kittySHM = shm
	var out bytes.Buffer
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frame := render.Frame{Img: img, EncodedPNG: encodedPNG(t, img)}
	assertKittySHMEncodeError(t, &s, shm, frame, &out, want, out.String)
}

// TestKittySHMAPCEncodeErrorReturns asserts an APC writer failure is returned
// after SHM preparation and cleanup unlinks the unconsumed object.
func TestKittySHMAPCEncodeErrorReturns(t *testing.T) {
	skipWithoutKittySHM(t)
	want := errors.New("forced Kitty APC failure")
	s := shmState()
	shm := newTestSHMWriter()
	s.kittySHM = shm
	out := &failKittyAPCWriter{err: want}
	img := solidImageSize(6, 4, color.RGBA{A: 0xff})
	frame := render.Frame{Img: img, EncodedPNG: encodedPNG(t, img)}
	assertKittySHMEncodeError(t, &s, shm, frame, out, want, out.String)
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
	if id := s.takeKittySHMID(); id != 0 {
		t.Fatal("direct fallback frame flagged as an shm transmit")
	}
	if !s.kittySHMLive() {
		t.Fatal("direct fallback for one frame changed the selected transport")
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

	latest, _, ok := drainFrames(frames, mk(0))
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

// handoffKittySHM records one test transmit in the session's current reset
// generation and runs the production handoff path.
func handoffKittySHM(s *streamSession, id int) {
	s.markKittySHMAssembled(id)
	s.markKittySHMHandoff()
}

// TestKittySHMPacingGate asserts the one-in-flight gate arms on an shm
// handoff, opens on its matching response or deadline, and returns a current
// failure without changing the selected transport.
func TestKittySHMPacingGate(t *testing.T) {
	st := shmState()
	s := &streamSession{st: &st, eng: &fakeEngine{}}

	if s.kittySHMGated() {
		t.Fatal("gate armed before any shm handoff")
	}
	handoffKittySHM(s, 1)
	if !s.kittySHMGated() {
		t.Fatal("gate not armed after an shm handoff")
	}
	if err := s.handleKittyResponse("i=1;OK"); err != nil {
		t.Fatalf("OK response returned error: %v", err)
	}
	if s.kittySHMGated() {
		t.Fatal("gate still armed after the OK response")
	}

	handoffKittySHM(s, 2)
	s.kittyShmDeadline = time.Now().Add(-time.Millisecond)
	if s.kittySHMGated() {
		t.Fatal("gate held past its deadline")
	}

	handoffKittySHM(s, 1)
	err := s.handleKittyResponse("i=1;EBADF:could not open shared memory")
	if err == nil || !strings.Contains(err.Error(), "terminal rejected shm transmit") {
		t.Fatalf("failure response error = %v, want active shm rejection", err)
	}
	if s.kittySHMGated() {
		t.Fatal("gate still armed after a failure response")
	}
	if !st.kittySHMLive() {
		t.Fatal("failure response changed the selected shm transport")
	}
}

// TestKittySHMLateErrorAfterDeadlinesAndIDReuseIsIgnored reproduces a terminal
// opening an object after two acknowledgement deadlines let the double-buffer
// image id cycle back. Reusing id 1 unlinks its old object, so the delayed old
// rejection must not settle the gate for the new id 1 transmit. A rejection
// for that current transmit remains fatal.
func TestKittySHMLateErrorAfterDeadlinesAndIDReuseIsIgnored(t *testing.T) {
	st := shmState()
	s := &streamSession{st: &st, eng: &fakeEngine{}}

	for _, id := range []int{1, 2} {
		handoffKittySHM(s, id)
		s.kittyShmDeadline = time.Now().Add(-time.Millisecond)
		if s.kittySHMGated() {
			t.Fatalf("id %d gate held past its deadline", id)
		}
	}

	handoffKittySHM(s, 1)
	if !s.kittySHMGated() {
		t.Fatal("reused id did not arm the current transmit gate")
	}
	if err := s.handleKittyResponse("i=1;ENOENT:failed to open shared memory object"); err != nil {
		t.Fatalf("delayed rejection for the unlinked old transmit returned error: %v", err)
	}
	if !s.kittySHMGated() {
		t.Fatal("delayed old rejection settled the current transmit gate")
	}

	err := s.handleKittyResponse("i=1;EBADF:could not open current shared memory object")
	if err == nil || !strings.Contains(err.Error(), "terminal rejected shm transmit") {
		t.Fatalf("current rejection error = %v, want active shm rejection", err)
	}
	if s.kittySHMGated() {
		t.Fatal("current rejection left its pacing gate armed")
	}
	if !st.kittySHMLive() {
		t.Fatal("current rejection changed the selected shm transport")
	}
}

// TestKittySHMLateProbeRepliesDoNotSettleCurrentGate asserts startup replies
// delayed into the input loop are ignored, including failures, without opening
// the gate for a live frame. An uncorrelated failure remains fatal.
func TestKittySHMLateProbeRepliesDoNotSettleCurrentGate(t *testing.T) {
	st := shmState()
	s := &streamSession{st: &st, eng: &fakeEngine{}}
	handoffKittySHM(s, 1)

	for _, body := range []string{"i=31;EBADF:late direct probe", "i=32;OK"} {
		if err := s.handleKittyResponse(body); err != nil {
			t.Fatalf("probe response %q returned error: %v", body, err)
		}
		if !s.kittySHMGated() {
			t.Fatalf("probe response %q opened the current frame gate", body)
		}
	}
	if err := s.handleKittyResponse("i=99;EBADF:unknown transmit"); err == nil {
		t.Fatal("unknown failure response returned success")
	}
	if !s.kittySHMGated() {
		t.Fatal("unknown failure response opened the current frame gate")
	}
	if err := s.handleKittyResponse("i=1;OK"); err != nil {
		t.Fatalf("current response returned error: %v", err)
	}
	if s.kittySHMGated() {
		t.Fatal("matching current response left the gate armed")
	}
}

// TestKittySHMLateErrorAfterCycleAwayIsIgnored reproduces the Ghostty
// switch-window race: an shm transmit is in flight when Alt+R cycles away from
// Kitty, so resetKitty unlinks the object before the busy terminal reads the
// escape, and the terminal's ENOENT reply lands after the switch. That failure
// is self-inflicted, so the viewer ignores it. The next cycle back to Kitty
// must run shm again with the engine decode on and the stale pacing gate
// dropped.
func TestKittySHMLateErrorAfterCycleAwayIsIgnored(t *testing.T) {
	st := shmState()
	st.SetEnabled([]Backend{BackendKitty, BackendBlock})
	st.kittyID = 1
	eng := &decodeFakeEngine{}
	done := make(chan struct{})
	defer close(done)
	s := &streamSession{eng: eng, st: &st, tty: &bytes.Buffer{}, ctl: newControl(eng, done)}

	// One shm transmit awaits its acknowledgement when the user cycles the backend.
	handoffKittySHM(s, 1)
	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendBlock {
		t.Fatalf("backend after cycle = %v, want Block", st.Backend)
	}
	if s.kittyShmAwait {
		t.Fatal("cycling away from Kitty left the stale shm pacing gate armed")
	}

	s.handleKey(keyEvent{act: actionCycleBackend})
	if st.Backend != BackendKitty {
		t.Fatalf("backend after second cycle = %v, want Kitty", st.Backend)
	}
	if !st.kittySHMLive() {
		t.Fatal("shm transport not live after returning to Kitty")
	}
	if !eng.kittyDecode {
		t.Fatal("engine kitty-shm decode not re-enabled on return to Kitty")
	}
	handoffKittySHM(s, 1)
	if !s.kittySHMGated() {
		t.Fatal("new Kitty stint did not arm its first frame gate")
	}

	// The old and new stints both use id 1. The oldest ledger entry claims the
	// stale failure, which must not open the new stint's gate.
	if err := s.handleKittyResponse("i=1;ENOENT:failed to open shared memory object"); err != nil {
		t.Fatalf("late failure response after cycling returned error: %v", err)
	}
	if !s.kittySHMGated() {
		t.Fatal("stale cycle reply opened the new Kitty stint's gate")
	}
	if err := s.handleKittyResponse("i=1;OK"); err != nil {
		t.Fatalf("new stint response returned error: %v", err)
	}
	if s.kittySHMGated() {
		t.Fatal("new stint response left its gate armed")
	}
}

// TestKittySHMResponseErrorPreservesTransportAndDecode asserts a runtime
// rejection returns an error without switching transport or disabling the
// engine's shm plane decode.
func TestKittySHMResponseErrorPreservesTransportAndDecode(t *testing.T) {
	st := shmState()
	eng := &decodeFakeEngine{}
	syncEngineKittyDecode(eng, &st)
	s := &streamSession{eng: eng, st: &st}

	handoffKittySHM(s, 1)
	err := s.handleKittyResponse("i=1;EBADF:could not open shared memory")
	if err == nil || !strings.Contains(err.Error(), "terminal rejected shm transmit") {
		t.Fatalf("failure response error = %v, want active shm rejection", err)
	}
	if !st.kittySHMLive() {
		t.Fatal("failure response changed the selected shm transport")
	}
	if !eng.kittyDecode {
		t.Fatal("failure response disabled the engine kitty-shm decode")
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
// after a cycle away or direct transport resolution, and a no-op for engines
// without the capability.
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
	st.kittyTransport = KittyTransportDirect
	syncEngineKittyDecode(eng, &st)
	if eng.kittyDecode {
		t.Fatal("direct transport must keep the decode off")
	}

	syncEngineKittyDecode(&fakeEngine{}, &st) // must not panic without the capability
}
