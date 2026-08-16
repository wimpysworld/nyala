(function(config){
var cw=config.cellWidth,ch=config.cellHeight;
var runs=[];
var csCache=new Map();

function gcs(el){
if(!csCache.has(el)){csCache.set(el,getComputedStyle(el));}
return csCache.get(el);
}

var ordCache=null;

function ord(el){
if(!ordCache){ordCache=new Map();var r=document.documentElement,i=0,n;ordCache.set(r,i++);var ow=document.createTreeWalker(r,NodeFilter.SHOW_ELEMENT,null);while((n=ow.nextNode())){ordCache.set(n,i++);}}
var v=ordCache.get(el);return v===undefined?-1:v;
}

function collapsed(clip){
var m=/^rect\((.+)\)$/.exec(clip);if(!m)return false;
var f=m[1].split(/[ ,]+/);if(f.length!==4)return false;
function off(s){s=s.trim();if(s==='auto')return null;return parseFloat(s);}
var t=off(f[0]),r=off(f[1]),bo=off(f[2]),l=off(f[3]);
var hc=(r!==null&&l!==null&&r<=l);var vc=(bo!==null&&t!==null&&bo<=t);
return hc||vc;
}

function clipPathHidden(cp){
var m=/^inset\(([^)]+)\)$/.exec(cp);if(!m)return false;
var p=m[1].trim().split(/\s+/);var ri=p.indexOf('round');if(ri>=0)p=p.slice(0,ri);
if(p.length<1||p.length>4)return false;
function pc(s){return /%$/.test(s)?(parseFloat(s)||0):0;}
var t,r,bo,l;
if(p.length===1){t=r=bo=l=pc(p[0]);}
else if(p.length===2){t=bo=pc(p[0]);r=l=pc(p[1]);}
else if(p.length===3){t=pc(p[0]);r=l=pc(p[1]);bo=pc(p[2]);}
else{t=pc(p[0]);r=pc(p[1]);bo=pc(p[2]);l=pc(p[3]);}
return (l+r>=100)||(t+bo>=100);
}

function hiddenStyle(el){
var s=gcs(el);
if(s.visibility==='hidden'||s.visibility==='collapse')return true;
var pos=s.position;
if((pos==='absolute'||pos==='fixed')&&(clipPathHidden(s.clipPath)||collapsed(s.clip)||(s.overflow==='hidden'&&parseFloat(s.width)<=1&&parseFloat(s.height)<=1)))return true;
var op=1;for(var a=el;a;a=a.parentElement){var ao=parseFloat(gcs(a).opacity);if(!isNaN(ao))op*=ao;}
if(op<config.opacityThreshold)return true;
return false;
}

function rgba(el){
var c=gcs(el).color;
var m=/rgba?\(([^)]+)\)/.exec(c);if(!m)return{r:0,g:0,b:0,a:255};
var p=m[1].split(/[ ,\/]+/).filter(function(x){return x.length;});
var a=p.length>3?Math.round(Math.max(0,Math.min(1,parseFloat(p[3])))*255):255;
return{r:parseInt(p[0])||0,g:parseInt(p[1])||0,b:parseInt(p[2])||0,a:a};
}

function bgOf(el,stop){
function parse(c){
var m=/rgba?\(([^)]+)\)/.exec(c);if(!m)return null;
var p=m[1].split(/[ ,\/]+/).filter(function(x){return x.length;});
var a=p.length>3?Math.max(0,Math.min(1,parseFloat(p[3]))):1;
return{r:parseInt(p[0])||0,g:parseInt(p[1])||0,b:parseInt(p[2])||0,a:a};
}
var or=0,og=0,ob=0,cov=0;
for(var a=el;a&&a!==stop;a=a.parentElement){
var s=parse(gcs(a).backgroundColor);
if(!s||s.a<=0)continue;var w=s.a*(1-cov);
or+=s.r*w;og+=s.g*w;ob+=s.b*w;cov+=w;if(cov>=0.999)break;
}
if(cov<0.999&&!stop){
var ds=parse(gcs(document.documentElement).backgroundColor)||parse(document.body?gcs(document.body).backgroundColor:'');
if(ds&&ds.a>0){var w=ds.a*(1-cov);or+=ds.r*w;og+=ds.g*w;ob+=ds.b*w;cov+=w;}
}
if(cov<=0)return{r:0,g:0,b:0,a:0};
return{r:Math.round(or/cov),g:Math.round(og/cov),b:Math.round(ob/cov),a:Math.round(Math.min(1,cov)*255)};
}

