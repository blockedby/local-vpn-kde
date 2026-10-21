import { expect, test } from "bun:test";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { Bridge, DemoBackend } from "./bridge";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

function fixture() {
  const child = Object.assign(new EventEmitter(), {
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: 0, signalCode: null,
  });
  const writes: { id: string; action: string }[] = [];
  child.stdin.on("data", (chunk) => {
    for (const line of chunk.toString().trim().split("\n")) writes.push(JSON.parse(line));
  });
  const bridge = new Bridge([], child as unknown as ChildProcessWithoutNullStreams);
  const send = (value: object) => child.stdout.write(JSON.stringify(value) + "\n");
  const finish = (id: string, reason = id) =>
    send({ id, ok: true, reason, code: 0, status: { vpn_state: "healthy" } });
  return { bridge, child, writes, send, finish };
}

test("bridge resolves concurrent replies by ID in reverse order", async () => {
  const f = fixture();
  const first = f.bridge.request("servers/speed", "a", "speed");
  const second = f.bridge.request("servers/select", "b", "select");
  f.finish("select");
  expect((await second).reason).toBe("select");
  f.finish("speed");
  expect((await first).reason).toBe("speed");
  expect(f.writes.map((r) => r.id)).toEqual(["speed", "select"]);
  await f.bridge.close();
});

test("cancellation targets one request and preserves its peer", async () => {
  const f = fixture();
  const first = f.bridge.request("servers/speed", "a", "speed");
  const second = f.bridge.request("servers/select", "b", "select");
  f.bridge.cancel("speed");
  expect(f.writes.at(-1)).toEqual({ action: "cancel", id: "speed" });
  f.finish("speed", "cancelled");
  expect((await first).reason).toBe("cancelled");
  f.finish("select");
  expect((await second).reason).toBe("select");
  await f.bridge.close();
});

test("progress carries request IDs and ignores late events", async () => {
  const f = fixture();
  const phases: string[] = [], checks: string[] = [];
  f.bridge.onProgress = (phase, id) => phases.push(id + ":" + phase);
  f.bridge.onCheckProgress = (_, id) => checks.push(id!);
  const a = f.bridge.request("servers/check-batch", "{}", "check");
  const b = f.bridge.request("servers/select", "b", "select");
  f.send({ id: "select", event: "progress", phase: "nm-work" });
  const event = {
    id: "check", event: "server-check", server_id: "srv_" + "a".repeat(27),
    stage: "ping", ping_status: "ready", latency_ms: 12, availability: "untested",
  };
  f.send(event);
  f.finish("check"); await a;
  f.send(event);
  f.send({ id: "check", event: "progress", phase: "late" });
  f.finish("select"); await b;
  expect(phases).toEqual(["select:nm-work"]);
  expect(checks).toEqual(["check"]);
  await f.bridge.close();
});

test("close rejects all pending requests and prevents new work", async () => {
  const f = fixture();
  const outcomes = Promise.allSettled([
    f.bridge.request("servers/speed"), f.bridge.request("servers/list"),
  ]);
  await f.bridge.close();
  for (const result of await outcomes) {
    expect(result.status).toBe("rejected");
    if (result.status === "rejected") expect(result.reason.message).toBe("backend-unavailable");
  }
  expect(f.writes.filter((r) => r.action === "cancel")).toHaveLength(2);
  await expect(f.bridge.request("status")).rejects.toThrow("backend-unavailable");
});

test("duplicate IDs cannot replace an existing request", async () => {
  const f = fixture();
  const first = f.bridge.request("status", undefined, "same");
  await expect(f.bridge.request("status", undefined, "same")).rejects.toThrow("duplicate-request-id");
  f.finish("same");
  expect((await first).ok).toBe(true);
  await f.bridge.close();
});

test("demo cancellation is scoped to its task", async () => {
  const b = new DemoBackend();
  const speed = b.request("servers/speed", b.servers[0].server_id, "speed");
  const select = b.request("servers/select", b.servers[1].server_id, "select");
  b.cancel("speed");
  expect((await speed).reason).toBe("cancelled");
  expect((await select).ok).toBe(true);
  expect(b.servers[1].selected).toBe(true);
  await b.close();
});

