import { test, expect } from "bun:test";
import { createTestRenderer } from "@opentui/core/testing";
import { App, cell } from "./app";
import { connection } from "./model";
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

test("shell footer has state-specific actions and no stale hints or refresh timestamp", async () => {
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
    expect(t.captureCharFrame()).toContain("s подключить");
    b.status.networkmanager_active = "yes";
    await app.perform("status");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("x отключить");
    t.resize(60, 20);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("? помощь");
  } finally {
    await app.close();
  }
});

test("shell has one clickable tab row", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const app = new App(t.renderer, new FakeBackend());
  try {
    await app.perform("status");
    await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("[h] Главная");
    for (const label of ["[Серверы]", "Профили", "Диагностика", "Настройки"])
      expect(frame.split(label)).toHaveLength(2);
    const row = frame
      .split("\n")
      .findIndex((line) => line.includes("Диагностика"));
    await t.mockMouse.click(26, row);
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
    expect(t.captureCharFrame()).toContain("s подключить");
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
    expect(t.captureCharFrame()).toContain("○ отключён");
    expect(t.captureCharFrame()).toContain("s подключить");
    t.mockInput.pressKey("s"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("● подключён");
    t.mockInput.pressKey("x"); await settle();
    t.mockInput.pressKey("n"); await settle();
    t.mockInput.pressKey("x"); t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Native-служба работает");
    t.mockInput.pressKey("z"); await settle();
    t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Native-служба и VPN остановлены");
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
      expect(t.captureCharFrame()).toContain("доступна только в установленном native-пакете");
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
    expect(frame.replace(/\s+/g, " ")).toContain("нажмите «Подключить VPN» отдельно");
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
      "p ping",
      "t скорость",
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
    expect(t.captureCharFrame()).toContain("ping ↑");
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
 expect(t.captureCharFrame().split("\n").map(line => line.slice(0, 82)).join("\n").split("подготовка").length-1).toBe(2);
 b.onCheckProgress?.(event(0,"speed"));await t.renderOnce();expect(t.captureCharFrame()).toContain("замер");expect(t.captureCharFrame()).toContain("подготовка");
 b.onCheckProgress?.({...event(0,"complete"),availability:"ready",status:"ready",download_mbps:31});
 t.mockInput.pressKey("k");finish({...reply(),ok:false,reason:"cancelled"});await settle();await t.renderOnce();
 expect(t.captureCharFrame()).toContain("31.0");expect(t.captureCharFrame()).toContain("1/2");
 }finally{finish(reply());await settle();await app.close();}
});

test("presentation: single-server speed appears in the table and status line", async () => {
  const t = await createTestRenderer({ width: 90, height: 35 });
  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 1 }));
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("T"); await settle(); await t.renderOnce();
    const frame = t.captureCharFrame();
    expect(frame).toContain("50.0 Мбит/с");
    expect(frame).not.toContain("Подписка: есть");
    expect(frame).not.toContain("Проверки и выбор серверов");
    t.resize(60, 24); await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("СКОРОСТЬ СЕРВЕРА");
    expect(t.captureCharFrame().split("\n").slice(2).find(line => line.includes("Amsterdam"))).toMatch(/24\s+50\.0/);
    t.resize(90, 35); await t.renderOnce();
    expect(t.captureCharFrame().split("\n")[0]).toContain("50.0 Мбит/с");

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
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("T");
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
    expect(t.captureCharFrame()).toContain("режим: строгий");
    t.mockInput.pressKey("m"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("режим: умный");
    expect(t.captureCharFrame()).not.toContain("Проверка DNS после подключения не прошла");
    t.mockInput.pressKey("m"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("режим: строгий");
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
    expect(t.captureCharFrame()).toContain("● подключён");
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
    expect(t.captureCharFrame()).toContain("● подключён");
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
      expect(frame).not.toContain("● подключён");
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

test("presentation: installed native reuses the server table, selection, checks and mode controls", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true };
  const original=b.request.bind(b);
  b.request=async(action,value)=>{const r=await original(action,value);if(action.startsWith("profiles/"))r.profiles={active:"profile_legacy1",items:[{id:"profile_legacy1",name:"Existing",kind:"subscription",count:2,stale:false}]};return r};
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("1"); await settle(); await t.renderOnce();
    const frame=t.captureCharFrame();
    expect(frame).toContain("Amsterdam");
    expect(frame).toContain("p ping");
    expect(frame).not.toContain("пока не подключены");
    t.mockInput.pressEnter(); await settle();

    await app.perform("servers/ping",rows[0]!.server_id);

    t.mockInput.pressKey("h"); await Bun.sleep(60);
    t.mockInput.pressKey("m"); await settle();

    t.resize(70,18);await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("недоступен в native");
  } finally { await app.close(); }
});

