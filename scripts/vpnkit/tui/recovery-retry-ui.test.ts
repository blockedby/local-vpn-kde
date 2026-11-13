import { test, expect } from "bun:test";
import { AppState } from "./app-state";
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

test("home speed recovery retries captured server after selection and updates gauge time",async()=>{
 const b=new BackendStub();const app=new AppState(b);const s=app;
 try{
  await app.perform("status");b.fail="servers/speed";await s.testHomeSpeed();expect(s.homeSpeedAt).toBe(0);
  b.fail=undefined;await app.perform("servers/select",rows[1]!.server_id);
  s.onKey({name:"f",preventDefault(){}});expect(s.items().some(x=>x.key==="r")).toBe(true);s.onKey({name:"r",preventDefault(){}});
  for(let i=0;i<50&&s.homeTesting;i++)await Bun.sleep(5);
  s.onKey({name:"h",preventDefault(){}});
  expect(b.calls.filter(c=>c.action==="servers/speed").at(-1)?.value).toBe(rows[0]!.server_id);
  expect(s.homeSpeed).toBe(80);expect(s.homeSpeedAt).toBeGreaterThan(0);
  expect(s.homeSpeedServer).toEqual({id:rows[0]!.server_id,name:"Node 1"});
  expect(s.servers.find(row=>row.selected)?.server_id).toBe(rows[1]!.server_id);
 }finally{await app.close();}
});

test("batch recovery restores streamed progress and final availability on captured ids",async()=>{
 const b=new BackendStub();const app=new AppState(b);const s=app;let finish!:(r:Reply)=>void;
 try{
  await app.perform("status");await app.perform("servers/list");s.open("servers");b.fail="servers/check-batch";await s.runBatch("ping");
  const captured=b.calls.filter(c=>c.action==="servers/check-batch").at(-1)!.value;
  b.fail=undefined;b.hold=new Promise(r=>finish=r);s.onKey({name:"f",preventDefault(){}});s.onKey({name:"r",preventDefault(){}});await Bun.sleep(10);
  expect(s.batch).toBeDefined();expect(b.calls.filter(c=>c.action==="servers/check-batch").at(-1)!.value).toBe(captured);
  b.onCheckProgress?.({event:"server-check",server_id:rows[0]!.server_id,stage:"complete",ping_status:"ready",latency_ms:17,availability:"ready"});
  expect(s.batch!.done).toBe(1);expect(s.servers[0].availability).toBe("ready");
  finish({...ok(),catalog:{status:"ok",servers:rows.map(v=>({...v,ping_status:"ready",latency_ms:17,availability:"ready"}))}});
  for(let i=0;i<50&&s.batch;i++)await Bun.sleep(5);
  s.onKey({name:"v",preventDefault(){}});expect(s.batch).toBeUndefined();expect(s.servers.every((v:Server)=>v.availability==="ready")).toBe(true);
 }finally{finish?.(ok());await app.close();}
});

test("cancelled operation clears preceding recovery and shows cancellation",async()=>{
 const b=new BackendStub();const app=new AppState(b);const s=app;
 try{
  await app.perform("status");b.fail="start";b.reason="dns-failed";await app.perform("start");expect(s.recovery!.stage).toBe("dns");
  b.reason="cancelled";const result=await app.perform("start");expect(result?.reason).toBe("cancelled");expect(s.noticeIdentity).toEqual({action:"start",reason:"cancelled",ok:false});expect(s.error).toBe(true);expect(s.recovery).toBeUndefined();
 }finally{await app.close();}
});

test("retry confirmation keeps captured operation when recovery changes",async()=>{
 const b=new BackendStub();const state=new AppState(b);
 const key=(name:string)=>state.onKey({name,preventDefault(){}});
 try{
  await state.perform("status");b.fail="start";b.reason="timeout";await state.perform("start");
  key("f");key("r");expect(state.screen).toBe("retry-confirm");expect(state.selected).toBe(0);
  b.fail=undefined;state.recovery={action:"stop",stage:"docker",message:"new error",actions:["retry"]};key("y");await Bun.sleep(10);
  expect(b.calls.at(-1)?.action).toBe("start");expect(b.calls.some(c=>c.action==="stop")).toBe(false);
 }finally{await state.close();}
});
