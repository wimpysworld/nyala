package viewer

import (
	"os"
	"os/signal"
)

// watchFatalSignal takes ownership of a channel registered with signal.Notify.
// Its stop function lets the watcher finish any received or queued signal
// before the caller can return normally.
func watchFatalSignal(fatal chan os.Signal, restore func(), closeEngine func() error, exit func(int)) func() {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if sig, ok := <-fatal; ok {
			handleFatalSignal(sig, restore, closeEngine, exit)
		}
	}()
	return func() {
		signal.Stop(fatal)
		close(fatal)
		<-finished
	}
}
