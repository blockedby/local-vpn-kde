import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { fileURLToPath } from "node:url";

export type Action =
  | "status"
  | "subscription"
  | "subscription/read"
  | "backend/start"
  | "disconnect"
  | "start"
  | "stop"
  | "retest/select"
  | "toggle-mode"
  | "diagnostics"
  | "servers/list"
  | "servers/refresh"
  | "servers/check-batch"
  | "servers/ping"
  | "servers/speed"
  | "servers/availability"
  | "servers/select";
export interface Status {
  vpn_state: string;
  gateway_state?: string;
  subscription: string;
  routing_mode: string;
  networkmanager_configured: string;
  networkmanager_active: string;
  diagnostics: string;
  last_attempt?: string;
}
export interface Server {
  server_id: string;
  display_name: string;
  selected: boolean;
  status: string;
  ping_status?: string;
  availability?: string;
  latency_ms?: number;
  download_mbps?: number;
  download_seconds?: number;
  downloaded_bytes?: number;
}
export interface Reply {
  ok: boolean;
  reason: string;
  code: number | null;
  status: Status;
  value?: string;
  catalog?: { status: string; servers?: Server[]; server?: Server };
}
export interface CheckProgress {
  event: "server-check";
  server_id: string;
  stage: "start" | "ping" | "complete";
  ping_status: "untested" | "ready" | "failed";
  latency_ms: number;
  availability: "ready" | "failed" | "untested";
}
export interface Backend {
  request(action: Action, value?: string, requestId?: string): Promise<Reply>;
  cancel?(requestId?: string): void;
  onProgress?: (phase: string, requestId?: string) => void;
  onCheckProgress?: (result: CheckProgress, requestId?: string) => void;
  close(): Promise<void>;
}

// Correlate independent operations; the backend serializes conflicting mutations.
export class Bridge implements Backend {
  private child: ChildProcessWithoutNullStreams;
  private pending = new Map<string, {
    resolve: (reply: Reply) => void;
    reject: (error: Error) => void;
  }>();
  private sequence = 0;
  private buffer = "";
  private closed = false;
  onProgress?: (phase: string, requestId?: string) => void;
  onCheckProgress?: (result: CheckProgress, requestId?: string) => void;
  constructor(args: string[] = [], child?: ChildProcessWithoutNullStreams) {
    const root = fileURLToPath(new URL("../../../", import.meta.url));
    this.child = child ?? spawn(
      fileURLToPath(new URL("../../../.build/local-vpn-kde.bin", import.meta.url)),
      ["bridge", "--repo", root, ...args],
      { stdio: "pipe" },
    );
    this.child.stderr.resume();
    this.child.stdout.setEncoding("utf8");
    this.child.stdout.on("data", (chunk: string) => {
      this.buffer += chunk;
      if (Buffer.byteLength(this.buffer) > 2 * 1048576) {
        this.fail();
        return;
      }
      while (this.buffer.includes("\n")) {
        const newline = this.buffer.indexOf("\n");
        const line = this.buffer.slice(0, newline);
        this.buffer = this.buffer.slice(newline + 1);
        try {
          const reply = JSON.parse(line);
          if (typeof reply.id !== "string") throw new Error();
          const pending = this.pending.get(reply.id);
          if (reply.event === "server-check") {
            if (!/^srv_[A-Za-z0-9_-]{27}$/.test(reply.server_id) ||
                !["start", "ping", "complete"].includes(reply.stage) ||
                !(reply.stage === "start" ? reply.ping_status === "untested" && reply.latency_ms === 0 && reply.availability === "untested" : ["ready", "failed"].includes(reply.ping_status)) ||
                !["ready", "failed", "untested"].includes(reply.availability) ||
                !Number.isInteger(reply.latency_ms) || reply.latency_ms < 0 || reply.latency_ms > 3600000) throw new Error();
            if (pending) this.onCheckProgress?.(reply, reply.id);
            continue;
          }
          if (reply.event === "progress" && typeof reply.phase === "string") {
            if (pending) this.onProgress?.(reply.phase, reply.id);
            continue;
          }
          if (
            typeof reply.ok !== "boolean" ||
            typeof reply.status?.vpn_state !== "string"
          )
            throw new Error();
          this.pending.delete(reply.id);
          pending?.resolve(reply);
        } catch {
          this.fail();
        }
      }
    });
    this.child.on("error", () => this.fail());
    this.child.on("exit", () => this.fail());
    this.child.stdin.on("error", () => this.fail());
  }
  private fail() {
    this.closed = true;
    for (const pending of this.pending.values())
      pending.reject(new Error("backend-unavailable"));
    this.pending.clear();
  }
  request(action: Action, value?: string, requestId?: string): Promise<Reply> {
    const id = requestId ?? `bridge-${++this.sequence}`;
    if (this.closed)
      return Promise.reject(new Error("backend-unavailable"));
    if (this.pending.has(id))
      return Promise.reject(new Error("duplicate-request-id"));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.child.stdin.write(
        JSON.stringify({ id, action, value, progress: true }) + "\n",
      );
    });
  }
  cancel(requestId?: string) {
    const ids = requestId === undefined ? [...this.pending.keys()] : [requestId];
    for (const id of ids) {
      if (this.pending.has(id))
        this.child.stdin.write(JSON.stringify({ action: "cancel", id }) + "\n");
    }
  }
  async close() {
    this.cancel();
    this.fail();
    this.child.stdin.end();
    if (this.child.exitCode !== null || this.child.signalCode !== null) return;
    await new Promise<void>((resolve) =>
      this.child.once("exit", () => resolve()),
    );
  }
}

