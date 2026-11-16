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
    for (const title of ["Подключение", "Серверы", "Замеры", "Профили", "Диагностика", "Настройки"]) expect(frame.split(title)).toHaveLength(2);
    expect(frame).toContain("←↑↓→ выбор"); expect(frame).toContain("Enter открыть"); expect(frame).toContain("? все клавиши"); expect(frame).toContain("q выход");
    expect(frame).not.toContain("private-token"); expect(frame).not.toContain("secret=hidden"); expect(frame).not.toContain("fragment");
    if (height >= 20) { expect(frame).toContain("Сайт: https://example.invalid"); expect(frame).toContain("Лучший ping:"); }
    else { expect(frame).not.toContain("Лучший ping:"); expect(frame).not.toContain("Сайт:"); }
    if (process.env.TILE_HOME_CAPTURE === "1") await Bun.write(`../../../.build/tile-home-frames/${width}x${height}-home.txt`, frame + "\n");
  } finally { await app.close(); }
});

test("clicking a tile activates the section and escape returns its highlight", async () => {
  const t = await createTestRenderer({ width: 100, height: 35 }); const app = new App(t.renderer, new FakeBackend());
  try { await app.perform("status"); await t.renderOnce(); await t.mockMouse.click(38, 2); await t.renderOnce(); expect(app.screen).toBe("servers"); expect(t.captureCharFrame()).toContain("‹ Главная › Серверы"); t.mockInput.pressEscape(); await Bun.sleep(60); await t.renderOnce(); expect(app.screen).toBe("home"); expect(app.homeIndex).toBe(1); } finally { await app.close(); }
});
