package engine

import (
	"fmt"
	"strings"
)

// layoutShiftBinding is the name of the Runtime binding the injected
// PerformanceObserver calls when the page's cumulative layout shift crosses the
// threshold. Runtime.addBinding exposes window.<name> as a function; each call
// raises a Runtime.bindingCalled event the engine listens for. It must match in
// the addBinding registration and in the observer script.
const layoutShiftBinding = "__nyalaLayoutShift"

// layoutShiftObserverHandle is the window property the observer script stores its
// PerformanceObserver on, so the removal script can disconnect the live observer
// on a backend switch.
const layoutShiftObserverHandle = "__nyalaLayoutShiftObserver"

// layoutGenProperty is the window property the observer script keeps a monotonic
// layout-generation counter on. It starts at 0 on each document and increments on
// every non-input layout shift the observer sees, so the snapshot goroutine can
// read the current generation in the same sequence as its DOMSnapshot and tag the
// decoded runs with it. composeFrame then pairs a run set to a bitmap only when
// their generations agree, so the overlay never composes over a reflowed bitmap of
// a different generation. A document without the observer (a graphics backend, or
// the observer failing to install) leaves it undefined, which every read coerces
// to 0.
const layoutGenProperty = "__nyalaLayoutGen"

// layoutShiftWorld is the name of the isolated world the layout-shift observer
// script installs into. The observer, its binding reference, and the generation
// counter live on that world's window, so the page cannot see or tamper with
// them. Page.addScriptToEvaluateOnNewDocument takes the name as
// worldName and Page.createIsolatedWorld reuses it, so both routes address one
// world per frame.
const layoutShiftWorld = "__nyala_world"

// layoutShiftEpsilon is the pending layout-shift score the observer must exceed
// before it pokes the binding, so animation jitter and sub-pixel reflow noise do
// not thrash the snapshot goroutine. The reflow-channel debounce rate-limits
// further, so this only needs to drop the smallest shifts; a real content reflow
// (a late web-font swap, an image loading and reshaping a card) scores well above
// it.
const layoutShiftEpsilon = 0.01

// buildLayoutShiftScript returns the JavaScript that observes layout-shift
// performance entries, bumps the layout-generation counter on every non-input
// shift, and pokes the layout-shift binding once the pending non-input shift score
// crosses layoutShiftEpsilon. It runs via Page.addScriptToEvaluateOnNewDocument, so
// it installs on the first document and every later one, catching a reflow that
// lands after the last navigation, resize, scroll, or networkAlmostIdle trigger.
// buffered:true replays shifts that landed before the observer registered, so an
// early reflow is not lost. It ignores entries flagged hadRecentInput (a shift the
// user caused by interacting) and zero-value entries, and guards against a browser
// without the layout-shift entry type so the script never throws where
// PerformanceObserver lacks it. The generation counter bumps on every qualifying
// entry, independent of the epsilon threshold that gates the binding poke, so a
// sub-epsilon reflow that moves boxes without re-snapshotting still advances the
// generation and the compose pairing can tell the layouts apart. Runtime bindings
// take exactly one string argument, so the script encodes its payload: it calls the
// binding once at install with the literal 'announce', which tells the engine the
// isolated execution context id the script runs in, and calls it on each qualifying
// shift with 'shift:<cumulative>:<generation>'. The announce call sits directly
// after the binding guard, because a world without the binding must return before
// it, and before the PerformanceObserver capability guards, because the text walk
// needs the context id even where the observer cannot install. A browser without
// the observer therefore loses the layout-shift trigger alone and keeps the text
// overlay. The
// script runs in every frame, so the announce is guarded by a top-frame test: an
// iframe's isolated world must not overwrite the recorded context id and send the
// text walk into a subframe document. The observer install stays unconditional, so
// a shift in a subframe still reports. It
// stores the observer on
// window.<observerHandle> so the removal script can disconnect it, and initialises
// window.<layoutGenProperty> to 0 so the generation read returns a number from the
// first document.
func buildLayoutShiftScript() string {
	var b strings.Builder
	b.WriteString("(function(){")
	b.WriteString("if(window.")
	b.WriteString(layoutGenProperty)
	b.WriteString("===undefined)window.")
	b.WriteString(layoutGenProperty)
	b.WriteString("=0;")
	b.WriteString("var fn=window[")
	b.WriteString(jsStringLiteral(layoutShiftBinding))
	b.WriteString("];if(typeof fn!=='function')return;")
	b.WriteString("try{if(window.top===window)fn('announce');}catch(err){}")
	b.WriteString("if(typeof PerformanceObserver!=='function')return;")
	b.WriteString("var supported=PerformanceObserver.supportedEntryTypes;")
	b.WriteString("if(supported&&supported.indexOf('layout-shift')===-1)return;")
	b.WriteString("var cumulative=0,pending=0;")
	b.WriteString("var o=new PerformanceObserver(function(list){")
	b.WriteString("var entries=list.getEntries();")
	b.WriteString("for(var i=0;i<entries.length;i++){var e=entries[i];")
	b.WriteString("if(e.hadRecentInput)continue;")
	b.WriteString("if(!(e.value>0))continue;")
	b.WriteString("window.")
	b.WriteString(layoutGenProperty)
	b.WriteString("++;")
	b.WriteString("cumulative+=e.value;pending+=e.value;}")
	fmt.Fprintf(&b, "if(pending>%[2]g){pending=0;try{fn('shift:'+String(cumulative)+':'+String(window.%[1]s));}catch(err){}}", layoutGenProperty, layoutShiftEpsilon)
	b.WriteString("});")
	b.WriteString("window.")
	b.WriteString(layoutShiftObserverHandle)
	b.WriteString("=o;")
	b.WriteString("try{o.observe({type:'layout-shift',buffered:true});}catch(err){}")
	b.WriteString("})();")
	return b.String()
}

// buildLayoutShiftRemovalScript returns the JavaScript that disconnects the live
// layout-shift observer, so a cycle from the block backend to a graphics backend
// stops the observer without a reload. It reads the observer off
// window.<observerHandle>, disconnects it, and clears the handle; a missing
// observer is a no-op, so it never throws when none is live.
func buildLayoutShiftRemovalScript() string {
	var b strings.Builder
	b.WriteString("(function(){var o=window.")
	b.WriteString(layoutShiftObserverHandle)
	b.WriteString(";if(o&&typeof o.disconnect==='function'){o.disconnect();}")
	b.WriteString("window.")
	b.WriteString(layoutShiftObserverHandle)
	b.WriteString("=undefined;})();")
	return b.String()
}
