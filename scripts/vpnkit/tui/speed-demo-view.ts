import { ASCIIFontRenderable, BoxRenderable, TextRenderable, StyledText, fg, type CliRenderer } from "@opentui/core";
const cyan = "#65dce8", green = "#91edbc", muted = "#8296af";
export function demoSpeed(ms: number) {
  const t = Math.max(0, Math.min(ms, 9000)) / 1000;
  return (1 - Math.exp(-t / 1.4)) * (147 + 21 * Math.sin(t * 1.8) + 9 * Math.sin(t * 4.1));
}
// Braille provides a 2 x 4 pixel canvas per terminal cell.
function dial(width: number, rows: number, speed: number) {
  const bits = Array.from({length: rows}, () => Array<number>(width).fill(0));
  const colors = Array.from({length: rows}, () => Array<string>(width).fill(muted));
  const cx = width - 1, cy = rows * 4 - 3, rx = width - 5, ry = cy - 3;
  const masks = [[1,8],[2,16],[4,32],[64,128]];
  const dot = (x: number, y: number, color: string) => {
    x = Math.round(x); y = Math.round(y);
    if (x < 0 || y < 0 || x >= width * 2 || y >= rows * 4) return;
    bits[y >> 2]![x >> 1]! |= masks[y % 4]![x % 2]!;
    colors[y >> 2]![x >> 1] = color;
  };
  for (let i = 0; i <= 500; i++) {
    const a = Math.PI * (1 - i / 500), color = i < 250 ? cyan : i < 400 ? green : "#f5cc86";
    for (const r of [1, .98]) dot(cx + rx*r*Math.cos(a), cy - ry*r*Math.sin(a), color);
  }
  for (let i = 0; i <= 10; i++) {
    const a = Math.PI*(1-i/10);
    for (let r = .86; r < .93; r += .01) dot(cx+rx*r*Math.cos(a),cy-ry*r*Math.sin(a),muted);
  }
  const a = Math.PI * (1-Math.min(1,Math.max(0,speed)/250));
  for (let r = 0; r < .8; r += .004) dot(cx+rx*r*Math.cos(a),cy-ry*r*Math.sin(a),"#edf6ff");
  for (let a = 0; a < 6.3; a += .1) dot(cx+2*Math.cos(a),cy+2*Math.sin(a),"#edf6ff");
  return new StyledText(bits.flatMap((line,y) => [...line.map((v,x) => fg(colors[y]![x]!)(v ? String.fromCharCode(0x2800+v) : " ")), fg(muted)(y < rows-1 ? "\n" : "")]));
}
export class SpeedDemoView {
  private panel: BoxRenderable;
  private gauge: TextRenderable;
  private digits: ASCIIFontRenderable;
  private chart: TextRenderable;
  private progress: TextRenderable;
  private status: TextRenderable;
  private needle = 0;
  private last = 0;
  constructor(private renderer: CliRenderer) {
    const root = new BoxRenderable(renderer,{width:"100%",height:"100%",justifyContent:"center",alignItems:"center",backgroundColor:"#080e1a"});
    renderer.root.add(root);
    this.panel = new BoxRenderable(renderer,{border:true,borderStyle:"rounded",borderColor:"#30465e",backgroundColor:"#101c2c",padding:1,flexDirection:"column",alignItems:"center",flexShrink:0});
    root.add(this.panel);
    const text = (content: string, color: string) => {
      const item = new TextRenderable(renderer,{content,fg:color,height:1,flexShrink:0});
      this.panel.add(item); return item;
    };
    text("VPNKIT  /  SPEED LAB",cyan);
    text("ДЕМО · симуляция",muted);
    this.gauge = text("",cyan);
    text("0              125              250",muted);
    this.digits = new ASCIIFontRenderable(renderer,{text:"0",font:"tiny",color:[cyan,green],flexShrink:0});
    this.panel.add(this.digits);
    text("Мбит/с",muted);
    this.chart = text("",cyan);
    this.progress = text("",green);
    this.status = text("", "#edf6ff");
    text("Enter — ещё раз · Esc — выход",muted);
    this.render(0);
  }
  render(ms: number) {
    const compact = this.renderer.height < 30;
    const width = Math.max(26,Math.min(56,this.renderer.width-10)), rows = compact ? 7 : 13;
    this.panel.width = Math.min(68,this.renderer.width);
    this.gauge.height = rows;
    this.chart.visible = !compact;
    const elapsed = Math.max(0,Math.min(9000,ms));
    if (ms < this.last) { this.needle = 0; this.last = 0; }
    this.needle += (demoSpeed(elapsed)-this.needle)*(1-Math.exp(-Math.max(0,ms-this.last)/140));
    this.last = ms;
    this.gauge.content = dial(width,rows,this.needle);
    this.digits.text = demoSpeed(elapsed).toFixed(1);
    const cells = Math.min(34,width), completed = Math.floor(elapsed/9000*cells);
    this.progress.content = new StyledText([fg(green)("━".repeat(completed)),fg("#30465e")("━".repeat(cells-completed))]);
    const blocks = "▁▂▃▄▅▆▇█";
    this.chart.content = Array.from({length:cells},(_,i) => {
      const time = i/(cells-1)*9000;
      return time > elapsed ? "·" : blocks[Math.min(7,Math.floor(demoSpeed(time)/250*8))]!;
    }).join("");
    this.status.content = elapsed < 9000 ? `Измерение  ${(elapsed/1000).toFixed(1)} / 9 с` : "Замер завершён";
    this.renderer.requestRender();
  }
}
