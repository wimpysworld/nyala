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
var fixedCache=new Map();

// A 'fixed' ancestor holds its screen rect while the page scrolls, so the
// compose-time shift can tell viewport-anchored text from flow text. 'sticky' is
// left out on purpose: a stuck element is anchored but an unstuck one scrolls
// with the page, and telling them apart needs a second layout measurement the
// mid-scroll path cannot afford.
function collectFixed(el){
if(fixed.length>=FIXED_CAP)return;
for(var a=el;a;a=a.parentElement){
if(fixedCache.has(a))return;
fixedCache.set(a,1);
var s=gcs(a);
if(s.position!=='fixed')continue;
// A negative z-index paints the element behind content, so it can never cover.
var z=parseInt(s.zIndex,10);if(!isNaN(z)&&z<0)continue;
var rc=a.getBoundingClientRect();
if(rc.width<=0||rc.height<=0)continue;
fixed.push({l:rc.left,t:rc.top,r:rc.right,b:rc.bottom,o:bgOf(a,null).a>=255});
if(fixed.length>=FIXED_CAP)return;
}
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

function flexItem(el){
var p=el.parentElement;if(!p)return false;
var d=gcs(p).display;
return d==='flex'||d==='inline-flex'||d==='grid'||d==='inline-grid';
}

function stacks(el){
if(el===document.documentElement)return true;
var s=gcs(el);
if(s.position&&s.position!=='static')return true;
if(s.zIndex&&s.zIndex!=='auto'&&flexItem(el))return true;
if(parseFloat(s.opacity)<1)return true;
if(s.mixBlendMode&&s.mixBlendMode!=='normal')return true;
if(nn(s.transform)||nn(s.scale)||nn(s.rotate)||nn(s.translate)||nn(s.perspective))return true;
if(nn(s.filter)||nn(s.backdropFilter))return true;
if(nn(s.clipPath))return true;
if(nn(s.mask)||nn(s.maskImage)||nn(s.maskBorder))return true;
if(s.isolation==='isolate')return true;
if(/\b(layout|paint|strict|content)\b/.test(s.contain||''))return true;
if(s.contentVisibility==='auto')return true;
if(/\b(opacity|transform|scale|rotate|translate|perspective|filter|backdrop-filter|clip-path|mask|mask-image|mask-border|isolation|mix-blend-mode|contain|content-visibility|position|z-index|view-transition-name)\b/.test(s.willChange||''))return true;
if(nn(s.viewTransitionName))return true;
return false;
}

var partCache=new Map();

function participates(el){
if(!partCache.has(el)){partCache.set(el,stacks(el));}
return partCache.get(el);
}

function zOf(el){
var s=gcs(el);
if(s.position==='static'&&!flexItem(el))return 0;
var z=parseInt(s.zIndex,10);return isNaN(z)?0:z;
}

var vecCache=new Map();

function stackVector(el){
if(vecCache.has(el))return vecCache.get(el);
var chain=[],base=null;
for(var a=el;a;a=a.parentElement){if(vecCache.has(a)){base=vecCache.get(a);break;}chain.push(a);}
if(!base)base=[];
for(var i=chain.length-1;i>=0;i--){var n=chain[i];base=participates(n)?base.concat([[zOf(n),ord(n)]]):base;vecCache.set(n,base);}
return base;
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
collectFixed(el);
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
runs.push({t:t,c:Math.round(ln.left/cw),r:Math.round(ln.top/ch),x:ln.left,y:ln.top,w:ln.right-ln.left,h:h,fg:fg,bg:bg,z:sv});
}
}

// Each run carries its stacking vector until the sort below replaces it with the
// dense front-to-back rank, 0 being front-most. The array itself stays in DOM order.
var order=runs.map(function(_,i){return i;});
order.sort(function(i,j){var c=cmpVec(runs[i].z,runs[j].z);return c!==0?c:i-j;});
for(var oi=0;oi<order.length;oi++){runs[order[oi]].z=oi;}

var gen=(window[config.generationProperty]||0);
var de=document.documentElement||{};
var sx=(window.pageXOffset||de.scrollLeft||0);
var sy=(window.pageYOffset||de.scrollTop||0);
return JSON.stringify({gen:gen,sx:sx,sy:sy,runs:runs,fixed:fixed});
})(__NYALA_TEXT_WALK_CONFIG__);
