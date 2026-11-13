import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App } from "./app";
import type { Action, CheckProgress, Reply } from "./bridge";
import { FakeBackend, settle, reply, rows, initial } from "./app-test-fixtures";

test("legacy status without runtime keeps Docker lifecycle wording", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запустить Docker");
    expect(t.captureCharFrame()).not.toContain("NATIVE PREVIEW");
    b.status.gateway_state = "healthy";
    await app.perform("status");
    t.mockInput.pressKey("z");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("остановить Docker и отключить VPN?");
    expect(t.captureCharFrame()).not.toContain("native-службу");
  } finally {
    await app.close();
  }
});

test("compact home has state-specific actions, no stale generic hints or refresh timestamp", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame).toContain("Запустить Docker");
    expect(frame).not.toContain("[s] Подключить");
    expect(frame).not.toContain("Обновлено:");
    expect(frame).not.toContain("Нажмите");
    b.status.gateway_state = "healthy";
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Подключить VPN");
    b.status.networkmanager_active = "yes";
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Отключить VPN");
    const lines = t.captureCharFrame().split("\n");
    expect(lines.findIndex(line => line.includes("[x] Отключить VPN"))).toBe(3);
    t.resize(60, 20);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("q выход");
  } finally {
    await app.close();
  }
});

test("home has one list of navigation entries and no top tabs", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const app = new App(t.renderer, new FakeBackend());
  try {
    await app.perform("status");
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("[h] Главная");
    for (const label of ["[v] Серверы", "[c] Подписка", "[d] Диагностика"])
      expect(frame.split(label)).toHaveLength(2);
    const row = frame
      .split("\n")
      .findIndex((line) => line.includes("[d] Диагностика"));
    await t.mockMouse.click(5, row);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
  } finally {
    await app.close();
  }
});

test("autostart screen renders failed and masked service states", async () => {
  for (const service of [
    { enabled: "enabled", active: "failed", expected: ["автозапуск включён", "текущее состояние: ошибка"] },
    { enabled: "masked", active: "inactive", expected: ["автозапуск замаскирован", "текущее состояние: неактивна"] },
    { enabled: "not-installed", active: "unknown", expected: ["служба не установлена", "текущее состояние: неизвестно"] },
  ]) {
    const t = await createTestRenderer({ width: 100, height: 28 });
    const b = new FakeBackend();
    const original = b.request.bind(b);
    b.request = async (action, value) => {
      const result = await original(action, value);
      if (action === "autostart/read") {
        result.autostart = { gateway: true, connect: false };
        result.autostart_service = { enabled: service.enabled, active: service.active };
      }
      return result;
    };
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      t.mockInput.pressKey("a");
      await settle();
      await t.renderOnce();
      const frame = t.captureCharFrame();
      for (const expected of service.expected) expect(frame).toContain(expected);
    } finally {
      await app.close();
    }
  }
});

test("missing actual autostart status stays explicit and does not infer from saved intent", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read")
      result.autostart = { gateway: true, connect: false };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Запускать шлюз: да");
    expect(frame).toContain("не проверено");
    expect(frame).toContain("недоступно");
    expect(frame).not.toContain("Фактическое состояние службы: автозапуск включён");
  } finally {
    await app.close();
  }
});

test("presentation: completed diagnostics never reopen a screen the user left", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "diagnostics", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("d");
    const task = app.perform("diagnostics");
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("OpenVPN / KDE");
    expect(t.captureCharFrame()).toContain("Подключить VPN");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: installed native separates service startup, connection, disconnect and shutdown", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, gateway_state: "absent" };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    if (action === "backend/start") b.status.gateway_state = "healthy";
    if (action === "start") b.status.networkmanager_active = "yes";
    if (action === "disconnect") b.status.networkmanager_active = "no";
    if (action === "stop") b.status.gateway_state = "stopped";
    return original(action, value);
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("s"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("VPN отключён");
    expect(t.captureCharFrame()).toContain("Подключить VPN");
    expect(b.calls.map(call => call.action)).toEqual(["status", "backend/start"]);
    t.mockInput.pressKey("s"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("VPN подключён");
    t.mockInput.pressKey("x"); await settle();
    expect(b.calls.at(-1)?.action).toBe("start");
    t.mockInput.pressKey("n"); await settle();
    expect(b.status.networkmanager_active).toBe("yes");
    t.mockInput.pressKey("x"); t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Native-служба работает");
    expect(b.status.gateway_state).toBe("healthy");
    t.mockInput.pressKey("z"); await settle();
    expect(b.calls.at(-1)?.action).toBe("disconnect");
    t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Native-служба и VPN остановлены");
    expect(b.calls.map(call => call.action)).toEqual(["status", "backend/start", "start", "disconnect", "stop"]);
  } finally { await app.close(); }
});

