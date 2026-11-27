import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend } from "./app-test-fixtures";

test("shortcut tabs, search focus and selection share one keyboard window", async () => {
  const terminal = await createTestRenderer({ width: 80, height: 24 });
  const app = new App(terminal.renderer, new FakeBackend());
  const input = terminal.mockInput;
  try {
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list");
    for (const screen of ["home", "settings", "servers"] as const) {
      app.open(screen, false, false); await terminal.renderOnce();
      expect(terminal.captureCharFrame().split("\n").at(-2)?.trimEnd()).toEndWith("? все клавиши");
    }
    input.pressKey("?"); input.pressKey("q"); await terminal.renderOnce();
    expect(app.helpOpen).toBe(true); expect(app.searchFocused).toBe(false); expect(app.paletteQuery).toBe("");
    expect(terminal.captureCharFrame()).toContain("Фильтр серверов");
    input.pressTab(); expect(app.keysTab).toBe(1);
    expect(app.filteredPaletteActions().every(row => row.action.scope === "global")).toBe(true);
    input.pressTab(); expect(app.keysTab).toBe(2);
    input.pressKey("\x1b[Z"); expect(app.keysTab).toBe(1);
    input.pressKey("/"); await input.typeText("cTrL+p"); await terminal.renderOnce();
    expect(app.filteredPaletteActions().map(row => row.action.id)).toEqual(["global:palette"]);
    input.pressEscape(); await Bun.sleep(50); input.pressKey(":");
    expect(app.searchFocused).toBe(true); expect(app.paletteQuery).toBe(""); expect(app.keysTab).toBe(0);
    input.pressKey("?"); await terminal.renderOnce();
    expect(app.paletteQuery).toBe("?"); expect(terminal.captureCharFrame()).toContain("Ничего не найдено");
    input.pressEscape(); await Bun.sleep(50); expect(app.helpOpen).toBe(false);
  } finally { await app.close(); }
});

test("selected shortcut stays visible at minimum size and after paging", async () => {
  const terminal = await createTestRenderer({ width: 40, height: 10 });
  const app = new App(terminal.renderer, new FakeBackend());
  const key = (name: string) => app.onKey({ name, preventDefault() {} });
  try {
    key("?"); key("tab"); key("tab"); key("pagedown");
    expect(app.paletteIndex).toBe(app.keysPageSize());
    key("end"); await terminal.renderOnce();
    expect(app.paletteIndex).toBe(app.filteredPaletteActions().length - 1);
    expect(terminal.captureCharFrame()).toContain("›");
    key("pageup"); expect(app.paletteIndex).toBe(app.filteredPaletteActions().length - 1 - app.keysPageSize());
    key("home"); key("down"); key("up"); expect(app.paletteIndex).toBe(0);
    terminal.resize(39, 10); await terminal.renderOnce();
    expect(terminal.captureCharFrame()).toContain("Увеличьте окно"); expect(terminal.captureCharFrame()).not.toContain("Этот экран");
    key("return"); expect(app.helpOpen).toBe(true); key("escape"); expect(app.helpOpen).toBe(false);
  } finally { await app.close(); }
});

test("filter and shortcut search accept text without dispatching shortcuts", async () => {
  const terminal = await createTestRenderer({ width: 80, height: 24 });
  const backend = new FakeBackend(); const app = new App(terminal.renderer, backend);
  try {
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list");
    terminal.mockInput.pressKey("/"); terminal.mockInput.pressKey("t"); terminal.mockInput.pressKey("o"); await terminal.renderOnce();
    expect(app.filter).toBe("to"); expect(app.filterEditing).toBe(true); expect(backend.calls.some(call => call.action === "servers/speed-batch")).toBe(false);
    expect(terminal.captureCharFrame()).toContain("1/2");
    terminal.mockInput.pressEnter(); terminal.mockInput.pressEscape(); await Bun.sleep(50);
    terminal.mockInput.pressKey(":"); terminal.mockInput.pressKey("q"); await terminal.renderOnce();
    expect(app.helpOpen).toBe(true); expect(app.paletteQuery).toBe("q"); expect(app.closing).toBe(false);
    expect(terminal.captureCharFrame()).toContain("Клавиши");
    terminal.mockInput.pressEscape(); await Bun.sleep(50); terminal.mockInput.pressKey("p", { ctrl: true }); await terminal.renderOnce();
    expect(app.helpOpen).toBe(true); expect(backend.calls.some(call => call.action === "servers/check-batch")).toBe(false);
  } finally { await app.close(); }
});

test("clicking a table column selects and then reverses its sort", async () => {
  const terminal = await createTestRenderer({ width: 80, height: 24 });
  const app = new App(terminal.renderer, new FakeBackend());
  try {
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); await terminal.renderOnce();
    await terminal.mockMouse.click(42, 2); await terminal.renderOnce();
    expect(app.sort).toBe("ping"); expect(app.sortDescending).toBe(false); expect(terminal.captureCharFrame()).toContain("Ping мс↑");
    await terminal.mockMouse.click(42, 2); await terminal.renderOnce();
    expect(app.sortDescending).toBe(true); expect(terminal.captureCharFrame()).toContain("Ping мс↓");
  } finally { await app.close(); }
});
