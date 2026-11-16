// Render the real App with public demonstration data; never opens a backend.
// bun capture-readme.ts OUTPUT_DIRECTORY [PNG_DIRECTORY]
import { copyFile, mkdir, readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import type { Backend, Reply, Server, Status } from "./bridge";
const output = process.argv[2];
if (!output) throw new Error("Expected capture output directory");
await mkdir(output, { recursive: true });
const status: Status = { runtime:"native", installed:true, vpn_state:"active", gateway_state:"healthy", subscription:"configured", routing_mode:"smart", networkmanager_configured:"yes", networkmanager_active:"yes", diagnostics:"ok" };
const servers: Server[] = [
  ["Demo-01", 28, 84.6], ["Demo-02", 35, 72.3], ["Demo-03", 42, 61.8], ["Demo-04", 186, 39.4],
].map(([name, latency, speed], i) => ({ profile_id:"demo-subscription", server_id:`srv_${String(i+1).repeat(27)}`, display_name:String(name), country:["NL", "DE", "FI", "JP"][i], selected:i===0, status:"ready", ping_status:"ready", availability:"ready", latency_ms:Number(latency), download_mbps:Number(speed), download_seconds:3 }));
const profiles: NonNullable<Reply["profiles"]> = { active:"demo-subscription", items:[
  {id:"demo-subscription",name:"Моя подписка",kind:"subscription",count:4,stale:false},
  {id:"demo-vps",name:"Мой VPS · Hysteria2",kind:"server",count:1,stale:false},
  {id:"demo-backup",name:"Резервная подписка",kind:"subscription",count:6,stale:false},
] };
const backend: Backend = {
  async request(action) { return {ok:true,reason:"ok",code:0,status, profiles,
    ...(action.startsWith("servers/") ? {catalog:{status:"ok",servers}} : {})}; },
  async close() {},
};
const t = await createTestRenderer({width:100,height:20});
const app = new App(t.renderer,backend);
try {
  app.target = "https://example.com/";
  await app.perform("status");
  await app.perform("profiles/list");
  await app.perform("servers/list");
  for (const server of app.servers) Object.assign(server, servers.find(item => item.server_id === server.server_id));
  app.notice = "";
  app.paint();
  for (const [name, key] of [["home", ""], ["servers", "v"], ["server-menu", ""], ["profiles", "c"]]) {
    if (name === "profiles") { t.mockInput.pressEscape(); await Bun.sleep(30); }
    if (key) { t.resize(100, 24); t.mockInput.pressKey(key); }
    await Bun.sleep(40);
    if (name === "server-menu") { t.mockInput.pressEnter(); await Bun.sleep(40); }
    await t.renderOnce();
    const frame = t.captureSpans();
    await Bun.write(`${output}/${name}.json`, JSON.stringify({ ...frame, lines:frame.lines.map(line => ({spans:line.spans.map(span => ({...span,fg:span.fg.toInts(),bg:span.bg.toInts()}))})) }));
  }
} finally { await app.close(); }

// Reuse the repository's captureSpans rasterizer for all four frames.
const rasterizer = await readFile(resolve(import.meta.dir, "../../../docs/render-tui-captures.py"), "utf8");
const script = rasterizer.replace('("home", "servers", "profiles")', '("home", "servers", "server-menu", "profiles")');
const result = Bun.spawnSync(["python3", "-c", script, output, "/usr/share/fonts/noto/NotoSansMono-Regular.ttf"], { stdout: "inherit", stderr: "inherit" });
if (result.exitCode !== 0) throw new Error(`Capture rasterizer failed: ${result.exitCode}`);
if (process.argv[3]) {
  await mkdir(process.argv[3], { recursive: true });
  for (const name of ["home", "servers", "server-menu", "profiles"])
    await copyFile(`${output}/${name}.png`, `${process.argv[3]}/${name}.png`);
}
