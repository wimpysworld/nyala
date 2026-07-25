(function(cellHeight){
// A caret key's scroll is Chromium's own default action, so the page, not the
// engine, owns the offset it landed on. When the backend quantises scroll the
// offset snaps to the cell pitch first, so the crisp text overlay stays on the
// grid, and the instant behaviour keeps the read below settled. The settled
// offset is then reported for the engine to adopt as its held offset.
if(cellHeight>0){
var snapped=Math.max(0,Math.round(window.scrollY/cellHeight)*cellHeight);
if(snapped!==window.scrollY)window.scrollTo({left:window.scrollX,top:snapped,behavior:'instant'});
}
return {sx:window.scrollX,sy:window.scrollY};
})(__NYALA_CELL_HEIGHT__);
