package engine

import (
	"time"
)

// snapshotReadTimeout bounds one in-page text-walk read. Document layout can
// wedge, and the snapshot goroutine is the only one that decodes text, so a hung
// read must not block session teardown. It matches fontCalibrationTimeout,
// the other bounded layout read on this goroutine.
const snapshotReadTimeout = 2 * time.Second

// hiddenOpacityEps is the effective-opacity floor below which a text run is treated
// as invisible and dropped. A visibility:hidden or opacity:0 element still lays out
// a box, so its text would compose phantom runs over the page without this drop. The
// in-page walk folds the effective-opacity check into the tree walk against this
// floor, so a hidden node emits no run.
const hiddenOpacityEps = 0.05