test("presentation: native runtime shows the supported lifecycle preview and native service prompts", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", gateway_state: "absent" };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame).toContain("NATIVE PREVIEW");
    expect(frame).toContain("Native-служба");
    expect(frame).toContain("Запустить сервис");
    expect(frame).not.toContain("Docker");
    t.mockInput.pressKey("s");
    await settle();

    b.status.gateway_state = "healthy";
    await app.perform("status");
    t.mockInput.pressKey("z");
    await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame).toContain("остановить сервис и отключить VPN?");
    expect(frame).not.toContain("Docker");
  } finally {
    await app.close();
  }
});


test("presentation: native mode guards unsupported actions before the backend request", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    const unsupported: [Action, string?][] = [
      ["servers/list"],
      ["servers/refresh"],
      ["servers/check-batch", JSON.stringify({ ids: [] })],
      ["servers/speed-batch", JSON.stringify({ ids: [], url: "https://example.invalid/" })],
      ["servers/ping", rows[0]!.server_id],
      ["servers/speed", rows[0]!.server_id],
      ["servers/availability", JSON.stringify({ id: rows[0]!.server_id })],
      ["servers/select", rows[0]!.server_id],
      ["retest/select"],
      ["toggle-mode"],
      ["subscription/read"],
      ["subscription", "https://example.invalid/new"],
      ["diagnostics"],
    ];
    for (const [action, value] of unsupported) await app.perform(action, value);
    t.mockInput.pressKey("v");
    t.mockInput.pressKey("c");
    t.mockInput.pressKey("d");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("native preview");
    expect(t.captureCharFrame()).not.toContain("Docker");
  } finally {
    await app.close();
  }
});


test("presentation: checkout native and native status without installed=true keep subscription setup disabled", async () => {
  for (const installed of [undefined, false] as const) {
    const t = await createTestRenderer({ width: 100, height: 28 });
    const b = new FakeBackend();
    b.status = { ...b.status, runtime: "native", installed };
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      t.mockInput.pressKey("c");
      await settle();
      await t.renderOnce();
      expect(t.captureCharFrame()).toContain("недоступна в native preview");
      expect(t.captureCharFrame()).not.toContain("Enter сохранить");
    } finally {
      await app.close();
    }
  }
});

test("presentation: installed native subscription setup uses the private editor and explicit confirmation", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = {
    ...b.status,
    runtime: "native",
    installed: true,
    subscription: "not configured",
  };
  const app = new App(t.renderer, b);
  const value = "https://example.invalid/private-initial-subscription";
  try {
    await app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    await t.renderOnce();
    let frame = t.captureCharFrame();
    expect(frame).toContain("настройка native-пакета");
    expect(frame).toContain("Polkit");
    expect(frame).toContain("несколько минут");
    expect(frame).toContain("Подключить VPN");
    expect(frame).toContain("автозапуск");
    expect(frame).not.toContain("недоступна в native preview");

    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText(value);
    t.mockInput.pressEnter();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("подтверждение администратора");

    t.mockInput.pressKey("y");
    await settle();
    await t.renderOnce();
    frame = t.captureCharFrame();
    expect(frame).toContain("Native-пакет настроен");
    expect(frame).toContain("нажмите «Подключить VPN» отдельно");
    expect(frame).not.toContain(value);
  } finally {
    await app.close();
  }
});

test("presentation: native setup failures never echo the private editor value", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, subscription: "not configured" };
  const value = "https://example.invalid/private-failure-marker";
  const original = b.request.bind(b);
  b.request = async (action, requestValue) => {
    const result = await original(action, requestValue);
    if (action === "subscription")
      return { ...result, ok: false, reason: value, status: { ...result.status, last_attempt: value } };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText(value);
    t.mockInput.pressEnter();
    t.mockInput.pressKey("y");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Не удалось настроить native-пакет");
    expect(frame).not.toContain(value);
  } finally {
    await app.close();
  }
});

