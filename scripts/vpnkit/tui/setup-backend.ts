import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";

export type SetupResult = { ok: boolean; reason: string; attempt?: string };
export interface SetupBackend {
  run(action: "image" | "install", progress: (phase: string) => void): Promise<SetupResult>;
  cancel(): void;
}
const root = fileURLToPath(new URL("../../../", import.meta.url));

export class NativeSetupBackend implements SetupBackend {
  private child?: ChildProcess;
  async run(action: "image" | "install", progress: (phase: string) => void): Promise<SetupResult> {
    if (this.child) return { ok: false, reason: "busy" };
    return new Promise((resolve) => {
      // Match the installer's fixed local configuration boundary; shell
      // overrides from another checkout must not select its state or image.
      const env = Object.fromEntries(Object.entries(process.env).filter(([name]) =>
        !/^(VPNKIT_LOCAL_|VPNKIT_OPENVPN_|VPNKIT_ROUTING_MODE$|VPNKIT_RULESET_SOURCE_MODE$|VPNKIT_SELECTED_OUTBOUND_MODE$)/.test(name)));
      const child = spawn(`${root}/.build/local-vpn-kde.bin`, ["setup", "--repo", root, "--action", action], { stdio: ["ignore", "pipe", "pipe"], env });
      this.child = child;
      let buffer = "", result: SetupResult | undefined;
      child.stderr?.resume();
      child.stdout?.setEncoding("utf8");
      child.stdout?.on("data", (chunk: string) => {
        buffer += chunk;
        if (buffer.length > 65536) { this.cancel(); buffer = ""; return; }
        while (buffer.includes("\n")) {
          const i = buffer.indexOf("\n"), line = buffer.slice(0, i);
          buffer = buffer.slice(i + 1);
          try {
            const event = JSON.parse(line);
            if (event.event === "progress" && typeof event.phase === "string") progress(event.phase);
            if (event.event === "result" && typeof event.ok === "boolean" && typeof event.reason === "string") {
              result = { ok: event.ok, reason: event.reason,
                attempt: typeof event.attempt === "string" && /^[0-9]{8}T[0-9]{6}Z-[a-f0-9]{12}$/.test(event.attempt) ? event.attempt : undefined };
            }
          } catch { this.cancel(); }
        }
      });
      child.on("error", () => { result = { ok: false, reason: "backend-unavailable" }; });
      // Resolve only after the Go supervisor has drained its operation group.
      child.on("close", () => {
        this.child = undefined;
        resolve(result ?? { ok: false, reason: "backend-unavailable" });
      });
    });
  }
  cancel() { this.child?.kill("SIGTERM"); }
}

export class DemoSetupBackend implements SetupBackend {
  private cancelled = false;
  async run(action: "image" | "install", progress: (phase: string) => void): Promise<SetupResult> {
    this.cancelled = false;
    const stages = action === "image" ? ["image"] : ["setup-assets", "setup-underlay", "setup-gateway", "setup-profile", "setup-verify"];
    for (const phase of stages) {
      await Bun.sleep(350);
      if (this.cancelled) return { ok: false, reason: "cancelled" };
      progress(phase);
    }
    return { ok: true, reason: "ok" };
  }
  cancel() { this.cancelled = true; }
}
