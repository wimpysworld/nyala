package viewer

import "github.com/wimpysworld/nyala/internal/render"

type fakeEngine struct {
	frames chan render.Frame
}

func (e *fakeEngine) SetBackend(Backend)          {}
func (e *fakeEngine) SetCellPx(int, int)          {}
func (e *fakeEngine) Resize(int, int)             {}
func (e *fakeEngine) Navigate(string) error       { return nil }
func (e *fakeEngine) Frames() <-chan render.Frame { return e.frames }
func (e *fakeEngine) SendKey(string)              {}
func (e *fakeEngine) SendMouse(MouseEvent)        {}
func (e *fakeEngine) Close() error                { return nil }