test("presentation: native autostart edits retain the returned runtime and do not connect immediately", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  let options = {
    gateway: false,
    connect: false,
    runtime: "native" as const,
    last_result: "ready",
  };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/set") options = { ...options, ...JSON.parse(value!) };
    if (action === "autostart/read" || action === "autostart/set")
      result.autostart = { ...options };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запускать native-службу: нет");
    t.mockInput.pressKey("g");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запускать native-службу: да");
  } finally {
    await app.close();
  }
});

test("presentation: navigation and diagnostics Back are immediate during a held mutation", async () => {
  const t = await createTestRenderer({ width: 90, height: 28 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "start", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    const task = app.perform("start");
    t.mockInput.pressKey("d");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("OpenVPN / KDE");
    t.mockInput.pressKey("c");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Enter сохранить");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Enter сохранить");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: subscription is visible and editable; paste never dispatches action hotkeys", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    t.mockInput.pressKey("c");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
    t.mockInput.pressKey("u", { ctrl: true });
    await t.mockInput.pasteBracketedText("https://example.invalid/sxrm");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/sxrm");
    t.mockInput.pressEnter();
    await settle();
  } finally {
    await app.close();
  }
});

test("presentation: failed subscription save retains input and Back works while saving", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "subscription",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    t.mockInput.pressKey("c");
    await settle();
    t.mockInput.pressEnter();
    await settle();
    t.mockInput.pressEscape();
    await Bun.sleep(60);
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("Enter сохранить");
    finish({ ...reply(), ok: false, reason: "invalid-request" });
    await settle();
    t.mockInput.pressKey("c");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: server table is sorted by name, keeps columns aligned and exposes combined batch actions", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame.indexOf("Amsterdam")).toBeLessThan(frame.indexOf("Tokyo"));
    for (const text of [
      "Ping всех",
      "Скорость + сайт",
      "Ping всех + сайт",
      "Ping мс",
      "Мбит/с",
    ])
      expect(frame).toContain(text);
    t.mockInput.pressKey("o");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("ping ↑");
    t.resize(90, 18); await t.renderOnce();
    const compact = t.captureCharFrame();
    expect(compact).not.toContain("Docker:");
    expect(compact).not.toContain("Сортировка [o]");
    expect(compact).not.toContain("Сайт: https:");
    expect(compact).toContain("Amsterdam");
    t.resize(100, 30); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Сортировка [o]");
    expect(t.captureCharFrame()).toContain("Docker:");
    t.mockInput.pressArrow("down");
    t.mockInput.pressEnter();
    await t.renderOnce();
  } finally {
    await app.close();
  }
});

test("presentation: batch publishes each result and allows navigation without cancelling", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/check-batch",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    t.mockInput.pressKey("d");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
    b.hold = undefined;
    finish({
      ...reply(),
      catalog: {
        status: "ok",
        servers: rows.map((s) => ({
          ...s,
          latency_ms: 24,
          ping_status: "ready",
          availability: "ready",
        })),
      },
    });
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("2/2");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: Cancel stops the remaining batch and preserves its completed rows", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "servers/speed-batch",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("t");
    await settle();
    t.mockInput.pressKey("k");
    finish({ ...reply(), ok: false, reason: "cancelled" });
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Остановлено");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: changing subscription invalidates old rows and refreshes the catalog", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await app.perform("subscription", "https://example.invalid/new");
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("Amsterdam");
    t.mockInput.pressKey("v");
    await settle();
  } finally {
    await app.close();
  }
});

test("presentation: Docker readiness loads the list on an already-open servers screen", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status.gateway_state = "absent";
  let finish!: (r: Reply) => void;
  b.hold = {
    action: "backend/start",
    promise: new Promise((r) => (finish = r)),
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    const task = app.perform("backend/start");
    t.mockInput.pressKey("v");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Amsterdam");
  } finally {
    finish(reply());
    await settle();
    await app.close();
  }
});

