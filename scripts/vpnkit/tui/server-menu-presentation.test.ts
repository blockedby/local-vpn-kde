import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend, rows, settle } from "./app-test-fixtures";

test("minimum-size server panel exposes the last action and the last details", async () => {
  const terminal = await createTestRenderer({ width: 40, height: 10 });
  const app = new App(terminal.renderer, new FakeBackend()), input = terminal.mockInput;
  try {
    await app.perform("status"); input.pressKey("v"); await settle(); input.pressEnter(); app.onKey({ name: "end", preventDefault() {} }); await terminal.renderOnce();
    expect(terminal.captureCharFrame()).toContain("› Переименовать"); input.pressEnter(); expect(app.screen).toBe("server-rename");
    input.pressEscape(); await Bun.sleep(50); input.pressEnter();
    for (let step = 0; step < 3; step++) input.pressArrow("down");
    input.pressEnter(); expect(app.screen).toBe("details"); await terminal.renderOnce();
    expect(terminal.captureCharFrame()).not.toContain("Состояние:");
    for (let step = 0; step < 5; step++) input.pressArrow("down");
    await terminal.renderOnce(); const frame = terminal.captureCharFrame();
    for (const field of ["Скорость:", "Сайт:", "Состояние:"]) expect(frame).toContain(field);
  } finally { await app.close(); }
});

for (const scenario of ["missing subscription", "egress unavailable", "empty catalog"]) test(`Servers fallback activates its displayed row: ${scenario}`, async () => {
  const terminal = await createTestRenderer({ width: 100, height: 24 });
  const backend = new FakeBackend(); backend.rows = [];
  backend.status = { ...backend.status, runtime: "native", installed: true,
    ...(scenario === "missing subscription" ? { subscription: "not configured" } : scenario === "egress unavailable" ? { gateway_state: "egress-unavailable" } : {}) };
  const app = new App(terminal.renderer, backend);
  try {
    await app.perform("status"); terminal.mockInput.pressKey("v"); await settle();
    if (scenario === "egress unavailable") terminal.mockInput.pressArrow("down");
    const label = scenario === "missing subscription" ? "Добавить подписку" : scenario === "egress unavailable" ? "Запустить сервис" : "Обновить подписку и список";
    await terminal.renderOnce(); const frame = terminal.captureCharFrame();
    expect(frame).toContain(`› ${label}`); expect(frame).toContain(`Enter ${label.toLowerCase()}`);
    const before = backend.calls.length; terminal.mockInput.pressEnter(); await settle();
    if (scenario === "missing subscription") expect(app.screen).toBe("subscription");
    else expect(backend.calls[before]?.action).toBe(scenario === "egress unavailable" ? "backend/start" : "servers/refresh");
  } finally { await app.close(); }
});

for (const [width, height] of [[100, 35], [60, 16]] as const) test(`row menu fits ${width}x${height}`, async () => {
  const terminal = await createTestRenderer({ width, height }); const backend = new FakeBackend(); const app = new App(terminal.renderer, backend);
  try {
    backend.rows = [{ ...rows[0]!, display_name: "Tokyo-03", latency_ms: 24, download_mbps: 112, ping_status: "ready" }, rows[1]!];
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); app.serverID = rows[0]!.server_id; app.notice = ""; app.paint();
    Object.assign(app.servers.find(server => server.server_id === app.serverID)!, { latency_ms: 24, download_mbps: 112, ping_status: "ready" });
    terminal.mockInput.pressEnter(); await terminal.renderOnce();
    const frame = terminal.captureCharFrame();
    for (const text of ["Tokyo-03 · 24 мс · 112.0 Мбит/с", "Выбрать сервер", "Ping и сайт", "Скорость сервера", "Подробности сервера", "Переименовать", "↑↓ действие · Enter выполнить"]) expect(frame).toContain(text);
    expect(frame.split("\n").at(-2)?.trimEnd()).toEndWith(width === 100 ? "Esc закрыть · ? все клавиши" : "Enter выполнить · ? все клавиши");
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
