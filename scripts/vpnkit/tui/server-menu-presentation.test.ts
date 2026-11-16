import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend, rows } from "./app-test-fixtures";

for (const [width, height] of [[100, 35], [60, 16]] as const) test(`row menu fits ${width}x${height}`, async () => {
  const terminal = await createTestRenderer({ width, height }); const backend = new FakeBackend(); const app = new App(terminal.renderer, backend);
  try {
    backend.rows = [{ ...rows[0]!, display_name: "Tokyo-03", latency_ms: 24, download_mbps: 112, ping_status: "ready" }, rows[1]!];
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); app.serverID = rows[0]!.server_id; app.notice = ""; app.paint();
    Object.assign(app.servers.find(server => server.server_id === app.serverID)!, { latency_ms: 24, download_mbps: 112, ping_status: "ready" });
    terminal.mockInput.pressEnter(); await terminal.renderOnce();
    const frame = terminal.captureCharFrame();
    for (const text of ["Tokyo-03 · 24 мс · 112.0 Мбит/с", "Выбрать сервер", "Ping и сайт", "Скорость сервера", "Подробности сервера", "Переименовать", "↑↓ действие · Enter выполнить · Esc закрыть"]) expect(frame).toContain(text);
    if (process.env.TILE_HOME_CAPTURE === "1") await Bun.write(`../../../.build/tile-home-frames/${width}x${height}-row-menu.txt`, frame + "\n");
    terminal.mockInput.pressEnter(); await terminal.renderOnce();
    const confirm = terminal.captureCharFrame(); expect(confirm).toContain("Выбрать сервер Tokyo-03?"); expect(app.selected).toBe(0);
    if (width === 100 && process.env.TILE_HOME_CAPTURE === "1") await Bun.write("../../../.build/tile-home-frames/100x35-select-confirm.txt", confirm + "\n");
  } finally { await app.close(); }
});

test("mouse activates menu entries and outside click dismisses without touching the table", async () => {
  const terminal = await createTestRenderer({ width: 100, height: 35 }); const backend = new FakeBackend(); const app = new App(terminal.renderer, backend);
  try {
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); app.notice = ""; app.paint();
    terminal.mockInput.pressEnter(); await terminal.renderOnce();
    const frame = terminal.captureCharFrame().split("\n"); const y = frame.findIndex(line => line.includes("Подробности сервера")); const x = frame[y]!.indexOf("Подробности сервера");
    await terminal.mockMouse.click(x, y); await terminal.renderOnce(); expect(app.screen).toBe("details");
    app.open("servers", false, false); terminal.mockInput.pressEnter(); await terminal.renderOnce(); const id = app.serverID;
    await terminal.mockMouse.click(4, 4); await terminal.renderOnce(); expect(app.serverMenu).toBe(false); expect(app.serverID).toBe(id); expect(app.screen).toBe("servers");
  } finally { await app.close(); }
});

test("unavailable row entries and reasons remain visible at the minimum size", async () => {
  const terminal = await createTestRenderer({ width: 60, height: 16 }); const backend = new FakeBackend(); const app = new App(terminal.renderer, backend);
  try {
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); app.notice = ""; app.paint();
    terminal.mockInput.pressEnter(); app.catalogStale = true; app.paint(); await terminal.renderOnce();
    let frame = terminal.captureCharFrame();
    for (const text of ["Выбрать сервер", "Ping и сайт", "Скорость сервера", "Подробности сервера", "Подписка изменена.", "Обновите список серверов."]) expect(frame).toContain(text);
    for (let i = 0; i < 4; i++) terminal.mockInput.pressArrow("down"); await terminal.renderOnce(); expect(terminal.captureCharFrame()).toContain("Переименовать");
    app.servers = []; app.paint(); await terminal.renderOnce(); frame = terminal.captureCharFrame();
    for (const text of ["Выбрать сервер", "Ping и сайт", "Скорость сервера", "Подробности сервера", "Сервер изменился."]) expect(frame).toContain(text);
  } finally { await app.close(); }
});