test("presentation: native profiles show separate server lists and adding a link does not connect", async () => {
 const t=await createTestRenderer({width:100,height:28});const b=new FakeBackend();
 b.status={...b.status,runtime:"native",installed:true};
 let active="profile_one";
 const items=[{id:"profile_one",name:"Subscription",kind:"subscription" as const,count:1,stale:true},{id:"profile_two",name:"My VPS",kind:"server" as const,count:1,stale:false}];
 b.rows=[{...rows[0]!,profile_id:"profile_one"},{...rows[1]!,profile_id:"profile_two"}];
 const original=b.request.bind(b);
 b.request=async(action,value)=>{const r=await original(action,value);if(action==="profiles/open")active=value!;if(action.startsWith("profiles/"))r.profiles={active,items};return r};
 const app=new App(t.renderer,b);
 try{
  await app.perform("status");t.mockInput.pressKey("c");await settle();await t.renderOnce();
  expect(t.captureCharFrame()).toContain("My VPS");expect(t.captureCharFrame()).toContain("сохранённый список");
  t.mockInput.pressKey("2");await settle();await t.renderOnce();
  expect(t.captureCharFrame()).toContain("Amsterdam");expect(t.captureCharFrame()).not.toContain("Tokyo");

  t.mockInput.pressKey("c");await settle();t.mockInput.pressKey("n");await settle();
  t.mockInput.typeText("Other VPS");t.mockInput.pressEnter();await settle();
  t.mockInput.typeText("hy2://private-marker@example.invalid#VPS");t.mockInput.pressEnter();await settle();await t.renderOnce();
  expect(t.captureCharFrame()).not.toContain("private-marker");
  t.resize(70,18);await t.renderOnce();expect(t.captureCharFrame()).toContain("Добавить профиль");
 }finally{await app.close()}
});

test("presentation: failed profile open retains the current source and avoids catalog reload", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true };
  b.rows = [{ ...rows[0]!, profile_id: "profile_one" }, { ...rows[1]!, profile_id: "profile_two" }];
  const items = [
    { id: "profile_one", name: "First profile", kind: "subscription" as const, count: 1, stale: false },
    { id: "profile_two", name: "Second profile", kind: "server" as const, count: 1, stale: false },
  ];
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "profiles/open" && value === "profile_two")
      return { ...result, ok: false, reason: "profiles-failed" };
    if (action.startsWith("profiles/")) result.profiles = { active: "profile_one", items };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("c"); await settle();
    t.mockInput.pressKey("1"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Tokyo");
    t.mockInput.pressKey("c"); await settle();
    t.mockInput.pressKey("2"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Не удалось сохранить профиль");

    t.mockInput.pressKey("1"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Tokyo");
    expect(t.captureCharFrame()).not.toContain("Amsterdam");

  } finally { await app.close(); }
});