export class DemoBackend implements Backend {
  onProgress?: (phase: string, requestId?: string) => void;
  onCheckProgress?: (result: CheckProgress, requestId?: string) => void;
  private active = new Map<string, { cancelled: boolean }>();
  private sequence = 0;
  status: Status = {
    vpn_state: "inactive",
    gateway_state: "absent",
    subscription: "configured",
    routing_mode: "strict",
    networkmanager_configured: "yes",
    networkmanager_active: "no",
    diagnostics: "not-run",
  };
  private subscription = "https://example.invalid/subscription";
  servers: Server[] = [
    "Helsinki",
    "Amsterdam · Netherlands",
    "Frankfurt",
    "Tokyo",
    "New York",
  ].map((display_name, i) => ({
    server_id: `srv_${String(i).padStart(27, "0")}`,
    display_name,
    selected: false,
    status: "untested",
  }));
  async request(action: Action, value?: string, requestId?: string): Promise<Reply> {
    const id = requestId ?? `demo-${++this.sequence}`;
    if (this.active.has(id)) throw new Error("duplicate-request-id");
    const task = { cancelled: false };
    this.active.set(id, task);
    try {
      if (action !== "status" && action !== "subscription/read") {
        this.onProgress?.(
          action === "backend/start"
            ? "compose-up"
            : action === "start"
              ? "nm-work"
              : "runtime-wait",
          id,
        );
        await Bun.sleep(400);
      }
      if (task.cancelled)
        return {
          ok: false,
          reason: "cancelled",
          code: null,
          status: { ...this.status },
        };
      if (action === "backend/start") this.status.gateway_state = "healthy";
      if (action === "start") {
        this.status.vpn_state = "healthy";
        this.status.gateway_state = "healthy";
        this.status.networkmanager_active = "yes";
      }
      if (action === "disconnect" || action === "stop") {
        this.status.vpn_state = "inactive";
        this.status.networkmanager_active = "no";
      }
      if (action === "stop") this.status.gateway_state = "absent";
      if (action === "toggle-mode")
        this.status.routing_mode =
          this.status.routing_mode === "strict" ? "smart" : "strict";
      if (action === "subscription") {
        this.subscription = value ?? "";
        this.status.subscription = "configured";
      }
      if (action === "diagnostics") this.status.diagnostics = "available";
      const reply: Reply = {
        ok: true,
        reason: "ok",
        code: 0,
        status: { ...this.status },
      };
      if (action === "subscription/read") reply.value = this.subscription;
      if (action === "servers/list" || action === "servers/refresh")
        reply.catalog = { status: "ok", servers: structuredClone(this.servers) };
      if (action === "servers/check-batch") {
        const ids: string[] = JSON.parse(value ?? "{}").ids;
        const rows = this.servers.filter((s) => ids.includes(s.server_id));
        for (const row of rows) {
          row.ping_status = "ready";
          row.latency_ms = 20;
          row.availability = "ready";
        }
        reply.catalog = { status: "ok", servers: structuredClone(rows) };
      }
      const serverId =
        action === "servers/availability" ? JSON.parse(value ?? "{}").id : value;
      const server = this.servers.find((s) => s.server_id === serverId);
      if (server) {
        const i = this.servers.indexOf(server);
        if (action === "servers/ping") {
          server.ping_status = "ready";
          server.latency_ms = 15 + i * 11;
        }
        if (action === "servers/speed") {
          server.status = "ready";
          server.download_mbps = 90 - i * 12;
          server.download_seconds = 3;
        }
        if (action === "servers/availability")
          server.availability = i === 3 ? "failed" : "ready";
        if (action === "servers/select")
          this.servers.forEach((s) => (s.selected = s === server));
        reply.catalog = { status: "ok", server: { ...server } };
      }
      return reply;
    } finally {
      this.active.delete(id);
    }
  }
  cancel(requestId?: string) {
    for (const [id, task] of this.active)
      if (requestId === undefined || id === requestId) task.cancelled = true;
  }
  async close() { this.cancel(); }
}