test("presentation: subscription opened during status loads on the first visit after status completes", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    const task = app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/saved");
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: deferred subscription load does not replace text typed while busy", async () => {
  const t = await createTestRenderer({ width: 100, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "status", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  try {
    const task = app.perform("status");
    t.mockInput.pressKey("c");
    await settle();
    await t.mockInput.pasteBracketedText("https://example.invalid/new");
    finish(reply());
    await task;
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("https://example.invalid/new");
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: speed batch requests all IDs and configured URL once, merges mixed outcomes", async () => {
 const t = await createTestRenderer({width:110,height:30}); const b = new FakeBackend();
 b.hold = {action:"servers/speed-batch", promise:Promise.resolve({...reply(),catalog:{status:"ok",servers:[
 {...rows[0]!,ping_status:"ready",latency_ms:20,availability:"failed",status:"ready",download_mbps:44},
 {...rows[1]!,ping_status:"ready",latency_ms:30,availability:"ready",status:"failed"},
 ]}})};
 const app = new App(t.renderer,b);
 try { await app.perform("status"); t.mockInput.pressKey("v"); await settle();
 (app as any).target="https://example.invalid/custom";
 t.mockInput.pressKey("t"); await settle(); await t.renderOnce();
 expect(t.captureCharFrame()).toContain("2/2 · ошибок 2");
 expect(t.captureCharFrame()).toContain("44.0");
 } finally {await app.close();}
});

test("presentation: speed batch shows parallel preparation and speed stage, cancellation sends no subsequent request", async () => {
 const t = await createTestRenderer({width:120,height:30}); const b = new FakeBackend(); let finish!:(r:Reply)=>void;
 b.hold={action:"servers/speed-batch",promise:new Promise(r=>finish=r)}; const app = new App(t.renderer,b);
 try {await app.perform("status");t.mockInput.pressKey("v");await settle();t.mockInput.pressKey("t");await settle();
 const event=(i:number,stage:CheckProgress["stage"]):CheckProgress=>({event:"server-check",server_id:rows[i]!.server_id,stage,ping_status:stage==="start"?"untested":"ready",latency_ms:20,availability:"untested"});
 b.onCheckProgress?.(event(0,"start"));b.onCheckProgress?.(event(1,"start"));await t.renderOnce();
 expect(t.captureCharFrame().split("подготовка").length-1).toBe(2);
 b.onCheckProgress?.(event(0,"speed"));await t.renderOnce();expect(t.captureCharFrame()).toContain("замер");expect(t.captureCharFrame()).toContain("подготовка");
 b.onCheckProgress?.({...event(0,"complete"),availability:"ready",status:"ready",download_mbps:31});
 t.mockInput.pressKey("k");finish({...reply(),ok:false,reason:"cancelled"});await settle();await t.renderOnce();
 expect(t.captureCharFrame()).toContain("31.0");expect(t.captureCharFrame()).toContain("1/2");
 }finally{finish(reply());await settle();await app.close();}
});

test("presentation: home speedometer measures the selected server after ping and removes redundant labels", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 1 }));
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("t"); await settle(); await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("50.0 Мбит/с");
    expect(frame).not.toContain("Подписка: есть");
    expect(frame).not.toContain("Проверки и выбор серверов");
    t.resize(60, 24); await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("СКОРОСТЬ СЕРВЕРА");
    expect(t.captureCharFrame()).toContain("Тест скорости · 24ms, 50.0 Мбит/с");
    t.resize(90, 35); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("СКОРОСТЬ СЕРВЕРА");

  } finally { await app.close(); }
});

