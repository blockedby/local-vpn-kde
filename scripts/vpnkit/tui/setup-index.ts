import { createCliRenderer } from "@opentui/core";
import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { SetupApp } from "./setup";
import { DemoSetupBackend, NativeSetupBackend } from "./setup-backend";

if (!process.stdin.isTTY || !process.stdout.isTTY) {
  console.error("Запусти ./install.sh в обычном терминале.");
  process.exit(2);
}
const demo = process.argv.includes("--demo");
const root = fileURLToPath(new URL("../../../", import.meta.url));
const renderer = await createCliRenderer({ exitOnCtrlC:false, exitSignals:[], useMouse:true, targetFps:20, backgroundColor:"#0b1120" });
let authChild: ChildProcess | undefined;
let closing = false;
const authorize = async () => {
  if (demo) return true;
  app.setSuspended(true);
  renderer.suspend();
  try {
    process.stdout.write("\nВведите пароль администратора для настройки маршрутов.\n");
    return await new Promise<boolean>(resolve => {
      authChild = spawn("sudo", ["-v"], { stdio:"inherit" });
      authChild.on("error", () => resolve(false));
      authChild.on("close", code => { authChild = undefined; resolve(code === 0); });
    });
  } finally {
    renderer.resume();
    app.setSuspended(false);
  }
};
const finish = async (open: boolean) => {
  if (closing) return;
  closing = true;
  authChild?.kill("SIGTERM");
  await app.close();
  renderer.destroy();
  process.exitCode = app.exitCode;
  if (open) {
    const args = ["ui", ...(demo ? ["--demo"] : [])];
    const child = spawn(`${root}/.build/local-vpn-kde.bin`, args, { stdio:"inherit" });
    child.on("error", () => { process.exitCode = 1; });
    child.on("close", code => { process.exitCode = code ?? 1; });
  }
};
const app = new SetupApp(renderer, demo ? new DemoSetupBackend() : new NativeSetupBackend(), authorize, open => { void finish(open); }, demo);
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"] as const) process.on(signal, () => { void finish(false); });
app.start();