var FIXED_CAP=32;
var fixed=[];
var anchorCache=new Map();
var gen=(window[config.generationProperty]||0);

// linkCache maps an element to the nearest ancestor <a href> answer, so the climb
// runs once per element, like anchorCache does for the fixed hold. linkIds gives
// each anchor element one stable id per walk, an incrementing counter prefixed
// with the layout generation, so a partial repaint keeps one link per anchor.
// The a.href read happens once per anchor, when the anchor's own cache entry is
// built, and the resolved absolute URL travels in the entry.
var linkCache=new Map();
var linkIds=new Map();
var linkN=0;

function linkOf(el){
if(linkCache.has(el))return linkCache.get(el);
var chain=[],v=null;
for(var a=el;a;a=a.parentElement){
if(linkCache.has(a)){v=linkCache.get(a);break;}
chain.push(a);
if(a.tagName==='A'&&a.hasAttribute('href')){
// The block layer drops any URI over 2083 bytes, so serialising a longer
// href buys nothing, and a data: URL is unbounded. Drop the link here to
// keep the walk JSON small. The UTF-16 length is a lower bound on the
// UTF-8 byte length, so nothing under the cap is ever dropped, and an
// over-cap multi-byte href the length check misses is still dropped
// downstream by the block sanitiser.
var h=a.href;
if(h.length>2083){v=null;break;}
var id=linkIds.get(a);
if(id===undefined){id='g'+gen+'-a'+(++linkN);linkIds.set(a,id);}
v={hr:h,li:id};
break;
}
}
for(var i=0;i<chain.length;i++){linkCache.set(chain[i],v);}
return v;
}

// A 'fixed' ancestor holds its screen rect while the page scrolls, so the text
// inside it must not move when the compose-time shift tracks the bitmap. Descent
// is what decides that, not geometry: a rectangle test would also freeze every
// flow run that happens to lie inside a large transparent fixed container, which
// is what a toast root or a full-viewport portal is. 'sticky' is left out on
// purpose: a stuck element is anchored but an unstuck one scrolls with the page,
// and telling them apart needs a second layout measurement the mid-scroll path
// cannot afford.
//
// The same climb collects the boxes the compose-time coverage cull tests against,
// through collectFixed, which is deliberately narrower: it drops a box that can
// cover nothing and stops at FIXED_CAP. Keeping the two answers apart means a
// dropped or truncated box weakens the cull alone, never the hold, so the loss is
// text bleeding through rather than text drifting away.
function fixedAncestor(el){
var chain=[],anchored=false;
for(var a=el;a;a=a.parentElement){
if(anchorCache.has(a)){anchored=anchorCache.get(a);break;}
chain.push(a);
}
for(var i=chain.length-1;i>=0;i--){
var n=chain[i];
if(gcs(n).position==='fixed'){anchored=true;collectFixed(n);}
anchorCache.set(n,anchored);
}
return anchored;
}

// collectFixed records one fixed element's screen rect for the coverage cull. Its
// caller visits each element exactly once, so each box is recorded once.
function collectFixed(el){
if(fixed.length>=FIXED_CAP)return;
var s=gcs(el);
// A negative z-index paints the element behind content, so it can never cover.
var z=parseInt(s.zIndex,10);if(!isNaN(z)&&z<0)return;
var rc=el.getBoundingClientRect();
if(rc.width<=0||rc.height<=0)return;
// The parent stops the composite, so only the element's own background counts.
// Compositing past it would reach the opaque body nearly every page carries and
// read a transparent bar as a cover, which culls text the reader can see. A
// transparent wrapper around an opaque bar reads non-opaque instead: the cull
// then misses a real cover for one refresh window, the safe polarity.
fixed.push({l:rc.left,t:rc.top,r:rc.right,b:rc.bottom,o:bgOf(el,el.parentElement).a>=255});
}

