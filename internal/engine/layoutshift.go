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

// layoutShiftEpsilon is the pending layout-shift score the observer must exceed
// before it pokes the binding, so animation jitter and sub-pixel reflow noise do
// not thrash the snapshot goroutine. The reflow-channel debounce rate-limits
// further, so this only needs to drop the smallest shifts; a real content reflow
// (a late web-font swap, an image loading and reshaping a card) scores well above
// it.
const layoutShiftEpsilon = 0.01

// buildLayoutShiftScript returns the JavaScript that observes layout-shift
// performance entries and pokes the layout-shift binding once the pending
// non-input shift score crosses layoutShiftEpsilon. It runs via
// Page.addScriptToEvaluateOnNewDocument, so it installs on the first document and
// every later one, catching a reflow that lands after the last navigation, resize,
// scroll, or networkAlmostIdle trigger. buffered:true replays shifts that landed
// before the observer registered, so an early reflow is not lost. It ignores
// entries flagged hadRecentInput (a shift the user caused by interacting) and
// zero-value entries, and guards against a browser without the layout-shift entry
// type so the script never throws where PerformanceObserver lacks it. The binding
// is called with a single string argument, the running cumulative score, since
// Runtime bindings take exactly one string argument. It stores the observer on
// window.<observerHandle> so the removal script can disconnect it.
func buildLayoutShiftScript() string {
	var b strings.Builder
	b.WriteString("(function(){")
	b.WriteString("if(typeof PerformanceObserver!=='function')return;")
	b.WriteString("var supported=PerformanceObserver.supportedEntryTypes;")
	b.WriteString("if(supported&&supported.indexOf('layout-shift')===-1)return;")
	b.WriteString("var fn=window[")
	b.WriteString(jsStringLiteral(layoutShiftBinding))
	b.WriteString("];if(typeof fn!=='function')return;")
	b.WriteString("var cumulative=0,pending=0;")
	b.WriteString("var o=new PerformanceObserver(function(list){")
	b.WriteString("var entries=list.getEntries();")
	b.WriteString("for(var i=0;i<entries.length;i++){var e=entries[i];")
	b.WriteString("if(e.hadRecentInput)continue;")
	b.WriteString("if(!(e.value>0))continue;")
	b.WriteString("cumulative+=e.value;pending+=e.value;}")
	fmt.Fprintf(&b, "if(pending>%g){pending=0;try{fn(String(cumulative));}catch(err){}}", layoutShiftEpsilon)
	b.WriteString("});")
	b.WriteString("window.")
	b.WriteString(layoutShiftObserverHandle)
	b.WriteString("=o;")
	b.WriteString("try{o.observe({type:'layout-shift',buffered:true});}catch(err){}")
	b.WriteString("})();")
	return b.String()
}

// buildLayoutShiftRemovalScript returns the JavaScript that disconnects the live
// layout-shift observer, so a Tab from the block backend to a graphics backend
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
