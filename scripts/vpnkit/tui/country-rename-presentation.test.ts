import { expect, test } from "bun:test";
import { mkdir } from "node:fs/promises";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import { FakeBackend, rows } from "./app-test-fixtures";

async function capture(name: string, frame: string) {
  if (process.env.TILE_HOME_CAPTURE !== "1") return;
  await mkdir("../../../.build/tile-home-frames", { recursive: true });
  await Bun.write(`../../../.build/tile-home-frames/${name}.txt`, frame + "\n");
}
for (const [width, height] of [[100, 35], [60, 16]] as const) test(`country table fits ${width}x${height}`, async () => {
  const t = await createTestRenderer({ width, height }), b = new FakeBackend(), app = new App(t.renderer, b);
  try {
    b.rows.push({ ...rows[0]!, server_id: "unknown", display_name: "Unknown", country: undefined });
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); app.notice = ""; app.paint(); await t.renderOnce();
    const frame = t.captureCharFrame(), lines = frame.split("\n"), header = lines.find(line => line.includes("Ping мс"))!;
    expect(lines.every(line => Bun.stringWidth(line) <= width)).toBe(true);
    expect(header).toContain("Ping мс"); expect(header).toContain("Мбит/с");
    if (width === 100) { expect(header).toContain("Страна"); expect(lines.find(line => line.includes("Amsterdam"))).toMatch(/Amsterdam\s+NL\s+—\s+—/); expect(lines.find(line => line.includes("Unknown"))).toMatch(/Unknown\s+—\s+—\s+—/); }
    else expect(header).not.toContain("Страна");
    await capture(`${width}x${height}-servers-country`, frame);
    if (width === 100) {
      const x = header.indexOf("Страна"); const y = lines.indexOf(header); await t.mockMouse.click(x + 1, y); await t.renderOnce(); expect(app.sort).toBe("country");
      t.resize(66, 20); await t.renderOnce(); expect(t.captureCharFrame().split("\n")[2]).toContain("Страна"); expect(t.captureCharFrame().split("\n")[2]).not.toContain("Сайт");
      t.resize(61, 20); await t.renderOnce(); expect(t.captureCharFrame().split("\n")[2]).not.toContain("Страна");
    }
  } finally { await app.close(); }
});

test("rename editor shows original title, reset hint and footer", async () => {
  const t = await createTestRenderer({ width: 100, height: 35 }), b = new FakeBackend(), app = new App(t.renderer, b);
  try {
    b.rows[0] = { ...b.rows[0]!, display_name: "Custom Tokyo", original_name: "Tokyo original" };
    await app.perform("status"); app.open("servers", false); await app.perform("servers/list"); app.serverID = b.rows[0]!.server_id; app.notice = ""; app.paint();
    t.mockInput.pressEnter(); t.mockInput.pressKey("n"); await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(app.editorValue).toBe("Custom Tokyo"); expect(frame).not.toContain("Custom Tokyon");
    for (const text of ["Tokyo original", "Custom Tokyo", "Пустая строка вернёт имя из подписки.", "Enter сохранить · Esc отмена"]) expect(frame).toContain(text);
    await capture("100x35-rename-editor", frame);
  } finally { await app.close(); }
});