test("presentation: switching completes during a held home speed test; late result belongs to original server", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  b.rows = rows.map((row, i) => ({ ...row, selected: i === 0 }));
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/speed", promise: new Promise(r => finish = r) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("t");
    await settle();
    t.mockInput.pressKey("v");
    await settle();
    await app.perform("servers/select", rows[1]!.server_id);
    await t.renderOnce();
    expect(t.captureCharFrame().split("\n").find(line => line.includes("Amsterdam") && line.includes("●"))).toBeDefined();
    finish({ ...reply(), catalog: { status: "ok", server: { ...rows[0]!, selected: true, status: "ready", download_mbps: 77 } } });
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame.split("\n").find(line => line.includes("Tokyo") && line.includes("77.0") && !line.includes("●"))).toBeDefined();
    expect(frame.split("\n").find(line => line.includes("Amsterdam") && line.includes("●"))).toBeDefined();
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: measurement cancellation is scoped and never cancels concurrent server selection", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  let finishCheck!: (r: Reply) => void, finishSelect!: (r: Reply) => void;
  const check = new Promise<Reply>(r => finishCheck = r);
  const select = new Promise<Reply>(r => finishSelect = r);
  const tasks = new Map<Action, string | undefined>();
  const cancelled: (string | undefined)[] = [];
  const original = b.request.bind(b);
  b.request = async (action: Action, value?: string, id?: string) => {
    tasks.set(action, id);
    if (action === "servers/check-batch") return check;
    if (action === "servers/select") return select;
    return original(action, value);
  };
  b.cancel = (id?: string) => { cancelled.push(id); };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("p"); await settle();
    const switching = app.perform("servers/select", rows[1]!.server_id);
    await settle();
    t.mockInput.pressKey("k"); await settle();
    t.mockInput.pressKey("1"); await settle();
    finishCheck({ ...reply(), ok: false, reason: "cancelled" });
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Применяем сервер");
    finishSelect(reply()); await switching;
    t.mockInput.pressKey("v"); await settle();
    await t.renderOnce();
    expect(t.captureCharFrame().split("\n").find(line => line.includes("Amsterdam") && line.includes("●"))).toBeDefined();
  } finally { finishCheck(reply()); finishSelect(reply()); await settle(); await app.close(); }
});

test("presentation: autostart is opt-in, keeps gateway/connect consistent and never starts VPN immediately", async () => {
  const t = await createTestRenderer({ width: 90, height: 28 });
  const b = new FakeBackend();
  let options = { gateway: false, connect: false };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/set") options = JSON.parse(value!);
    if (action.startsWith("autostart/")) result.autostart = { ...options };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("При входе в KDE");
    expect(t.captureCharFrame()).toContain("Подключать VPN: нет");
    t.mockInput.pressKey("a"); await settle();
    t.mockInput.pressKey("g"); await settle();
    t.resize(60, 18); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запускать шлюз: нет");
  } finally { await app.close(); }
});

test("presentation: failed autostart save requires rereading authoritative settings", async () => {
  const t = await createTestRenderer({ width: 90, height: 28 });
  const b = new FakeBackend();
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read") result.autostart = { gateway: false, connect: false };
    if (action === "autostart/set") return { ...result, ok: false, reason: "autostart-unavailable" };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a"); await settle();
    t.mockInput.pressKey("a"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Загрузить настройки");
    expect(t.captureCharFrame()).not.toContain("Подключать VPN: да");
  } finally { await app.close(); }
});

test("presentation: autostart screen keeps saved intent separate from the actual disabled unit", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read") {
      result.autostart = { gateway: true, connect: true };
      result.autostart_service = { enabled: "disabled", active: "inactive" };
    }
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Запускать шлюз: да");
    expect(frame).toContain("Подключать VPN: да");
    expect(frame).toContain("Фактическое состояние службы: автозапуск отключён");
    expect(frame).toContain("текущее состояние: неактивна");
  } finally {
    await app.close();
  }
});

test("presentation: successful autostart saves refresh actual state and preserve native runtime payload", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native" };
  let options = { gateway: true, connect: false, runtime: "native" as const };
  let service = { enabled: "enabled", active: "active" };
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/set") {
      options = { ...options, ...JSON.parse(value!) };
      service = { enabled: options.gateway ? "enabled" : "disabled", active: "inactive" };
    }
    if (action === "autostart/read" || action === "autostart/set") {
      result.autostart = { ...options };
      result.autostart_service = { ...service };
    }
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("при входе в графическую сессию");
    expect(t.captureCharFrame()).not.toContain("При входе в KDE");
    t.mockInput.pressKey("g");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Запускать native-службу: нет");
    expect(t.captureCharFrame()).toContain("автозапуск отключён");
  } finally {
    await app.close();
  }
});

test("presentation: autostart errors clear a previously healthy actual state", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  let reads = 0;
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "autostart/read") {
      reads++;
      if (reads === 1) {
        result.autostart = { gateway: true, connect: false };
        result.autostart_service = { enabled: "enabled", active: "active" };
      } else return { ...result, ok: false, reason: "autostart-unavailable" };
    }
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("a");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("автозапуск включён");
    await app.perform("autostart/read");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("не проверено");
    expect(t.captureCharFrame()).toContain("недоступно");
    expect(t.captureCharFrame()).not.toContain("автозапуск включён");
  } finally {
    await app.close();
  }
});

