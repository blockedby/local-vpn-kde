import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend } from "./app-test-fixtures";

test("filter and palette accept text without dispatching shortcuts", async () => {
  const terminal = await createTestRenderer({ width: 80, height: 24 });
  const backend = new FakeBackend(); const app = new App(terminal.renderer, backend);
  try {
    await app.perform("status"); await app.perform("servers/list");
    terminal.mockInput.pressKey("/"); terminal.mockInput.pressKey("t"); terminal.mockInput.pressKey("o"); await terminal.renderOnce();
    expect(app.filter).toBe("to"); expect(app.filterEditing).toBe(true); expect(backend.calls.some(call => call.action === "servers/speed-batch")).toBe(false);
    expect(terminal.captureCharFrame()).toContain("1/2");
    terminal.mockInput.pressEnter(); terminal.mockInput.pressEscape(); await Bun.sleep(50);
    terminal.mockInput.pressKey(":"); terminal.mockInput.pressKey("q"); await terminal.renderOnce();
    expect(app.paletteOpen).toBe(true); expect(app.paletteQuery).toBe("q"); expect(app.closing).toBe(false);
    expect(terminal.captureCharFrame()).toContain("Команды");
    terminal.mockInput.pressEscape(); await Bun.sleep(50); terminal.mockInput.pressKey("p", { ctrl: true }); await terminal.renderOnce();
    expect(app.paletteOpen).toBe(true); expect(backend.calls.some(call => call.action === "servers/check-batch")).toBe(false);
  } finally { await app.close(); }
});

test("clicking a table column selects and then reverses its sort", async () => {
  const terminal = await createTestRenderer({ width: 80, height: 24 });
  const app = new App(terminal.renderer, new FakeBackend());
  try {
    await app.perform("status"); await app.perform("servers/list"); await terminal.renderOnce();
    await terminal.mockMouse.click(42, 2); await terminal.renderOnce();
    expect(app.sort).toBe("ping"); expect(app.sortDescending).toBe(false); expect(terminal.captureCharFrame()).toContain("Ping мс↑");
    await terminal.mockMouse.click(42, 2); await terminal.renderOnce();
    expect(app.sortDescending).toBe(true); expect(terminal.captureCharFrame()).toContain("Ping мс↓");
  } finally { await app.close(); }
});
