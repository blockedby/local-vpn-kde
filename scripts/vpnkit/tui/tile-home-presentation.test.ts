import { expect, test } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend, rows } from "./app-test-fixtures";

for (const [width, height] of [[100,35],[60,16]] as const) test(`home tiles fit ${width}x${height} and redact the check address`, async () => {
  const t = await createTestRenderer({ width, height }); const b = new FakeBackend(); const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    app.target = "https://example.invalid/private-token?secret=hidden#fragment";
    app.servers = [{ ...rows[0]!, selected: true, ping_status: "ready", latency_ms: 24 }, rows[1]!]; app.catalogLoaded = true;
    app.profiles = { active: "a", items: [{ id: "a", name: "Дом", kind: "subscription", count: 2, stale: false }] };
    app.autostart = { gateway: true, connect: true }; app.paint(); await t.renderOnce();
    const frame = t.captureCharFrame();
    for (const title of ["Подключение", "Серверы", "Профили", "Диагностика", "Настройки"]) expect(frame.split(title)).toHaveLength(2);
    expect(frame).toContain("▶ Подключение"); expect(frame).not.toContain("Замеры"); expect(frame).not.toContain("› ▶");
    for (const glyph of ["▶", "≡", "•", "◇", "+", "*"]) expect(Bun.stringWidth(glyph)).toBe(1);
    // Tiles are chosen with arrows; no letter shortcut is drawn on them.
    for (const title of ["Подключение", "Серверы", "Профили", "Диагностика", "Настройки"]) {
      const line = frame.split("\n").find(line => line.includes(title))!;
      expect(line).not.toMatch(new RegExp(`${title} +[a-zA-Z]( |$)`));
    }
    expect(frame).toContain("←↑↓→ выбор"); expect(frame).toContain("Enter открыть"); expect(frame).toContain("? все клавиши"); expect(frame).toContain("q выход");
    expect(frame).not.toContain("private-token"); expect(frame).not.toContain("secret=hidden"); expect(frame).not.toContain("fragment");
    if (height >= 20) { expect(frame).toContain("Режим: строгий · автозапуск:"); expect(frame).toContain("VPN"); expect(frame).toContain("Сайт:"); expect(frame).toContain("https://example.invalid/…"); expect(frame).toContain("Лучший ping:"); }
    else { expect(frame).not.toContain("Лучший ping:"); expect(frame).not.toContain("Сайт:"); }
    if (process.env.TILE_HOME_CAPTURE === "1") await Bun.write(`../../../.build/tile-home-frames/${width}x${height}-home.txt`, frame + "\n");
  } finally { await app.close(); }
});

test("clicking a tile activates the section and escape returns its highlight", async () => {
  const t = await createTestRenderer({ width: 100, height: 35 }); const app = new App(t.renderer, new FakeBackend());
  try { await app.perform("status"); await t.renderOnce(); await t.mockMouse.click(38, 2); await t.renderOnce(); expect(app.screen).toBe("servers"); expect(t.captureCharFrame()).toContain("‹ Главная › Серверы"); t.mockInput.pressEscape(); await Bun.sleep(60); await t.renderOnce(); expect(app.screen).toBe("home"); expect(app.homeIndex).toBe(1); } finally { await app.close(); }
});


test("connected tile: its letter shortcut still does what Enter does", async () => {
  const t = await createTestRenderer({ width: 100, height: 35 }); const b = new FakeBackend();
  b.status.networkmanager_active = "yes"; const app = new App(t.renderer, b);
  try {
    await app.perform("status"); await t.renderOnce();
    const line = t.captureCharFrame().split("\n").find(line => line.includes("Подключение"))!;
    expect(line).not.toMatch(/Подключение +[a-zA-Z]( |$)/);
    t.mockInput.pressKey("x"); await Bun.sleep(40);
    const byKey = [app.screen, app.confirmation];
    t.mockInput.pressEscape(); await Bun.sleep(40); expect(app.screen).toBe("home");
    t.mockInput.pressEnter(); await Bun.sleep(40);
    expect([app.screen, app.confirmation]).toEqual(byKey);
    expect(byKey).toEqual(["confirm", "disconnect"]);
    expect(b.calls.map(call => call.action)).not.toContain("disconnect");
  } finally { await app.close(); }
});

test("profiles tile shows no letter", async () => {
  const t = await createTestRenderer({ width: 100, height: 35 }); const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, subscription: "configured" }; const app = new App(t.renderer, b);
  try {
    await app.perform("status"); await t.renderOnce();
    const line = t.captureCharFrame().split("\n").find(line => line.includes("Профили"))!;
    expect(line).not.toMatch(/Профили +[a-zA-Z]( |$)/);
  } finally { await app.close(); }
});

test("tiles stay in navigation order after a resize between layouts", async () => {
  const t = await createTestRenderer({ width: 100, height: 35 }); const app = new App(t.renderer, new FakeBackend());
  const order = (frame: string) => ["Подключение", "Серверы", "Профили", "Диагностика", "Настройки"]
    .map(title => { const lines = frame.split("\n"); const row = lines.findIndex(line => line.includes(title)); return [row, lines[row]!.indexOf(title)] as const; });
  try {
    await app.perform("status"); await t.renderOnce();
    for (const width of [80, 100, 60]) {
      t.resize(width, 35); await Bun.sleep(40); app.paint(); await t.renderOnce();
      const columns = width >= 90 ? 3 : 2, cells = order(t.captureCharFrame());
      cells.forEach(([row, column], i) => {
        expect(row).toBe(cells[i - i % columns]![0]);
        if (i % columns) expect(column).toBeGreaterThan(cells[i - 1]![1]);
        if (i >= columns) expect(row).toBeGreaterThan(cells[i - columns]![0]);
      });
    }
    t.resize(80, 35); await Bun.sleep(40); app.paint(); await t.renderOnce();
    app.homeIndex = 0; t.mockInput.pressArrow("down"); await Bun.sleep(40); await t.renderOnce();
    expect(app.homeIndex).toBe(2); expect(t.captureCharFrame()).toContain("◇ Профили");
  } finally { await app.close(); }
});
