package viewer

import (
	"github.com/wimpysworld/nyala/internal/engine"
	"github.com/wimpysworld/nyala/internal/render"
)

type fakeEngine struct {
	frames chan render.Frame
}

func (e *fakeEngine) SetBackend(Backend)          {}
func (e *fakeEngine) SetCellPx(int, int)          {}
func (e *fakeEngine) Resize(int, int)             {}
func (e *fakeEngine) Navigate(string) error       { return nil }
func (e *fakeEngine) Frames() <-chan render.Frame { return e.frames }
func (e *fakeEngine) SendKey(string)              {}
func (e *fakeEngine) SendText(string)             {}
func (e *fakeEngine) SendPaste(string) bool       { return false }
func (e *fakeEngine) SendNav(NavAction)           {}
func (e *fakeEngine) CurrentURL() string          { return "" }
func (e *fakeEngine) CurrentTitle() string        { return "" }

func (e *fakeEngine) LoadState() engine.LoadState { return engine.LoadState{} }
func (e *fakeEngine) OpenURL(string)              {}
func (e *fakeEngine) BrowserVersion() string      { return "" }
func (e *fakeEngine) Find(string)                 {}
func (e *fakeEngine) FindNext()                   {}
func (e *fakeEngine) FindPrev()                   {}
func (e *fakeEngine) FindClear()                  {}
func (e *fakeEngine) FindState() FindState        { return FindState{} }
func (e *fakeEngine) Selection() (string, bool)   { return "", false }
func (e *fakeEngine) SendMouse(MouseEvent)        {}
func (e *fakeEngine) Close() error                { return nil }
