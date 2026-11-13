import { expect, test } from "bun:test";
import { AppState, type Screen } from "./app-state";
import { FakeBackend, initial, rows } from "./app-test-fixtures";
import { matchesAction } from "./action-registry";
import { recoveryFor } from "./recovery";

const candidates = [..."abcdefghijklmnopqrstuvwxyz0123456789[]-=/.,; '\\", "escape", "return", "tab", "shift+tab", "up", "down", "left", "right", "pageup", "pagedown", "home", "end", "backspace", "delete", "space", "ctrl+c", "~", "?"];
const screens = ["home", "home-stopped", "home-connected", "home-no-subscription", "home-busy", "home-offline", "home-closing", "servers", "servers-menu", "servers-empty", "servers-stale", "servers-busy", "subscription", "profiles", "profile-name", "profile-source", "profile-remove", "target", "diagnostics", "autostart", "autostart-loaded", "confirm-disconnect", "confirm-stop", "confirm-subscription", "tasks", "cancel-confirm", "recovery", "recovery-offline", "recovery-reconnecting", "recovery-reapply-busy", "retry-confirm"] as const;
function fixture(name: typeof screens[number], runtime: "docker" | "preview" | "native") {
  const backend = new FakeBackend(), state = new AppState(backend);
  state.status = { ...initial, runtime: runtime === "docker" ? "docker" : "native", installed: runtime === "native" };
  if (name === "home-stopped") state.status.gateway_state = "stopped";
  if (name === "home-connected") state.status.networkmanager_active = "yes";
  if (name === "home-no-subscription") state.status.subscription = "missing";
  state.screen = (name.startsWith("home") ? "home" : name.startsWith("servers") ? "servers" : name.startsWith("confirm-") ? "confirm" : name === "autostart-loaded" ? "autostart" : name.startsWith("recovery-") ? "recovery" : name) as Screen;
  state.servers = name === "servers-empty" ? [] : structuredClone(rows);
  state.serverID = rows[1].server_id;
  state.catalogLoaded = name !== "servers-empty"; state.catalogStale = name === "servers-stale";
  state.serverMenu = name === "servers-menu";
  state.profiles = { active: "profile_legacy1", items: Array.from({length: 5}, (_, i) => ({ id: `profile_${i}`, name: `Profile ${i}`, kind: "subscription" as const, count: 1, stale: false })) };
  state.editorValue = name === "profile-name" ? "Fixture" : "https://example.invalid/";
  if (name.startsWith("confirm-")) state.confirmation = name.slice(8) as typeof state.confirmation;
  if (name === "confirm-subscription") state.pendingSubscription = state.editorValue;
  if (name === "home-closing") state.closing = true;
  if (name === "home-offline" || name === "recovery-offline" || name === "recovery-reconnecting") state.backendOffline = true;
  if (name === "recovery-reconnecting") state.reconnecting = true;
  if (name === "home-busy" || name === "servers-busy" || name === "recovery-reapply-busy" || name === "tasks" || name === "cancel-confirm") { state.tasks.set("fixture", { action: "start", measurement: false, started: 0, phase: "" }); state.cancelTaskID = "fixture"; }
  if (name === "recovery" || name.startsWith("recovery-") || name === "retry-confirm") { state.recovery = { ...recoveryFor("start", "dns-failed"), action: "start" }; state.retryOperation = state.recovery; }
  if (name === "recovery-reapply-busy") state.recovery = {...recoveryFor("backend/start", "native-network-settings-changed", "native"), action: "backend/start"};
  if (name === "autostart-loaded") state.autostart = {gateway: false, connect: false};
  return { state, backend };
}
function event(name: string) { return {name: name.replace(/^(ctrl|shift)\+/, ""), ctrl: name.startsWith("ctrl+"), shift: name.startsWith("shift+"), preventDefault() {}}; }
function snapshot(state: AppState, backend: FakeBackend, includeNotice = true) {
  const ignored = new Set(["backend", "listeners", "timer", "staleTimer", "poll", ...(!includeNotice ? ["notice", "noticeIdentity", "noticeAttempt", "error"] : [])]);
  const fields = Object.fromEntries(Object.entries(state).filter(([key, value]) => !ignored.has(key) && typeof value !== "function"));
  return JSON.stringify({ fields, calls: backend.calls, cancelledCalls: backend.cancelled }, (_key, value) =>
    value instanceof Map ? [...value.entries()] : value instanceof Set ? [...value] : value);
}

test("registered effective shortcuts match observed state changes and backend calls", async () => {
  for (const screen of screens) for (const runtime of ["docker", "preview", "native"] as const) {
    const initialFixture = fixture(screen, runtime);
    const listed = initialFixture.state.actions();
    const keys = [...new Set([...candidates, ...listed.flatMap(a => a.keys)])];
    for (const name of keys) {
      const {state, backend} = fixture(screen, runtime);
      const action = state.actions().find(a => matchesAction(a, event(name)));
      const before = snapshot(state, backend, action?.available !== false);
      state.onKey(event(name));
      for (let tick = 0; tick < 8; tick++) await Promise.resolve();
      const changed = snapshot(state, backend, action?.available !== false) !== before;
      expect({screen, runtime, name, changed}).toEqual({screen, runtime, name, changed: action?.available ?? false});
      if (action && !action.available) {
        expect({screen, runtime, name, noticed: !!state.notice}).toEqual({screen, runtime, name, noticed: action.unavailableBehavior === "notice"});
      }
      state.tasks.clear();
      void state.close();
    }
  }
});

test("reading table rows does not change the viewport", () => {
  const {state} = fixture("servers", "docker");
  state.viewStart = 99;
  state.visibleServers();
  expect(state.viewStart).toBe(99);
  state.syncServerViewport();
  expect(state.viewStart).toBe(0);
});


test("server movement and catalog changes keep the cursor in the viewport", () => {
  const {state} = fixture("servers", "docker");
  state.servers = Array.from({length: 30}, (_, i) => ({...rows[0], server_id: `server_${i}`, display_name: `Server ${String(i).padStart(2, "0")}`, latency_ms: 30 - i}));
  state.serverID = state.servers[0].server_id;
  state.onKey(event("pagedown"));
  expect(state.viewStart).toBeGreaterThan(0);
  expect(state.visibleServers().some(server => server.server_id === state.serverID)).toBe(true);
  state.onKey(event("o"));
  expect(state.visibleServers().some(server => server.server_id === state.serverID)).toBe(true);
  state.updateCatalog({ok: true, reason: "ok", code: 0, status: {...initial}, catalog: {status: "ok", servers: state.servers.slice(-3)}}, "servers/list");
  expect(state.visibleServers().some(server => server.server_id === state.serverID)).toBe(true);
});

test("ping shortcut and its alias retain their default-prevention behavior", () => {
  for (const [name, expected] of [["p", true], ["a", false]] as const) {
    const {state} = fixture("servers", "docker");
    let prevented = false;
    state.runBatch = async () => {};
    state.onKey({...event(name), preventDefault() { prevented = true; }});
    expect(prevented).toBe(expected);
  }
});
