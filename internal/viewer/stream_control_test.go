package viewer

import (
	"errors"
	"image"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wimpysworld/nyala/internal/render"
)

type captureControlTestEngine struct {
	fakeEngine
	configure func(captureConfig) error
	streamErr error
}

func (e *captureControlTestEngine) ConfigureCapture(backend Backend, cols, rows, cellW, cellH int, kittySHM bool) error {
	return e.configure(captureConfig{backend, cols, rows, cellPx{W: cellW, H: cellH}, kittySHM})
}

func (e *captureControlTestEngine) StreamError() error { return e.streamErr }

func awaitCaptureConfig(t *testing.T, started <-chan captureConfig) captureConfig {
	t.Helper()
	select {
	case cfg := <-started:
		return cfg
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not receive a capture configuration")
		return captureConfig{}
	}
}

func TestCaptureControlKeepsSameBackendRepaint(t *testing.T) {
	started := make(chan captureConfig, 3)
	gate := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	eng := &captureControlTestEngine{configure: func(cfg captureConfig) error {
		started <- cfg
		select {
		case <-gate:
		case <-done:
		}
		return nil
	}}
	st := NewState(80, 24)
	s := &streamSession{eng: eng, st: &st, tty: io.Discard, ctl: newControl(eng, done)}
	s.recomposeBlock(true)
	first := awaitCaptureConfig(t, started)

	s.recomposeBlock(true)
	s.recomposeBlock(true)
	gate <- struct{}{}
	if got := awaitCaptureConfig(t, started); got != first {
		t.Fatalf("repaint changed capture configuration: got %+v, want %+v", got, first)
	}
}

func TestCaptureControlDiscardsPendingRestartOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{}, 2)
		gate := make(chan struct{})
		done := make(chan struct{})
		eng := &captureControlTestEngine{configure: func(captureConfig) error {
			started <- struct{}{}
			<-gate
			return nil
		}}
		ctl := newControl(eng, done)
		ctl.submit(captureConfig{backend: BackendBlock})
		<-started
		ctl.submit(captureConfig{backend: BackendKitty})
		close(done)
		close(gate)
		synctest.Wait()
		select {
		case <-started:
			t.Fatal("queued restart ran after shutdown")
		default:
		}
	})
}

func TestStreamLoopReportsCaptureControlError(t *testing.T) {
	want := errors.New("capture restart failed")
	eng := &captureControlTestEngine{configure: func(captureConfig) error { return want }}
	done := make(chan struct{})
	defer close(done)
	st := NewState(80, 24)
	s := &streamSession{eng: eng, st: &st, tty: io.Discard, ctl: newControl(eng, done)}
	keys := make(chan keyEvent, 1)
	defer close(keys)
	keys <- keyEvent{act: actionCycleBackend}
	loopDone := make(chan error, 1)
	go func() { loopDone <- s.loop(nil, keys) }()
	select {
	case err := <-loopDone:
		if !errors.Is(err, want) {
			t.Fatalf("loop returned %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not report the restart failure")
	}
}

func TestStreamLoopReportsClosedFrameError(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "empty"
		if queued {
			name = "queued plane"
		}
		t.Run(name, func(t *testing.T) {
			frames := make(chan render.Frame, 1)
			released := 0
			if queued {
				frames <- render.Frame{
					Img: image.NewRGBA(image.Rect(0, 0, 4, 4)), Release: func() { released++ },
				}
			}
			close(frames)
			want := errors.New("capture recovery failed")
			eng := &captureControlTestEngine{fakeEngine: fakeEngine{frames: frames}, streamErr: want}
			st := NewState(80, 24)
			s := &streamSession{eng: eng, st: &st, tty: io.Discard}
			if err := s.loop(nil, nil); !errors.Is(err, want) {
				t.Fatalf("loop returned %v, want %v", err, want)
			}
			if queued && released != 1 {
				t.Fatalf("queued frame released %d times, want 1", released)
			}
		})
	}
}
