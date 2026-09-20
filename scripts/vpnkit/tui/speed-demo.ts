import { createCliRenderer } from "@opentui/core";
import { SpeedDemoView } from "./speed-demo-view";
if (!process.stdin.isTTY || !process.stdout.isTTY) {
  console.error("Запустите демо в терминале.");
  process.exit(2);
}
const renderer = await createCliRenderer({exitOnCtrlC:false,exitSignals:[],targetFps:30,backgroundColor:"#080e1a"});
const view = new SpeedDemoView(renderer);
let start = performance.now(), closed = false;
const timer = setInterval(() => view.render(performance.now()-start),1000/30);
const close = () => { if (closed) return; closed = true; clearInterval(timer); renderer.destroy(); };
renderer.keyInput.on("keypress",key => {
  if (key.name === "escape" || key.name === "q" || (key.ctrl && key.name === "c")) close();
  if (key.name === "return" || key.name === "space") { start = performance.now(); view.render(0); }
});
for (const signal of ["SIGINT","SIGTERM","SIGHUP"] as const) process.on(signal,close);
