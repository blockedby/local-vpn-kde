import { expect, test } from "bun:test";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { Bridge, DemoBackend } from "./bridge";

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