test("presentation: removing the active source browses its fallback without changing the VPN", async () => {
  const t = await createTestRenderer({ width: 110, height: 30 });
  const b = new FakeBackend();
  b.status = { ...b.status, runtime: "native", installed: true, networkmanager_active: "yes" };
  b.rows = [{ ...rows[0]!, profile_id: "profile_one" }, { ...rows[1]!, profile_id: "profile_two" }];
  let items = [
    { id: "profile_one", name: "First profile", kind: "subscription" as const, count: 1, stale: false },
    { id: "profile_two", name: "Second profile", kind: "server" as const, count: 1, stale: false },
  ];
  let active = "profile_one";
  const original = b.request.bind(b);
  b.request = async (action, value) => {
    const result = await original(action, value);
    if (action === "profiles/remove") {
      items = items.slice(1);
      active = "profile_two";
    }
    if (action.startsWith("profiles/")) result.profiles = { active, items };
    return result;
  };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("c"); await settle();
    t.mockInput.pressKey("X"); await settle();

    t.mockInput.pressKey("y"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("First profile");
    expect(t.captureCharFrame()).toContain("Second profile");
    t.mockInput.pressKey("1"); await settle(); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Amsterdam");
    expect(t.captureCharFrame()).not.toContain("Tokyo");


  } finally { await app.close(); }
});

test("presentation: cached errors do not masquerade as tests in the current session", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const server = {
    ...rows[0],
    status: "failed",
    ping_status: "failed",
    download_mbps: undefined,
  };
  const app = new App(t.renderer, {
    request: async (action) => ({
      ...reply(),
      catalog:
        action === "servers/list"
          ? { status: "ok", servers: [server] }
          : {
              status: "ok",
              server: { ...server, ping_status: "ready", latency_ms: 15 },
            },
    }),
    close: async () => {},
  });
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("ошибка");
    expect(t.captureCharFrame()).toContain("не проверен");
    await app.perform("servers/ping", server.server_id);
    await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("ошибка");
    await app.perform("servers/speed", server.server_id);
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("ошибка");
  } finally {
    await app.close();
  }
});

test("presentation: a successful site check marks a server ready without claiming a speed result", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    await t.renderOnce();
    const row = t
      .captureCharFrame()
      .split("\n")
      .find((line) => line.includes("Amsterdam"))!;
    expect(row).toContain("готов");
    expect(row).toContain("да");
    expect(row).toContain("—");
    expect(row).not.toContain("50.0");
  } finally {
    await app.close();
  }
});

test("presentation: combined checks submit one snapshot once without restarting on navigation", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  b.rows = Array.from({ length: 12 }, (_, i) => ({ ...rows[0], server_id: `srv_${String(i).padStart(27, "0")}` }));
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    t.mockInput.pressKey("d");
    t.mockInput.pressKey("v");
    await settle();
    await t.renderOnce();

    expect(t.captureCharFrame()).not.toContain("5 потоков");
  } finally { await app.close(); }
});

test("presentation: batch failure retains the actionable outdated backend explanation", async () => {
  const t = await createTestRenderer({ width: 120, height: 30 });
  const b = new FakeBackend();
  b.hold = { action: "servers/check-batch", promise: Promise.resolve({ ...reply(), ok: false, reason: "backend-outdated" }) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    t.mockInput.pressKey("p");
    await settle();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Нужен обновлённый Docker-бэкенд");
    expect(t.captureCharFrame()).not.toContain("Результаты сохранены");
  } finally { await app.close(); }
});

