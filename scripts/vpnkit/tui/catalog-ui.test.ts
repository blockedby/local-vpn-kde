import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import type { Action, Backend, CheckProgress, Reply, Server, Status } from "./bridge";
const status: Status = { vpn_state:"active", gateway_state:"healthy", subscription:"configured", routing_mode:"smart", networkmanager_configured:"yes", networkmanager_active:"yes", diagnostics:"not-run" };
const row = (n:number):Server => ({server_id:`srv_${String(n).padStart(27,"0")}`,display_name:`Node ${n}`,status:"untested",selected:n===1});
const success = ():Reply => ({ok:true,reason:"ok",code:0,status:{...status}});
class CatalogBackend implements Backend {
 rows = [row(1),row(2),row(3)]; hold?:Promise<Reply>;
 onProgress?:(phase:string)=>void; onCheckProgress?:(p:CheckProgress)=>void;
 async request(action:Action,value?:string):Promise<Reply> {
  if(action==="servers/check-batch" && this.hold) return this.hold;
  const r=success();
  if(action==="servers/list" || action==="servers/refresh") r.catalog={status:"ok",servers:structuredClone(this.rows)};
  if(["servers/ping","servers/speed","servers/select"].includes(action)) {
   if(action==="servers/select") this.rows=this.rows.map(s=>({...s,selected:s.server_id===value}));
   r.catalog={status:"ok",server:{...this.rows.find(s=>s.server_id===value)!,status:"ready",ping_status:"ready",latency_ms:25,download_mbps:80}};
  }
  return r;
 }
 cancel() {} async close() {}
}

test("refresh retains measured fields only for unchanged identity, including rename",async()=>{
 const t=await createTestRenderer({width:110,height:30}); const b=new CatalogBackend(); const app=new App(t.renderer,b); const s=app as any;
 try {
  await app.perform("status"); await app.perform("servers/list"); await app.perform("servers/ping",row(1).server_id); await app.perform("servers/speed",row(1).server_id);
  s.screen="servers"; s.serverID=row(1).server_id;
  b.rows=[{...row(1),display_name:"Renamed node"},{...row(4),display_name:"Node 2",download_mbps:999,latency_ms:999,status:"failed"}];
  await app.perform("servers/refresh"); await t.renderOnce();
  const frame=t.captureCharFrame(); expect(frame).toContain("Renamed node"); expect(frame).toContain("80.0"); expect(frame).not.toContain("999");
  expect(s.serverID).toBe(row(1).server_id);
  expect(s.servers.find((v:Server)=>v.server_id===row(1).server_id)).toMatchObject({latency_ms:25,download_mbps:80});
  const fresh=s.servers.find((v:Server)=>v.server_id===row(4).server_id);
  expect(fresh).toMatchObject({status:"untested",ping_status:"untested",availability:"untested"}); expect(fresh.download_mbps).toBeUndefined();
 } finally {await app.close();}
});

test("refresh retains viewport anchor and moves removed cursor to nearest row",async()=>{
 const t=await createTestRenderer({width:100,height:20}); const b=new CatalogBackend(); b.rows=Array.from({length:15},(_,i)=>row(i+1)); const app=new App(t.renderer,b); const s=app as any;
 try {
  await app.perform("status"); await app.perform("servers/list"); s.screen="servers"; s.serverID=row(7).server_id; s.viewStart=4; s.paint(); await t.renderOnce(); expect(t.captureCharFrame()).toContain("Node 5");
  b.rows=b.rows.filter(v=>v.server_id!==row(7).server_id); await app.perform("servers/refresh"); await t.renderOnce();
  expect(s.serverID).toBe(row(8).server_id); expect(s.viewStart).toBe(4); expect(t.captureCharFrame()).toContain("Node 5"); expect(t.captureCharFrame()).not.toMatch(/Node 7\s/);
 } finally {await app.close();}
});

test("ping sort freezes during progress and reorders when the pass completes",async()=>{
 const t=await createTestRenderer({width:110,height:30}); const b=new CatalogBackend(); let finish!:(r:Reply)=>void; b.hold=new Promise(r=>finish=r); const app=new App(t.renderer,b); const s=app as any;
 try {
  await app.perform("status"); await app.perform("servers/list"); s.screen="servers"; s.sort="ping"; s.servers=s.servers.map((v:Server,i:number)=>({...v,latency_ms:10+i*10,ping_status:"ready"}));
  const run=s.runBatch("ping"); await Bun.sleep(10); const order=()=>s.sortedServers().map((v:Server)=>v.server_id);
  const before=[row(1).server_id,row(2).server_id,row(3).server_id]; expect(order()).toEqual(before);
  b.onCheckProgress?.({event:"server-check",server_id:row(3).server_id,stage:"complete",ping_status:"ready",latency_ms:1,availability:"ready"}); await t.renderOnce();
  expect(order()).toEqual(before); expect(t.captureCharFrame().indexOf("Node 1")).toBeLessThan(t.captureCharFrame().indexOf("Node 3"));
  finish({...success(),catalog:{status:"ok",servers:b.rows.map((v,i)=>({...v,ping_status:"ready",latency_ms:i===2?1:10+i*10,availability:"ready"}))}}); await run; await t.renderOnce();
  expect(order()[0]).toBe(row(3).server_id); expect(t.captureCharFrame().indexOf("Node 3")).toBeLessThan(t.captureCharFrame().indexOf("Node 1"));
 } finally {finish(success()); await app.close();}
});

test("home result retains source and completion time after selection and resize",async()=>{
 const t=await createTestRenderer({width:65,height:24}); const b=new CatalogBackend(); const app=new App(t.renderer,b); const s=app as any;
 try {
  await app.perform("status"); await s.testHomeSpeed(); const at=s.homeSpeedAt; expect(at).toBeGreaterThan(0); const stamp=new Date(at).toLocaleTimeString("ru-RU",{hour12:false});
  await t.renderOnce(); expect(t.captureCharFrame()).toContain(stamp); expect(t.captureCharFrame()).toContain("Node 1");
  await app.perform("servers/select",row(2).server_id); await t.renderOnce(); expect(t.captureCharFrame()).toContain("другой сервер"); expect(t.captureCharFrame()).toContain("Node 1");
  t.resize(100,35); await t.renderOnce(); expect(t.captureCharFrame()).toContain("Node 1"); expect(t.captureCharFrame()).toContain(stamp); expect(t.captureCharFrame()).toContain("СКОРОСТЬ СЕРВЕРА");
  t.resize(65,24); await t.renderOnce(); expect(t.captureCharFrame()).toContain("другой сервер"); expect(t.captureCharFrame()).toContain(stamp); expect(s.homeSpeedAt).toBe(at);
 } finally {await app.close();}
});
