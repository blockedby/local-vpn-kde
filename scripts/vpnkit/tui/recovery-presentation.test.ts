import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import type { Action, Backend, CheckProgress, Reply, Server, Status } from "./bridge";
const status:Status={vpn_state:"active",gateway_state:"healthy",subscription:"configured",routing_mode:"smart",networkmanager_configured:"yes",networkmanager_active:"yes",diagnostics:"not-run"};
const rows:Server[]=[1,2].map(n=>({server_id:`srv_${String(n).repeat(27)}`,display_name:`Node ${n}`,selected:n===1,status:"untested"}));
const ok=():Reply=>({ok:true,reason:"ok",code:0,status:{...status}});
class BackendStub implements Backend {
 rows=structuredClone(rows); calls:{action:Action,value?:string}[]=[];
 fail?:Action; reason="failed"; hold?:Promise<Reply>;
 onCheckProgress?:(p:CheckProgress,id?:string)=>void;
 async request(action:Action,value?:string):Promise<Reply>{
  this.calls.push({action,value});
  if(action===this.fail)return {...ok(),ok:false,reason:this.reason};
  if(action==="servers/check-batch" && this.hold)return this.hold;
  if(action==="servers/list" || action==="servers/refresh")return {...ok(),catalog:{status:"ok",servers:structuredClone(this.rows)}};
  if(action==="servers/select")this.rows=this.rows.map(s=>({...s,selected:s.server_id===value}));
  if(["servers/ping","servers/speed","servers/select"].includes(action))return {...ok(),catalog:{status:"ok",server:{...this.rows.find(s=>s.server_id===value)!,ping_status:"ready",latency_ms:21,status:"ready",download_mbps:80}}};
  return ok();
 }
 cancel(){} async close(){}
}

test("presentation: home speed recovery retries captured server after selection and updates gauge time",async()=>{
 const t=await createTestRenderer({width:65,height:24});const b=new BackendStub();const app=new App(t.renderer,b);const s=app as any;
 try{
  await app.perform("status");b.fail="servers/speed";await s.testHomeSpeed();
  b.fail=undefined;await app.perform("servers/select",rows[1]!.server_id);
  s.open("recovery");const retry=s.items().find((x:any)=>x.key==="r");retry.run();
  for(let i=0;i<50&&s.homeTesting;i++)await Bun.sleep(5);
  s.open("home");await t.renderOnce();
  expect(t.captureCharFrame()).toContain("Node 1");expect(t.captureCharFrame().split("\n").find((line:string)=>line.includes("Node 1") && line.includes("80.0"))).toBeDefined();
  expect(t.captureCharFrame().split("\n").find((line:string)=>line.includes("Node 2") && line.includes("●"))).toBeDefined();
 }finally{await app.close();}
});

test("presentation: batch recovery restores streamed progress and final availability on captured ids",async()=>{
 const t=await createTestRenderer({width:110,height:30});const b=new BackendStub();const app=new App(t.renderer,b);const s=app as any;let finish!:(r:Reply)=>void;
 try{
  await app.perform("status");await app.perform("servers/list");s.open("servers");b.fail="servers/check-batch";await s.runBatch("ping");
  b.fail=undefined;b.hold=new Promise(r=>finish=r);s.open("recovery");s.items().find((x:any)=>x.key==="r").run();await Bun.sleep(10);
  b.onCheckProgress?.({event:"server-check",server_id:rows[0]!.server_id,stage:"complete",ping_status:"ready",latency_ms:17,availability:"ready"});
  finish({...ok(),catalog:{status:"ok",servers:rows.map(v=>({...v,ping_status:"ready",latency_ms:17,availability:"ready"}))}});
  for(let i=0;i<50&&s.batch;i++)await Bun.sleep(5);
  s.open("servers");await t.renderOnce();expect(t.captureCharFrame()).toContain("да");
 }finally{finish?.(ok());await app.close();}
});

test("presentation: cancelled operation clears preceding recovery and shows cancellation",async()=>{
 const t=await createTestRenderer({width:100,height:30});const b=new BackendStub();const app=new App(t.renderer,b);const s=app as any;
 try{
  await app.perform("status");b.fail="start";b.reason="dns-failed";await app.perform("start");
  b.reason="cancelled";await app.perform("start");await t.renderOnce();expect(t.captureCharFrame()).toContain("Действие отменено");expect(t.captureCharFrame()).not.toContain("Проверка DNS");
 }finally{await app.close();}
});