test("presentation: ping and site render before batch completion and completed rows stop spinning", async () => {
 const t = await createTestRenderer({ width:120, height:30 });
 const b = new FakeBackend();
 let finish!: (r: Reply) => void;
 b.hold = { action:"servers/check-batch", promise:new Promise(r => finish=r) };
 const app = new App(t.renderer,b);
 try {
  await app.perform("status");
  t.mockInput.pressKey("v"); await settle();
  t.mockInput.pressKey("p"); await settle();
  const event: CheckProgress = { event:"server-check", server_id:rows[0].server_id, stage:"ping", ping_status:"ready", latency_ms:37, availability:"untested" };
  b.onCheckProgress?.(event);
  await t.renderOnce();
  let row=t.captureCharFrame().split("\n").find(s=>s.includes("Tokyo"))!;
  expect(row).toContain("37");
  expect(row).not.toContain("да");
  expect(t.captureCharFrame()).toContain("0/2");
  b.onCheckProgress?.({...event,stage:"complete",availability:"ready"});
  b.onCheckProgress?.({...event,stage:"complete",availability:"ready"});
  await t.renderOnce();
  row=t.captureCharFrame().split("\n").find(s=>s.includes("Tokyo"))!;
  expect(row).toContain("37"); expect(row).toContain("да");
  expect(row).not.toMatch(/[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/);
  expect(t.captureCharFrame()).toContain("1/2");
  t.mockInput.pressKey("d"); await t.renderOnce();
  expect(t.captureCharFrame()).toContain("OpenVPN / KDE");
  t.mockInput.pressKey("v"); await t.renderOnce();
  expect(t.captureCharFrame()).toContain("37");
  finish({...reply(),catalog:{status:"ok",servers:rows.map(s=>({...s,ping_status:"ready",availability:"ready",latency_ms:37}))}});
  await settle(); await t.renderOnce();
  expect(t.captureCharFrame()).toContain("2/2");
  expect(t.captureCharFrame()).not.toContain("3/2");

 } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: failed current ping overrides an earlier successful speed result", async () => {
  const t = await createTestRenderer({ width: 110, height: 24 });
  const b = new FakeBackend();
  const app = new App(t.renderer,b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v");
    await settle();
    await app.perform("servers/speed",rows[0]!.server_id);
    let finish!: (r: Reply) => void;
    b.hold = { action: "servers/check-batch", promise: new Promise(r => finish=r) };
    t.mockInput.pressKey("p");
    await settle();
    b.onCheckProgress?.({ event:"server-check",server_id:rows[0]!.server_id,stage:"complete",ping_status:"failed",latency_ms:0,availability:"untested" });
    await t.renderOnce();
    const row = t.captureCharFrame().split("\n").find(line => line.includes("Tokyo"))!;
    expect(row).toContain("50.0");
    expect(row).toMatch(/ошибка\s*$/);
    finish(reply()); await settle();
  } finally { await app.close(); }
});

test("presentation: only assigned workers spin while remaining servers wait in the queue", async () => {
 const t = await createTestRenderer({ width:120, height:30 });
 const b = new FakeBackend();
 b.rows = Array.from({length:6},(_,i)=>({...rows[0]!,server_id:`srv_${String.fromCharCode(97+i).repeat(27)}`,display_name:`Node ${i}`}));
 let finish!: (r:Reply)=>void;
 b.hold = {action:"servers/check-batch",promise:new Promise(r=>finish=r)};
 const app = new App(t.renderer,b);
 const event = (i:number,stage:"start"|"complete"): CheckProgress => ({event:"server-check",server_id:b.rows[i]!.server_id,stage,ping_status:stage==="start"?"untested":"ready",latency_ms:stage==="start"?0:12,availability:stage==="start"?"untested":"ready"});
 try {
  await app.perform("status"); t.mockInput.pressKey("v"); await settle();
  t.mockInput.pressKey("p"); await settle();
  for(let i=0;i<5;i++) b.onCheckProgress?.(event(i,"start"));
  await t.renderOnce();
  let lines=t.captureCharFrame().split("\n").filter(s=>s.includes("Node "));
  expect(lines.filter(s=>s.includes("подготовка"))).toHaveLength(5);
  expect(lines.find(s=>s.includes("Node 5"))).toContain("в очереди");
  expect(lines.find(s=>s.includes("Node 5"))).not.toMatch(/[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/);
  b.onCheckProgress?.(event(0,"complete")); b.onCheckProgress?.(event(5,"start"));
  await t.renderOnce();
  lines=t.captureCharFrame().split("\n").filter(s=>s.includes("Node "));
  expect(lines.filter(s=>s.includes("подготовка"))).toHaveLength(5);
  expect(lines.find(s=>s.includes("Node 0"))).toContain("готов");
  expect(lines.find(s=>s.includes("Node 5"))).toContain("подготовка");
 } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: quit cancels and drains in-flight work without destroying the renderer prematurely", async () => {
  const t = await createTestRenderer({ width: 80, height: 24 });
  const b = new FakeBackend();
  let finish!: (r: Reply) => void;
  b.hold = { action: "start", promise: new Promise((r) => (finish = r)) };
  const app = new App(t.renderer, b);
  await app.perform("status");
  const task = app.perform("start");
  await app.close();
  await app.close();
  await t.renderOnce();
  expect(t.captureCharFrame()).toContain("Завершаем текущую операцию");
  finish(reply());
  await task;
});

test("presentation: disconnect confirmation defaults to No and requires an explicit Yes", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  b.status.networkmanager_active = "yes";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("x");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Точно хотите отключить VPN?");

    t.mockInput.pressEnter();
    await settle();

    t.mockInput.pressKey("x");
    t.mockInput.pressEscape();
    await Bun.sleep(60);

    t.mockInput.pressKey("x");
    t.mockInput.pressArrow("down");
    t.mockInput.pressEnter();
    await settle();

    t.mockInput.pressKey("x");
    t.mockInput.pressEnter();
    await settle();

  } finally {
    await app.close();
  }
});

