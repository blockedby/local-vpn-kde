import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend, rows } from "./app-test-fixtures";

async function fixture(width: number, height: number) {
  const terminal = await createTestRenderer({ width, height });
  const backend = new FakeBackend();
  backend.rows = Array.from({ length: 25 }, (_, i) => ({ ...rows[0]!, server_id: `srv_${String(i).padStart(27, "0")}`, display_name: `Node ${String(i).padStart(2, "0")}`, selected: i === 0 }));
  const app = new App(terminal.renderer, backend);
  await app.perform("status"); await app.perform("servers/list");
  app.notice = ""; app.paint(); await terminal.renderOnce();
  return { terminal, backend, app };
}

test("shell: table has twenty rows at 80x24, footer uses available shortcuts", async () => {
  const { terminal: t, app } = await fixture(80, 24);
  try {
    const lines = t.captureCharFrame().split("\n");
    expect(lines[0]).toContain("отключён"); expect(lines[1]).toContain("Серверы");
    expect(lines[2]).toContain("Ping мс");
    expect(lines.slice(3, -1).filter(line => /Node \d\d/.test(line))).toHaveLength(20);
    expect(lines[23]).toContain("Enter выбрать"); expect(lines[23]).toContain("? помощь");
    expect(lines[23]).not.toContain("k отменить");
  } finally { await app.close(); }
});

test("shell: responsive table drops status then site and warns below minimum", async () => {
  const { terminal: t, app } = await fixture(100, 35);
  try {
    expect(t.captureCharFrame().split("\n")[2]).toContain("Статус");
    t.resize(80,24); await t.renderOnce(); expect(t.captureCharFrame().split("\n")[2]).toContain("Сайт");
    t.resize(60,16); await t.renderOnce(); const header = t.captureCharFrame().split("\n")[2]!;
    expect(header).toContain("Ping мс"); expect(header).toContain("Мбит/с"); expect(header).not.toContain("Сайт"); expect(header).not.toContain("Статус");
    t.resize(59,16); await t.renderOnce(); expect(t.captureCharFrame()).toContain("Увеличьте терминал");
  } finally { await app.close(); }
});

test("shell: help draws grouped registry actions and unavailable reason", async () => {
  const { terminal: t, app } = await fixture(100,35);
  try {
    app.onKey({ name: "?", preventDefault() {} }); await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Помощь"); expect(frame).toContain("Общие"); expect(frame).toContain("Нет операции для отмены");
    expect(frame).toContain("Ping всех + сайт");
    app.onKey({ name: "escape", preventDefault() {} }); await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("Нет операции для отмены");
  } finally { await app.close(); }
});

test("shell: status reports unknown and stale without a separate progress line", async () => {
  const { terminal: t, app } = await fixture(100,35);
  try {
    app.status!.routing_mode = "unknown"; app.checked = Date.now() - 16000; app.paint(); await t.renderOnce();
    const first = t.captureCharFrame().split("\n")[0]!;
    expect(first).toContain("устарел"); expect(first).toContain("неизвестно");
    expect(t.captureCharFrame().split("\n").filter(line => line.includes("устарел"))).toHaveLength(1);
  } finally { await app.close(); }
});

test("shell: tabs cycle and Home/End retain a visible cursor", async () => {
  const { terminal: t, app } = await fixture(80,24);
  try {
    app.onKey({ name: "end", preventDefault() {} }); await t.renderOnce();
    expect(app.serverID).toBe(app.sortedServers().at(-1)?.server_id);
    expect(t.captureCharFrame()).toContain("Node 24");
    app.onKey({ name: "home", preventDefault() {} }); await t.renderOnce();
    expect(app.serverID).toBe(app.sortedServers()[0]?.server_id);
    app.onKey({ name: "tab", preventDefault() {} });
    expect(app.screen).toBe("profiles");
    app.onKey({ name: "tab", shift: true, preventDefault() {} });
    expect(app.screen).toBe("servers");
  } finally { await app.close(); }
});

