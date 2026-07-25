(function(config){
var state=window[config.stateProperty];
if(!state){state={query:'',ranges:[],index:-1};window[config.stateProperty]=state;}

function report(){
return {total:state.ranges.length,current:state.index+1,sx:window.scrollX,sy:window.scrollY};
}

function styleSheet(){
var el=document.getElementById(config.styleID);
if(el)return el;
el=document.createElement('style');
el.id=config.styleID;
el.textContent='::highlight('+config.allName+'){background-color:'+config.allColour+';color:'+config.ink+'}'+
'::highlight('+config.currentName+'){background-color:'+config.currentColour+';color:'+config.ink+'}';
(document.head||document.documentElement).appendChild(el);
return el;
}

function paint(){
if(!window.CSS||!CSS.highlights||!window.Highlight)return;
styleSheet();
var all=new Highlight(),current=new Highlight();
for(var i=0;i<state.ranges.length;i++){(i===state.index?current:all).add(state.ranges[i]);}
CSS.highlights.set(config.allName,all);
CSS.highlights.set(config.currentName,current);
}

function unpaint(){
if(window.CSS&&CSS.highlights){
CSS.highlights.delete(config.allName);
CSS.highlights.delete(config.currentName);
}
var el=document.getElementById(config.styleID);
if(el)el.remove();
}

function sameStart(a,b){
return !!a&&!!b&&a.startContainer===b.startContainer&&a.startOffset===b.startOffset;
}

// saveCaret records the focused element and, for a text control, its caret. A
// control's caret lives in its own inner editor, which a document Range cannot
// round-trip, so it is saved separately. A control whose type does not support
// selection throws on the read, which leaves nothing to restore.
function saveCaret(){
var el=document.activeElement;
if(!el)return null;
try{
if(typeof el.selectionStart==='number'){
return {el:el,start:el.selectionStart,end:el.selectionEnd,dir:el.selectionDirection};
}
}catch(err){}
return {el:el};
}

// restoreCaret puts focus and the text-control caret back, so page input after a
// search still reaches the element and the offset the reader left it at.
function restoreCaret(saved){
if(!saved||!saved.el)return;
if(document.activeElement!==saved.el&&saved.el.focus)saved.el.focus();
if(typeof saved.start!=='number'||!saved.el.setSelectionRange)return;
try{saved.el.setSelectionRange(saved.start,saved.end,saved.dir);}catch(err){}
}

// collect asks Chromium's own window.find for every match in document order. It
// borrows the DOM selection find works through, then puts the page's selection,
// focus, caret, and scroll offset back, so a search leaves none of its own.
function collect(query){
var selection=window.getSelection();
var saved=[];
for(var i=0;i<selection.rangeCount;i++){saved.push(selection.getRangeAt(i).cloneRange());}
var caret=saveCaret();
var sx=window.scrollX,sy=window.scrollY;
var anchor=document.createRange();
anchor.setStart(document.body,0);
anchor.collapse(true);
selection.removeAllRanges();
selection.addRange(anchor);
var found=[];
while(found.length<config.max&&window.find(query,false,false,false,false,false,false)){
if(selection.rangeCount===0)break;
var hit=selection.getRangeAt(0).cloneRange();
// A match that cannot advance would spin the walk, so stop on a repeat.
if(sameStart(hit,found[found.length-1]))break;
found.push(hit);
}
selection.removeAllRanges();
for(var j=0;j<saved.length;j++){selection.addRange(saved[j]);}
restoreCaret(caret);
window.scrollTo(sx,sy);
return found;
}

// reveal scrolls the current match into view, leaving an already-visible match
// alone so a search does not move the page under the reader. Chromium's own
// scrollIntoView handles nested scroll containers; the window offset is then
// centred on the match and, when the backend quantises scroll, snapped to the
// cell pitch so the crisp text overlay stays on the grid.
function reveal(){
var range=state.ranges[state.index];
if(!range)return;
var rect=range.getBoundingClientRect();
if(rect.top>=0&&rect.bottom<=window.innerHeight&&rect.left>=0&&rect.right<=window.innerWidth)return;
var node=range.startContainer;
var element=node.nodeType===1?node:node.parentElement;
if(element&&element.scrollIntoView)element.scrollIntoView({block:'center',inline:'nearest'});
rect=range.getBoundingClientRect();
var target=window.scrollY+rect.top+rect.height/2-window.innerHeight/2;
if(config.cellHeight>0)target=Math.round(target/config.cellHeight)*config.cellHeight;
window.scrollTo(window.scrollX,Math.max(0,target));
}

if(config.op==='clear'||config.query===''||!document.body){
unpaint();
state.query='';state.ranges=[];state.index=-1;
return report();
}

if(config.op==='query'||state.query!==config.query){
state.query=config.query;
state.ranges=collect(config.query);
state.index=state.ranges.length>0?0:-1;
}else if(state.ranges.length>0){
state.index=(state.index+config.step+state.ranges.length)%state.ranges.length;
}
paint();
reveal();
return report();
})(__NYALA_FIND_CONFIG__);
