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
  stage: "ping" | "complete";
  ping_status: "ready" | "failed";
  latency_ms: number;
  availability: "ready" | "failed" | "untested";
}
export interface Backend {
  request(action: Action, value?: string): Promise<Reply>;
  cancel?(): void;
  onProgress?: (phase: string) => void;
  onCheckProgress?: (result: CheckProgress) => void;
  close(): Promise<void>;
}

// Serialize mutations, not screen navigation. All child I/O is asynchronous.
export class Bridge implements Backend {
  private child: ChildProcessWithoutNullStreams;
  private pending?: {
    resolve: (reply: Reply) => void;
    reject: (error: Error) => void;
  };
  private buffer = "";
  private closed = false;
  private cancelTimer?: ReturnType<typeof setInterval>;
  onProgress?: (phase: string) => void;
  onCheckProgress?: (result: CheckProgress) => void;
  constructor(args: string[] = []) {
    const root = fileURLToPath(new URL("../../../", import.meta.url));
    this.child = spawn(
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
          if (reply.event === "server-check") {
            if (!/^srv_[A-Za-z0-9_-]{27}$/.test(reply.server_id) ||
                !["ping", "complete"].includes(reply.stage) ||
                !["ready", "failed"].includes(reply.ping_status) ||
                !["ready", "failed", "untested"].includes(reply.availability) ||
                !Number.isInteger(reply.latency_ms) || reply.latency_ms < 0 || reply.latency_ms > 3600000) throw new Error();
            if (this.pending) this.onCheckProgress?.(reply);
            continue;
          }
          if (reply.event === "progress" && typeof reply.phase === "string") {
            this.onProgress?.(reply.phase);
            continue;
          }
          if (
            typeof reply.ok !== "boolean" ||
            typeof reply.status?.vpn_state !== "string"
          )
            throw new Error();
          const pending = this.pending;
          this.pending = undefined;
          clearInterval(this.cancelTimer);
          this.cancelTimer = undefined;
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
    clearInterval(this.cancelTimer);
    this.pending?.reject(new Error("backend-unavailable"));
    this.pending = undefined;
  }
  request(action: Action, value?: string): Promise<Reply> {
    if (this.closed || this.pending)
      return Promise.reject(new Error("backend-unavailable"));
    return new Promise((resolve, reject) => {
      this.pending = { resolve, reject };
      this.child.stdin.write(
        JSON.stringify({ action, value, progress: true }) + "\n",
      );
    });
  }
  cancel() {
    if (!this.pending || this.cancelTimer) return;
    // SIGUSR1 is ignored while idle and handled by supervised operations.
    // Retry closes the small race before a child installs its cancellation handler.
    this.child.kill("SIGUSR1");
    this.cancelTimer = setInterval(() => {
      if (this.pending) this.child.kill("SIGUSR1");
    }, 100);
  }
  async close() {
    this.closed = true;
    clearInterval(this.cancelTimer);
    this.child.stdin.end();
    if (this.child.exitCode !== null || this.child.signalCode !== null) return;
    await new Promise<void>((resolve) =>
      this.child.once("exit", () => resolve()),
    );
  }
}

export class DemoBackend implements Backend {
  onProgress?: (phase: string) => void;
  onCheckProgress?: (result: CheckProgress) => void;
  private cancelled = false;
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
  async request(action: Action, value?: string): Promise<Reply> {
    this.cancelled = false;
    if (action !== "status" && action !== "subscription/read") {
      this.onProgress?.(
        action === "backend/start"
          ? "compose-up"
          : action === "start"
            ? "nm-work"
            : "runtime-wait",
      );
      await Bun.sleep(400);
    }
    if (this.cancelled)
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
    const id =
      action === "servers/availability" ? JSON.parse(value ?? "{}").id : value;
    const server = this.servers.find((s) => s.server_id === id);
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
  }
  cancel() {
    this.cancelled = true;
  }
  async close() {}
}