test("presentation: installed native mode failure preserves strict state and retry shows smart then strict", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true };
  let fail = true;
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    if (action === "toggle-mode") {
      if (fail) { fail = false; return { ...reply(b.status), ok: false, reason: "dns-failed" }; }
      b.status.routing_mode = b.status.routing_mode === "strict" ? "smart" : "strict";
    }
    return original(action, value);
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("m"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Проверка DNS после подключения не прошла");
    expect(t.captureCharFrame()).toContain("Режим: строгий");
    t.mockInput.pressKey("m"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Режим: умный");
    expect(t.captureCharFrame()).not.toContain("Проверка DNS после подключения не прошла");
    t.mockInput.pressKey("m"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Режим: строгий");
  } finally { await app.close(); }
});

test("presentation: installed native diagnostics failure can be retried without changing connectivity", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, networkmanager_active: "yes" };
  let attempts = 0;
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "diagnostics") {
      if (++attempts === 1) return { ...result, ok: false, reason: "diagnostics-unavailable" };
      b.status.diagnostics = "available";
      return reply(b.status);
    }
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("d"); await settle();
    t.mockInput.pressKey("u"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Не удалось создать журнал");
    t.mockInput.pressKey("u"); await settle(); await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Native-служба:");
    expect(frame).toContain("Проверка: выполнена");
    expect(frame).not.toContain("Docker");
    expect(frame).not.toContain("Не удалось создать журнал");
    t.mockInput.pressKey("h"); await Bun.sleep(60); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("VPN подключён");
  } finally { await app.close(); }
});

test("presentation: failed native status polls retain native server screen without stale readiness", async () => {
  const t = await createTestRenderer({ width: 120, height: 30 });
  const b = new FakeBackend();
  b.status = { ...initial, runtime: "native", installed: true, vpn_state: "healthy", networkmanager_active: "yes" };
  const request = b.request.bind(b);
  b.request = async (action, value) => {
    const r = await request(action, value);
    if (action.startsWith("profiles/")) r.profiles = { active: "profile_legacy1", items: [{ id: "profile_legacy1", name: "Fixture", kind: "subscription", count: 2, stale: false }] };
    return r;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("VPN подключён");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("1");
    await settle();
    b.request = async () => { throw new Error("backend-unavailable"); };
    for (let attempt = 0; attempt < 2; attempt++) {
      await app.perform("status");
      await t.renderOnce();
      const frame = t.captureCharFrame();
      expect(frame).toContain("Статус недоступен");
      expect(frame).toContain("NATIVE");
      expect(frame).toContain("Служба управления недоступна");
      expect(frame).toContain("Запустить сервис");
      expect(frame).not.toContain("Docker");
      expect(frame).not.toContain("VPN подключён");
    }
  } finally {
    await app.close();
  }
});

test("presentation: an initial status failure does not invent native identity", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.request = async () => { throw new Error("backend-unavailable"); };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Служба управления недоступна");
    expect(t.captureCharFrame()).not.toContain("NATIVE");
  } finally {
    await app.close();
  }
});

test("presentation: failed reapplication replaces drift instruction with the actual busy failure", async () => {
  const t = await createTestRenderer({width:100,height:28});
  const b = new FakeBackend();
  b.status = {...b.status,runtime:"native",installed:true,gateway_state:"inactive"};
  const app = new App(t.renderer,b);
  try {
    await app.perform("status");
    b.hold = {action:"backend/start",promise:Promise.resolve({...reply(b.status),ok:false,reason:"native-network-settings-changed"})};
    await app.perform("backend/start");
    b.hold = {action:"network/reapply",promise:Promise.resolve({...reply(b.status),ok:false,reason:"native-network-busy"})};
    t.mockInput.pressKey("f"); await settle(); await t.renderOnce();
    const frame=t.captureCharFrame();
    expect(frame).toContain("другая системная служба");
    expect(frame).not.toContain("настройки VPN поменялись");
    expect(frame).not.toContain("применить их ещё раз");
    t.mockInput.pressKey("f"); await settle();
  } finally { await app.close(); }
});
