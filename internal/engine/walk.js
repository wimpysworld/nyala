(function(config){
var cw=config.cellWidth,ch=config.cellHeight;
var runs=[];
var csCache=new Map();

function gcs(el){
if(!csCache.has(el)){csCache.set(el,getComputedStyle(el));}
return csCache.get(el);
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

function lca(a,b){
for(var n=a;n;n=n.parentElement){if(n.contains(b))return n;}
return null;
}

function occluded(el,x,y){
var top=document.elementFromPoint(x,y);if(!top)return false;
if(top===el||el.contains(top)||top.contains(el))return false;
return bgOf(top,lca(top,el)).a>=255;
}

var w=document.createTreeWalker(document.body||document.documentElement,NodeFilter.SHOW_TEXT,null);
var node;while((node=w.nextNode())){
var el=node.parentElement;if(!el)continue;
if(!node.nodeValue||!node.nodeValue.trim())continue;
if(hiddenStyle(el))continue;
var fg=rgba(el);var bg=bgOf(el);
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
var cx=(ln.left+ln.right)/2,cy=(ln.top+ln.bottom)/2;
if(occluded(el,cx,cy))continue;
runs.push({t:t,c:Math.round(ln.left/cw),r:Math.round(ln.top/ch),x:ln.left,y:ln.top,w:ln.right-ln.left,h:h,fg:fg,bg:bg});
}
}

var gen=(window[config.generationProperty]||0);
var de=document.documentElement||{};
var sx=(window.pageXOffset||de.scrollLeft||0);
var sy=(window.pageYOffset||de.scrollTop||0);
return JSON.stringify({gen:gen,sx:sx,sy:sy,runs:runs});
})(__NYALA_TEXT_WALK_CONFIG__);
