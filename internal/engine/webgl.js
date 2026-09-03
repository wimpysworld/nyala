(function(){
// attempts lists the context names in preference order with the WebGL version
// each one reports. Chromium answers 'experimental-webgl' with a WebGL 1
// context, so it counts as version 1.
var attempts=[['webgl2',2],['webgl',1],['experimental-webgl',1]];

// text coerces a getParameter answer to a string. A missing parameter reads as
// null, and a driver can answer with a non-string, so both read as empty.
function text(value){
return typeof value==='string'?value:'';
}

// param reads one parameter and swallows a throw, because a lost context throws
// on every read and the probe must still answer with the version it created.
function param(gl,name){
try{return text(gl.getParameter(name));}catch(err){return '';}
}

// identity reads the unmasked vendor and renderer when Chromium exposes
// WEBGL_debug_renderer_info, otherwise the plain VENDOR and RENDERER pair.
function identity(gl){
var ext=null;
try{ext=gl.getExtension('WEBGL_debug_renderer_info');}catch(err){ext=null;}
if(ext){
return {vendor:param(gl,ext.UNMASKED_VENDOR_WEBGL),renderer:param(gl,ext.UNMASKED_RENDERER_WEBGL)};
}
return {vendor:param(gl,gl.VENDOR),renderer:param(gl,gl.RENDERER)};
}

var canvas=document.createElement('canvas');
for(var i=0;i<attempts.length;i++){
var gl=null;
try{gl=canvas.getContext(attempts[i][0]);}catch(err){gl=null;}
if(!gl)continue;
var id=identity(gl);
return {version:attempts[i][1],vendor:id.vendor,renderer:id.renderer};
}
// No context name answered, so the page has no WebGL. This is a result, not a
// failure, so the probe reports version 0 instead of throwing.
return {version:0,vendor:'',renderer:''};
})();