test("presentation: home speed test skips download after failed ping and allows navigation", async () => {
  const t = await createTestRenderer({ width: 60, height: 24 });
  const b = new FakeBackend();
  b.rows = rows.map((s, i) => ({ ...s, selected: i === 0 }));
  let finish!: (r: Reply) => void;
  b.hold = { action: "servers/ping", promise: new Promise(r => { finish = r; }) };
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("v"); await settle();
    t.mockInput.pressKey("T"); await settle();
    t.resize(60, 16); await t.renderOnce();
    const narrow = t.captureCharFrame();
    expect(narrow).toContain("режим: строгий");
    expect(narrow).toContain("k отменить");
    expect(narrow).not.toContain("Ping · выполняется");
    expect(narrow).toContain("Ping");
    t.mockInput.pressKey("d"); await t.renderOnce();
    expect(t.captureCharFrame()).not.toContain("СКОРОСТЬ СЕРВЕРА");
    finish({ ...reply(), ok: false, reason: "failed" }); await settle();

    t.mockInput.pressEscape(); await Bun.sleep(60); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Серверы");
    expect(t.captureCharFrame()).toContain("? помощь");
  } finally { finish(reply()); await settle(); await app.close(); }
});

test("presentation: held native and Docker lifecycle actions use their own runtime labels", async () => {
  for (const runtime of ["native", "docker"] as const) {
    const t = await createTestRenderer({ width: 120, height: 30 });
    const b = new FakeBackend();
    b.status = { ...initial, runtime, installed: runtime === "native" };
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      for (const action of ["backend/start", "stop"] as const) {
        let finish!: (r: Reply) => void;
        b.hold = { action, promise: new Promise(resolve => { finish = resolve; }) };
        const work = app.perform(action);
        await t.renderOnce();
        const frame = t.captureCharFrame();
        const verb = action === "stop" ? "Останавливаем" : "Запускаем";
        expect(frame).toContain(`${verb} ${runtime === "native" ? "сервис" : "Docker"}`);
        if (runtime === "native") expect(frame).not.toContain("Docker");
        finish(reply(b.status));
        await work;
        b.hold = undefined;
      }
    } finally {
      await app.close();
    }
  }
});

test("presentation: unknown routing mode is not presented as strict", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend(); b.status.routing_mode = "unknown";
  const app = new App(t.renderer, b);
  try {
    await app.perform("status"); await t.renderOnce();
    expect(t.captureCharFrame()).toContain("режим: неизвестно");
    expect(app.actions().find(item => item.key === "m")?.label).toContain("неизвестно");
  } finally { await app.close(); }
});

