(function(){
// The paste target decides whether Input.insertText can land. Chromium commits
// the text through the focused frame's input-method controller, which inserts
// at that frame's editing selection, and drops it without a word when nothing
// there can take it. A caller that wants to report the drop has to ask the page
// which case it is in.
//
// It answers 'field' when the insert has somewhere to go, 'none' when nothing
// does, and 'unknown' when the page cannot be read: focus inside a cross-origin
// frame, or an editing query the browser refused.
//
// Two oracles answer, and each alone is wrong somewhere, so 'none' needs both
// to say no and either one saying yes is enough for 'field'. Silence costs a
// reader nothing; a wrong claim contradicts the text on their screen.
//
// The editing query is the commit's own question, and it catches the case the
// DOM hides: blurring a text field leaves the frame's editing selection inside
// it, so an insert still lands there with the body as the active element. It
// misses the opposite case: a page that drops the document selection while a
// field keeps focus still takes the insert, and the query says no. It is
// deprecated too, so a browser that drops it throws here and the answer
// degrades to 'unknown', which reports nothing rather than a guess.
function enabled(doc){
try{
return doc.queryCommandEnabled('insertText');
}catch(err){
return null;
}
}

// nonText lists the input types that hold focus but take no text, so a focused
// checkbox or button reads as no target at all.
var nonText={button:1,checkbox:1,color:1,file:1,hidden:1,image:1,radio:1,range:1,reset:1,submit:1};

function editable(el){
if(!el)return false;
var tag=el.tagName;
if(tag==='TEXTAREA')return !el.disabled&&!el.readOnly;
if(tag==='INPUT')return !el.disabled&&!el.readOnly&&!nonText[String(el.type||'text').toLowerCase()];
return !!el.isContentEditable;
}

// Focus nests through frames, so walk inward from the top document: a frame's
// field is invisible to the document above it. The depth bound stops a frame
// chain from costing the whole dispatch budget.
var doc=document;
for(var depth=0;depth<8;depth++){
var ok=enabled(doc);
if(ok===null)return 'unknown';
var el=doc.activeElement;
if(ok||editable(el))return 'field';
if(!el)return 'none';
if(el.tagName!=='IFRAME'&&el.tagName!=='FRAME')return 'none';
var inner=null;
try{inner=el.contentDocument;}catch(err){inner=null;}
if(!inner)return 'unknown';
doc=inner;
}
return 'unknown';
})();