test("backend exit rejects every concurrent waiter", async () => {
  const f = fixture();
  const outcomes = Promise.allSettled([
    f.bridge.request("servers/speed", "a", "speed"),
    f.bridge.request("servers/select", "b", "select"),
  ]);
  f.child.emit("exit", 1, null);
  expect((await outcomes).map((result) => result.status)).toEqual(["rejected", "rejected"]);
  await expect(f.bridge.request("status")).rejects.toThrow("backend-unavailable");
  await f.bridge.close();
});

test("speed samples are request scoped and stop after the final response", async () => {
  const f = fixture();
  const id = "srv_" + "s".repeat(27);
  const samples: string[] = [];
  f.bridge.onSpeedProgress = (row, requestId) => samples.push(requestId + ":" + row.downloaded_bytes);
  const task = f.bridge.request("servers/speed", id, "speed");
  const event = { id: "speed", event: "speed-progress", server_id: id, downloaded_bytes: 1024, elapsed_seconds: 0.2, download_mbps: 0.04096 };
  f.send(event);
  f.finish("speed"); await task;
  f.send(event);
  expect(samples).toEqual(["speed:1024"]);
  await f.bridge.close();
});

test("malformed or wrong-action speed samples fail the connection", async () => {
  for (const action of ["status", "servers/speed"] as const) {
    const f = fixture();
    const id = "srv_" + "s".repeat(27);
    const outcome = f.bridge.request(action, id, "sample").catch((error: Error) => error.message);
    f.send({ id: "sample", event: "speed-progress", server_id: id, downloaded_bytes: action === "status" ? 1 : -1, elapsed_seconds: 1, download_mbps: 1 });
    expect(await outcome).toBe("backend-unavailable");
    await f.bridge.close();
  }
});

test("explicit recovery replaces a real exited child without replaying interrupted mutation", async () => {
  const { spawn } = await import("node:child_process");
  let starts = 0;
  const children: ReturnType<typeof spawn>[] = [];
  const source = `
    const readline = require("node:readline");
    const seen = [];
    readline.createInterface({input:process.stdin}).on("line", line => {
      const r = JSON.parse(line);
      if (r.action === "cancel") return;
      seen.push(r.action);
      if (r.action === "servers/select") { process.exit(19); return; }
      process.stdout.write(JSON.stringify({id:r.id,ok:true,reason:"ok",code:0,status:{vpn_state:"unknown"},value:JSON.stringify(seen)})+"\\n");
    });
  `;
  const bridge = new Bridge([], () => {
    starts++;
    const child = spawn(process.execPath, ["-e", source], { stdio: "pipe" });
    children.push(child);
    return child;
  });
  let disconnects = 0;
  bridge.onDisconnect = () => disconnects++;
  try {
    await expect(bridge.request("servers/select", "a")).rejects.toThrow("backend-unavailable");
    expect(starts).toBe(1);
    expect(disconnects).toBe(1);
    await expect(bridge.request("status")).rejects.toThrow("backend-unavailable");
    await Promise.all([bridge.reconnect(), bridge.reconnect()]);
    expect(starts).toBe(2);
    const reply = await bridge.request("status");
    expect(JSON.parse(reply.value!)).toEqual(["status"]);
    // Old process events cannot break the new generation.
    children[0].emit("error", new Error("late"));
    expect((await bridge.request("servers/list")).ok).toBe(true);
    children[1].kill("SIGTERM");
    await new Promise<void>((resolve) => children[1].once("exit", () => resolve()));
    expect(disconnects).toBe(2);
    await bridge.reconnect();
    expect((await bridge.request("status")).ok).toBe(true);
  } finally {
    await bridge.close();
  }
  await expect(bridge.reconnect()).rejects.toThrow("backend-unavailable");
});

test("recovery refuses to overlap a child that has not drained", async () => {
  const { spawn } = await import("node:child_process");
  let starts = 0;
  const child = spawn(process.execPath, ["-e", `
    process.stdin.resume();
    process.stdin.on("end", () => {});
    setInterval(() => {}, 1000);
  `], { stdio: "pipe" });
  const bridge = new Bridge([], () => { starts++; return child; });
  const outcome = bridge.request("servers/select", "a").catch((e: Error) => e.message);
  // Simulate a transport error while its operation process remains alive.
  child.stdout.emit("data", "invalid-json\n");
  expect(await outcome).toBe("backend-unavailable");
  try {
    await expect(bridge.reconnect()).rejects.toThrow("backend-shutdown-timeout");
    expect(starts).toBe(1);
  } finally {
    child.kill("SIGTERM");
    await new Promise<void>((resolve) => child.once("exit", () => resolve()));
    await bridge.close().catch(() => {});
  }
}, 8000);