for (const width of [48, 100, 140]) {
  test(`presentation: native network repair is offered inline and f runs once at ${width} columns`, async () => {
    const t = await createTestRenderer({ width, height: 28 });
    const b = new FakeBackend();
    b.status = { ...b.status, runtime: "native", installed: true, gateway_state: "inactive", last_attempt: "private-attempt" };
    const app = new App(t.renderer, b);
    try {
      await app.perform("status");
      b.hold = { action: "backend/start", promise: Promise.resolve({ ...reply(b.status), ok: false, reason: "native-network-settings-changed" }) };
      await app.perform("backend/start");
      await t.renderOnce();
      const frame = t.captureCharFrame();
      expect(frame).toContain("Сетевые настройки VPN поменялись,");
      expect(frame).toContain("нажмите [f]");
      expect(frame).toContain("ещё раз.");
      expect(frame).not.toContain("Действие не выполнено");
      expect(frame).not.toContain("[f] Действия");
      expect(frame).not.toContain("private-attempt");
      expect(frame).toContain("s запустить");
      let resolve!: (r: Reply) => void;
      b.hold = { action: "network/reapply", promise: new Promise(r => resolve = r) };
      t.mockInput.pressKey("f"); await settle();
      t.mockInput.pressKey("f"); await settle();
      resolve(reply(b.status)); await settle(); await t.renderOnce();
      expect(t.captureCharFrame()).toContain("Сетевые настройки VPN применены.");
      t.mockInput.pressKey("f"); await settle();
      b.hold = { action: "backend/start", promise: Promise.resolve({ ...reply(b.status), ok: false, reason: "native-network-settings-changed" }) };
      await app.perform("backend/start");
      await app.perform("disconnect");
      t.mockInput.pressKey("f"); await settle();
    } finally { await app.close(); }
  });
}

test("presentation: stopping Docker also requires confirmation; mouse No does not disconnect", async () => {
  const t = await createTestRenderer({ width: 100, height: 28 });
  const b = new FakeBackend();
  const app = new App(t.renderer, b);
  try {
    await app.perform("status");
    t.mockInput.pressKey("z");
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain(
      "остановить Docker и отключить VPN?",
    );
  } finally {
    await app.close();
  }
});

test("presentation: connection requires healthy Docker and active KDE", () => {
  expect(connection({ ...initial, networkmanager_active: "yes" }).title).toBe(
    "VPN подключён",
  );
  expect(connection(initial).title).toBe("VPN отключён");
  expect(
    connection({
      ...initial,
      gateway_state: "unhealthy",
      networkmanager_active: "yes",
    }).title,
  ).not.toBe("VPN подключён");
  expect(cell("東京", 6)).toBe("東京  ");
  expect(Bun.stringWidth(cell("🇫🇮 Helsinki", 8))).toBe(8);
});

test("presentation: home speed starts during status polling and queues behind a mutation", async () => {
  const t = await createTestRenderer({ width: 100, height: 30 });
  const b = new FakeBackend();
  b.rows = rows.map((row, i) => ({ ...row, selected: i === 0 }));
  const app = new App(t.renderer, b);
  const state = app as any;
  try {
    await app.perform("status");
    let finish!: (result: Reply) => void;
    b.hold = { action: "status", promise: new Promise(resolve => { finish = resolve; }) };
    const poll = app.perform("status");
    await state.testHomeSpeed();
    finish(reply()); await poll;
    b.hold = { action: "start", promise: new Promise(resolve => { finish = resolve; }) };
    const mutation = app.perform("start");
    await state.testHomeSpeed();
    await t.renderOnce();
    expect(t.captureCharFrame()).toContain("Операция поставлена в очередь");
    finish(reply()); await mutation;
  } finally { await app.close(); }
});
