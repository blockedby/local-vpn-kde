import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { SetupApp } from "./setup";
import type { SetupBackend, SetupResult } from "./setup-backend";

class FakeSetup implements SetupBackend {
  calls: string[] = [];
  cancels = 0;
  result: SetupResult = { ok: true, reason:"ok" };
  hold?: Promise<SetupResult>;
  progress?: (phase: string) => void;
  async run(action: "image" | "install", progress: (phase:string) => void) {
    this.calls.push(action); this.progress = progress;
    if (action === "image") return { ok:true, reason:"ok" };
    progress("setup-profile");
    return this.hold ?? this.result;
  }
  cancel() { this.cancels++; }
}

test("setup stays navigable and cancellation defaults to No during a held operation", async () => {
  const t = await createTestRenderer({ width:80, height:24 });
  const backend = new FakeSetup();
  let release!: (r:SetupResult) => void;
  backend.hold = new Promise(r => release = r);
  const app = new SetupApp(t.renderer, backend, async () => true, () => {});
  try {
    const task = app.run();
    await Bun.sleep(10);
    t.mockInput.pressEscape(); await Bun.sleep(60); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Ход установки");
    expect(backend.cancels).toBe(0);
    t.mockInput.pressKey("k"); await Bun.sleep(10); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("› Нет, продолжить установку");
    t.mockInput.pressEnter(); await Bun.sleep(10); await t.renderOnce();
    expect(backend.cancels).toBe(0);
    t.mockInput.pressKey("k"); t.mockInput.pressArrow("down"); t.mockInput.pressEnter();
    await Bun.sleep(10); await t.renderOnce();
    expect(backend.cancels).toBe(1);
    expect(t.captureCharFrame()).toContain("Останавливаем операцию");
    release({ ok:false, reason:"cancelled" }); await task;
  } finally { release({ ok:false, reason:"cancelled" }); await app.close(); }
});

test("setup retries the failed installation without rebuilding and never skips authorization", async () => {
  const t = await createTestRenderer({ width:80, height:24 });
  const backend = new FakeSetup();
  backend.result = { ok:false, reason:"foreign-profile", attempt:"20260915T120000Z-123456abcdef" };
  let permissions = 0;
  const app = new SetupApp(t.renderer, backend, async () => { permissions++; return true; }, () => {});
  try {
    await app.run(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Профиль vpnkit-local уже существует");
    expect(t.captureCharFrame()).toContain("Повторить установку");
    expect(t.captureCharFrame()).toContain("123456abcdef.log");
    backend.result = { ok:true, reason:"ok" };
    await app.run(); await t.renderOnce();
    expect(backend.calls).toEqual(["image","install","install"]);
    expect(permissions).toBe(2);
    expect(t.captureCharFrame()).toContain("Открыть программу");
  } finally { await app.close(); }
});

test("a rejected sudo prompt does not start host setup", async () => {
  const t = await createTestRenderer({ width:80, height:24 });
  const backend = new FakeSetup();
  const app = new SetupApp(t.renderer, backend, async () => false, () => {});
  try {
    await app.run(); await t.renderOnce();
    expect(backend.calls).toEqual(["image"]);
    expect(t.captureCharFrame()).toContain("Права не получены");
  } finally { await app.close(); }
});

for (const [width,height] of [[100,28],[60,20],[48,18]]) {
  test(`setup keeps actions visible at ${width}x${height}`, async () => {
    const t = await createTestRenderer({ width,height });
    const backend = new FakeSetup();
    const app = new SetupApp(t.renderer, backend, async () => true, () => {});
    try {
      await t.renderOnce();
      expect(t.captureCharFrame()).toContain("Начать установку");
      await app.run(); await t.renderOnce();
      expect(t.captureCharFrame()).toContain("Открыть программу");
      expect(t.captureCharFrame()).toContain("Enter");
    } finally { await app.close(); }
  });
}
