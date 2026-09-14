import { createCliRenderer } from "@opentui/core";
import { App } from "./app";
import { Bridge, DemoBackend } from "./bridge";

const args = process.argv.slice(2);
const demo = args.includes("--demo");
if (!process.stdin.isTTY || !process.stdout.isTTY) {
  console.error("Откройте VPNKIT в терминале. Для JSON используйте --status-json.");
  process.exit(2);
}
// App owns shutdown: OpenTUI must not destroy native buffers before an
// in-flight operation has finished and App has stopped its render timer.
const renderer = await createCliRenderer({ exitOnCtrlC: false, exitSignals: [], useMouse: true, targetFps: 20, backgroundColor: "#0b1120" });
const app = new App(renderer, demo ? new DemoBackend() : new Bridge(args), demo);
process.on("SIGTERM", () => void app.close());
process.on("SIGINT", () => void app.close());
process.on("SIGHUP", () => void app.close());
app.start();