test("shell: T measures the focused server and t starts a batch", async () => {
  const { app, backend } = await fixture(80,24);
  try {
    app.serverID = app.sortedServers()[1]!.server_id;
    app.onKey({ name: "t", shift: true, preventDefault() {} });
    for (let i = 0; i < 30 && app.homeTesting; i++) await Bun.sleep(10);
    expect(backend.calls.find(call => call.action === "servers/speed")?.value).toBe(app.serverID);
    app.onKey({ name: "t", preventDefault() {} });
    for (let i = 0; i < 30 && app.batch; i++) await Bun.sleep(10);
    expect(backend.calls.some(call => call.action === "servers/speed-batch")).toBe(true);
  } finally { await app.close(); }
});

test("shell: an empty catalog explains how to refresh", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend(); b.rows = [];
  const app = new App(t.renderer, b);
  try {
    await app.perform("status"); await app.perform("servers/list"); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Список серверов пуст");
    expect(t.captureCharFrame()).toContain("[r]");
  } finally { await app.close(); }
});

test("shell: status uses available width and omits unknown measurements", async () => {
  const { terminal: t, app } = await fixture(100, 35);
  try {
    app.servers[0]!.display_name = "Amsterdam · Netherlands";
    app.status!.networkmanager_active = "yes";
    app.notice = ""; app.paint(); await t.renderOnce();
    const line = t.captureCharFrame().split("\n")[0]!;
    expect(line).toContain("Amsterdam · Netherlands");
    expect(line).not.toContain("ping ?"); expect(line).not.toContain("скорость ?");
    t.resize(60, 16); await t.renderOnce();
    expect(t.captureCharFrame().split("\n")[0]).toContain("подключён");
  } finally { await app.close(); }
});

test("shell: confirmation keeps its originating tab and footer follows context", async () => {
  const { terminal: t, app } = await fixture(80, 24);
  try {
    app.status!.networkmanager_active = "yes";
    app.open("settings"); await t.renderOnce();
    expect(t.captureCharFrame().split("\n").at(-2)).toContain("x отключить");
    app.onKey({ name: "x", preventDefault() {} }); await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame.split("\n")[1]).toContain("[Настройки]");
    expect(frame).toContain("Подтверждение");
    app.onKey({ name: "n", preventDefault() {} }); await t.renderOnce();
    expect(app.screen).toBe("settings");
    app.open("target"); await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame.split("\n")[1]).toContain("[Настройки]");
    expect(frame.split("\n").at(-2)).not.toContain("? помощь");
    const before = app.editorValue;
    t.mockInput.pressKey("?"); await t.renderOnce();
    expect(app.helpOpen).toBe(false);
    expect(app.editorValue).toContain("?");
    expect(app.editorValue.length).toBe(before.length + 1);
  } finally { await app.close(); }
});

test("shell: help combines aliases and long notices wrap without hiding recovery", async () => {
  const { terminal: t, app } = await fixture(100, 35);
  try {
    app.onKey({ name: "?", preventDefault() {} }); await t.renderOnce();
    const help = t.captureCharFrame();
    expect(help).toContain("v/h Серверы");
    expect(help).toContain("Tab/Shift+Tab Сменить вкладку");
    app.onKey({ name: "escape", preventDefault() {} });
    app.notify("Сетевые настройки VPN поменялись, нажмите [f] чтобы применить их ещё раз.", true);
    t.resize(60, 16); await t.renderOnce();
    const frame = t.captureCharFrame().replace(/\s+/g, " ");
    expect(frame).toContain("нажмите [f] чтобы применить их ещё раз.");
  } finally { await app.close(); }
});

test("shell: live speed stays attributed to its measured row and appears in status", async () => {
  const { terminal: t, app } = await fixture(100, 35);
  try {
    app.homeTesting = true;
    app.homeSpeedServer = { id: app.servers[1]!.server_id, name: app.servers[1]!.display_name };
    app.liveSpeeds.set(app.servers[1]!.server_id, 16);
    app.paint(); await t.renderOnce();
    const lines = t.captureCharFrame().split("\n");
    expect(lines[0]).toContain("тест Node 01: ~16.0");
    expect(lines.find(line => line.includes("Node 01") && line.includes("~16.0"))).toBeDefined();
  } finally { app.homeTesting = false; await app.close(); }
});