test("close drains a failed spawn which emits close without exit", async () => {
  const child = Object.assign(new EventEmitter(), {
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: null, signalCode: null,
  });
  const bridge = new Bridge([], child as unknown as ChildProcessWithoutNullStreams);
  const request = bridge.request("status");
  child.emit("error", new Error("ENOENT"));
  await expect(request).rejects.toThrow("backend-unavailable");
  const closing = bridge.close();
  child.emit("close", -2, null);
  await closing;
  // Repeated cleanup must also work after the close event was consumed.
  await bridge.close();
});

test("actual Go bridge forwards speed stage and stays available after combined batch", async () => {
  const repo = mkdtempSync(join(tmpdir(), "vpnkit-local-tui-stream-"));
  mkdirSync(join(repo, "scripts/vpnkit"), { recursive: true, mode: 0o700 });
  mkdirSync(join(repo, "secrets/vpnkit-local/vibe-vpn"), { recursive: true, mode: 0o700 });
  const id = "srv_" + "a".repeat(27);
  const event = { event: "server-check", server_id: id, ping_status: "ready", latency_ms: 259, availability: "ready" };
  const frames = [
    { ...event, stage: "start", ping_status: "untested", latency_ms: 0, availability: "untested" },
    { ...event, stage: "ping", availability: "untested" },
    { ...event, stage: "speed" },
    { ...event, stage: "complete", status: "ready", download_mbps: 12, download_seconds: 3, downloaded_bytes: 4500000 },
    { schema: "vibe-vpn.server-browser.v2", status: "ok", servers: [{ ...event, display_name: "Fixture", selected: true, status: "ready", download_mbps: 12, download_seconds: 3, downloaded_bytes: 4500000 }] },
  ];
  const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
  // Exercise the real Go CheckStream and transport with an inert adapter.
  // Checkout-native measurement actions require installed assets, so this
  // shared-protocol fixture uses an explicit adapter override without invoking
  // Docker, privileged helpers, network namespaces or actual measurements.
  writeFileSync(join(repo, "scripts/vpnkit/vpnkit-native.sh"),
    '#!/bin/sh\ncase "$1" in\nstatus) printf \'%s\\n\' ' + quote(JSON.stringify({ container: "healthy", runtime: "native", networkmanager_configured: "yes", networkmanager_active: "yes" })) + ";;\nservers) printf '%s\\n' " + frames.map(frame => quote(JSON.stringify(frame))).join(" ") + ";;\n*) exit 2;;\nesac\n", { mode: 0o700 });
  const bridge = new Bridge(["--repo", repo, "--runtime", "docker", "--lifecycle-executable", join(repo, "scripts/vpnkit/vpnkit-native.sh"), "--secrets-dir", "secrets/vpnkit-local"]);
  const stages: string[] = [];
  bridge.onCheckProgress = row => stages.push(row.stage);
  try {
    const reply = await bridge.request("servers/speed-batch", JSON.stringify({ ids: [id], url: "https://example.com/" }));
    expect(reply.ok).toBe(true);
    expect(stages).toEqual(["start", "ping", "speed", "complete"]);
    expect(reply.catalog?.servers?.[0]?.download_mbps).toBe(12);
    const second = await bridge.request("status");
    expect(second.ok).toBe(true);
    expect(second.status.gateway_state).toBe("healthy");
  } finally {
    await bridge.close();
    rmSync(repo, { recursive: true, force: true });
  }
});

test("bridge rejects unknown stages and speed without completed site evidence", async () => {
  for (const override of [{ stage: "unknown" }, { stage: "speed", availability: "untested" }]) {
    const f = fixture();
    const request = f.bridge.request("servers/speed-batch", "{}", "batch");
    f.send({ id: "batch", event: "server-check", server_id: "srv_" + "a".repeat(27), ping_status: "ready", latency_ms: 259, availability: "ready", ...override });
    await expect(request).rejects.toThrow("backend-unavailable");
    await f.bridge.close();
  }
});
