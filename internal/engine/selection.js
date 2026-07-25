(function(max){
// frame bounds what crosses the CDP wire. A selection longer than max is
// refused here rather than shipped and dropped later, so a select-all on a huge
// document never moves megabytes for a copy the clipboard write will not
// accept. The cap counts UTF-16 units, so the caller re-checks the byte length
// the clipboard sequence is really bounded by.
function frame(text){
if(text.length>max)return {text:'',over:true};
return {text:text,over:false};
}

// controlSelection reads a focused text control's own selection. A control
// keeps it in an inner editor the document selection cannot see, so a selection
// in an input or a textarea would otherwise read as empty. A control whose type
// does not support selection throws on the read, which leaves the document
// selection as the answer.
function controlSelection(){
var el=document.activeElement;
try{
if(el&&typeof el.selectionStart==='number'&&el.selectionEnd>el.selectionStart){
return String(el.value).slice(el.selectionStart,el.selectionEnd);
}
}catch(err){}
return '';
}

var text=controlSelection();
if(text===''){
var selection=window.getSelection();
text=selection?selection.toString():'';
}
return frame(text);
})(__NYALA_SELECTION_MAX__);