function lca(a,b){
for(var n=a;n;n=n.parentElement){if(n.contains(b))return n;}
return null;
}

function coveredAt(el,x,y){
var top=document.elementFromPoint(x,y);if(!top)return null;
if(top===el||el.contains(top)||top.contains(el))return false;
return bgOf(top,lca(top,el)).a>=255;
}

// Sampling the line box's start, centre, and end keeps a partly covered run
// whole: only a cover over every sample that hit an element drops it. A null hit
// is no evidence, so an off-viewport sample neither culls nor saves the run.
function occluded(el,l,t,r,b){
var cy=(t+b)/2,inset=Math.min(1,(r-l)/4),sampled=false;
var xs=[l+inset,(l+r)/2,r-inset];
for(var i=0;i<xs.length;i++){
var cov=coveredAt(el,xs[i],cy);
if(cov===null)continue;
if(!cov)return false;
sampled=true;
}
return sampled;
}

function nn(v){if(!v)return false;var f=String(v).trim();if(!f)return false;return f.split(/[ ,(]/)[0]!=='none';}

// A 'display: contents' element generates no box, so it is neither a flex item
// itself nor the flex container of its children: the nearest ancestor that does
// generate a box holds them.
function flexItem(el){
for(var p=el.parentElement;p;p=p.parentElement){
var d=gcs(p).display;
if(d==='contents')continue;
return d==='flex'||d==='inline-flex'||d==='grid'||d==='inline-grid';
}
return false;
}

// The top layer paints above the whole page, outside the page's own stacking
// order, so its members take a level above any z-index CSS can express and root
// their own vector rather than extend the one they sit in. One query per walk
// matches nothing on a page with no dialog or popover open, and the guard covers
// an engine that does not know either selector.
var TOP_LAYER_Z=2147483648;
var topLayer=new Set();
try{var tls=document.querySelectorAll(':modal,:popover-open');for(var tli=0;tli<tls.length;tli++){topLayer.add(tls[tli]);}}catch(e){}

// Stacking participation is three-valued, because a 'relative' or 'absolute' box
// at 'z-index: auto' opens no stacking context. CSS 2.1 Appendix E step 8 paints
// such a box as if it did, after the parent context's in-flow content, but its
// positioned descendants and any descendant that opens a real context belong to
// the parent context, not to it. STACK_PSEUDO carries that half-level: it orders
// the box's own text but never traps a descendant's z-index beneath it. 'fixed'
// and 'sticky' open a real context whatever their z-index does.
var STACK_NONE=0,STACK_PSEUDO=1,STACK_REAL=2;

function stacks(el){
if(el===document.documentElement||topLayer.has(el))return STACK_REAL;
var s=gcs(el);
var pos=s.position,positioned=!!pos&&pos!=='static';
if(positioned&&pos!=='relative'&&pos!=='absolute')return STACK_REAL;
if(positioned&&s.zIndex&&s.zIndex!=='auto')return STACK_REAL;
if(s.zIndex&&s.zIndex!=='auto'&&flexItem(el))return STACK_REAL;
if(parseFloat(s.opacity)<1)return STACK_REAL;
if(s.mixBlendMode&&s.mixBlendMode!=='normal')return STACK_REAL;
if(nn(s.transform)||nn(s.scale)||nn(s.rotate)||nn(s.translate)||nn(s.perspective))return STACK_REAL;
if(nn(s.filter)||nn(s.backdropFilter))return STACK_REAL;
if(nn(s.clipPath))return STACK_REAL;
if(nn(s.mask)||nn(s.maskImage)||nn(s.maskBorder))return STACK_REAL;
if(s.isolation==='isolate')return STACK_REAL;
if(/\b(layout|paint|strict|content)\b/.test(s.contain||''))return STACK_REAL;
if(s.contentVisibility==='auto')return STACK_REAL;
if(/\b(opacity|transform|scale|rotate|translate|perspective|filter|backdrop-filter|clip-path|mask|mask-image|mask-border|isolation|mix-blend-mode|contain|content-visibility|position|z-index|view-transition-name)\b/.test(s.willChange||''))return STACK_REAL;
if(nn(s.viewTransitionName))return STACK_REAL;
return positioned?STACK_PSEUDO:STACK_NONE;
}

var partCache=new Map();

function participates(el){
if(!partCache.has(el)){partCache.set(el,stacks(el));}
return partCache.get(el);
}

function zOf(el){
if(topLayer.has(el))return TOP_LAYER_Z;
var s=gcs(el);
if(s.position==='static'&&!flexItem(el))return 0;
var z=parseInt(s.zIndex,10);return isNaN(z)?0:z;
}

// vecCache holds an element's own stacking vector; baseCache holds the vector its
// descendants extend. The two differ by exactly one entry beneath a pseudo level,
// which a deeper level replaces instead of nesting below. A pseudo level is
// therefore always the last entry of the vector that carries it.
var vecCache=new Map();
var baseCache=new Map();

function stackVector(el){
if(vecCache.has(el))return vecCache.get(el);
var chain=[],vec=null,base=null;
for(var a=el;a;a=a.parentElement){if(vecCache.has(a)){vec=vecCache.get(a);base=baseCache.get(a);break;}chain.push(a);}
if(!vec){vec=[];base=[];}
for(var i=chain.length-1;i>=0;i--){
var n=chain[i],st=participates(n);
if(topLayer.has(n))base=[];
if(st!==STACK_NONE){vec=base.concat([[zOf(n),ord(n)]]);if(st===STACK_REAL)base=vec;}
vecCache.set(n,vec);baseCache.set(n,base);
}
return vec;
}

function cmpVec(a,b){
var n=Math.min(a.length,b.length);
for(var i=0;i<n;i++){
if(a[i][0]!==b[i][0])return b[i][0]-a[i][0];
if(a[i][1]!==b[i][1])return b[i][1]-a[i][1];
}
if(a.length===b.length)return 0;
// The shorter vector is the shared context's own in-flow content, so the longer
// one paints behind it only when its next level carries a negative z-index.
if(a.length>b.length)return a[n][0]<0?1:-1;
return b[n][0]<0?-1:1;
}

var w=document.createTreeWalker(document.body||document.documentElement,NodeFilter.SHOW_TEXT,null);
var node;while((node=w.nextNode())){
var el=node.parentElement;if(!el)continue;
if(!node.nodeValue||!node.nodeValue.trim())continue;
if(hiddenStyle(el))continue;
var fx=fixedAncestor(el);
var lk=linkOf(el);
var fg=rgba(el);var bg=bgOf(el);var sv=stackVector(el);
var text=node.nodeValue;var lines=[];
var rng=document.createRange();rng.selectNodeContents(node);
var nodeRects=rng.getClientRects();var fragments=[];
for(var fi=0;fi<nodeRects.length;fi++){
var fr=nodeRects[fi];if(fr.width<=0&&fr.height<=0)continue;
fragments.push({id:fragments.length,l:fr.left,t:fr.top,r:fr.left+fr.width,b:fr.top+fr.height});
}

function fragmentOf(rr){
var cx=rr.left+rr.width/2,cy=rr.top+rr.height/2,best=-1,bestDist=Infinity;
for(var fi=0;fi<fragments.length;fi++){
var fr=fragments[fi];
if(cx>=fr.l-0.01&&cx<=fr.r+0.01&&cy>=fr.t-0.01&&cy<=fr.b+0.01)return fr.id;
var dx=cx<fr.l?fr.l-cx:(cx>fr.r?cx-fr.r:0);var dy=cy<fr.t?fr.t-cy:(cy>fr.b?cy-fr.b:0);
var dist=dx*dx+dy*dy;if(dist<bestDist){bestDist=dist;best=fr.id;}
}
return best;
}

function lineAt(frag,row){
var ln=lines.length?lines[lines.length-1]:null;
if(!ln||ln.frag!==frag||ln.row!==row){ln={frag:frag,row:row,left:Infinity,top:Infinity,right:-Infinity,bottom:-Infinity,chars:[]};lines.push(ln);}
return ln;
}

var words=[];var complex=false;var wre=/\S+/g;var wm;
while((wm=wre.exec(text))){
rng.setStart(node,wm.index);rng.setEnd(node,wm.index+wm[0].length);
var wc=rng.getClientRects();
var wl=Infinity,wt=Infinity,wrt=-Infinity,wb=-Infinity,wrow=null,wfrag=null,wmulti=false;
for(var j=0;j<wc.length;j++){
var rr=wc[j];if(rr.width<=0&&rr.height<=0)continue;
var rw=Math.round(rr.top/ch),rf=fragmentOf(rr);
if(wrow===null){wrow=rw;wfrag=rf;}else if(wrow!==rw||wfrag!==rf){wmulti=true;break;}
if(rr.left<wl)wl=rr.left;if(rr.top<wt)wt=rr.top;
if(rr.left+rr.width>wrt)wrt=rr.left+rr.width;if(rr.top+rr.height>wb)wb=rr.top+rr.height;
}
if(wmulti){complex=true;break;}
if(wrow===null)continue;
words.push({frag:wfrag,row:wrow,l:wl,t:wt,r:wrt,b:wb,x:wm[0]});
}

if(complex){
for(var i=0;i<text.length;i++){
rng.setStart(node,i);rng.setEnd(node,i+1);
var rc=rng.getClientRects();if(!rc.length)continue;
var rr=rc[0];if(rr.width<=0&&rr.height<=0)continue;
var row=Math.round(rr.top/ch);
var frag=fragmentOf(rr);
var ln=lineAt(frag,row);
var ink=/\S/.test(text[i]);
if(ink){
if(rr.left<ln.left)ln.left=rr.left;
if(rr.top<ln.top)ln.top=rr.top;
if(rr.left+rr.width>ln.right)ln.right=rr.left+rr.width;
if(rr.top+rr.height>ln.bottom)ln.bottom=rr.top+rr.height;
}
ln.chars.push(text[i]);
}
}else{
for(var wi=0;wi<words.length;wi++){
var wd=words[wi];
var ln=lineAt(wd.frag,wd.row);
if(wd.l<ln.left)ln.left=wd.l;if(wd.t<ln.top)ln.top=wd.t;
if(wd.r>ln.right)ln.right=wd.r;if(wd.b>ln.bottom)ln.bottom=wd.b;
if(ln.chars.length)ln.chars.push(' ');ln.chars.push(wd.x);
}
}

for(var k=0;k<lines.length;k++){
var ln=lines[k];
if(!isFinite(ln.left))continue;
var t=ln.chars.join('').replace(/\s+/g,' ').replace(/^ | $/g,'');if(!t)continue;
var h=ln.bottom-ln.top;
if(h<ch/4)continue;
if(occluded(el,ln.left,ln.top,ln.right,ln.bottom))continue;
var run={t:t,c:Math.round(ln.left/cw),r:Math.round(ln.top/ch),x:ln.left,y:ln.top,w:ln.right-ln.left,h:h,fg:fg,bg:bg,z:sv,f:fx};
if(lk){run.hr=lk.hr;run.li=lk.li;}
runs.push(run);
}
}

// Each run carries its stacking vector until the sort below replaces it with the
// dense front-to-back rank, 0 being front-most. The array itself stays in DOM order.
var order=runs.map(function(_,i){return i;});
order.sort(function(i,j){var c=cmpVec(runs[i].z,runs[j].z);return c!==0?c:i-j;});
for(var oi=0;oi<order.length;oi++){runs[order[oi]].z=oi;}

var de=document.documentElement||{};
var sx=(window.pageXOffset||de.scrollLeft||0);
var sy=(window.pageYOffset||de.scrollTop||0);
return JSON.stringify({gen:gen,sx:sx,sy:sy,runs:runs,fixed:fixed});
})(__NYALA_TEXT_WALK_CONFIG__);
