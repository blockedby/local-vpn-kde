import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { serverMenuActions } from "./action-registry";
import { FakeBackend, initial, reply, rows, settle } from "./app-test-fixtures";

function fixture() {
  const backend = new FakeBackend(), state = new AppState(backend);
  state.status = { ...initial }; state.screen = "servers"; state.tab = "servers";
  state.servers = structuredClone(rows); state.serverID = rows[0]!.server_id; state.catalogLoaded = true;
  const key = (name: string) => state.onKey({ name, preventDefault() {} });
  return { state, backend, key };
}

test("row menu navigates with clamping, Home/End and escape restores its server", async () => {
  const { state, key } = fixture();
  try {
    key("return"); expect(state.serverMenu).toBe(true); expect(state.serverMenuIndex).toBe(0);
    expect(serverMenuActions(state).map(a => [a.id, a.key])).toEqual([["servers:select", "return"], ["servers:single-check", ""], ["servers:single-speed", "shift+t"], ["servers:details", "l"], ["servers:rename", "n"]]);
    key("up"); expect(state.serverMenuIndex).toBe(0); key("end"); key("down"); expect(state.serverMenuIndex).toBe(4);
    key("home"); key("down"); expect(state.serverMenuIndex).toBe(1);
    state.serverID = rows[1]!.server_id; key("escape"); expect(state.serverMenu).toBe(false); expect(state.serverID).toBe(rows[0]!.server_id);
  } finally { await state.close(); }
});

test("menu actions use the existing handlers and captured target, including queued speed", async () => {
  for (const trigger of ["return", "T"]) {
    const { state, key } = fixture(); let speedID: string | undefined;
    state.testHomeSpeed = async id => { speedID = id; };
    try { key("return"); state.serverID = rows[1]!.server_id; if (trigger === "return") { key("down"); key("down"); } key(trigger); expect(speedID).toBe(rows[0]!.server_id); expect(state.serverMenu).toBe(false); } finally { await state.close(); }
  }
  {
    const { state, key } = fixture(); let speedID: string | undefined;
    state.testHomeSpeed = async id => { speedID = id; };
    try { key("T"); expect(speedID).toBe(rows[0]!.server_id); } finally { await state.close(); }
  }
  for (const viaMenu of [false, true]) {
    const { state, key } = fixture();
    try { if (viaMenu) { key("return"); key("end"); key("up"); key("return"); } else key("l"); expect(state.screen).toBe("details"); expect(state.serverID).toBe(rows[0]!.server_id); } finally { await state.close(); }
  }
  const { state, key } = fixture();
  try { state.tasks.set("busy", { action: "start", measurement: false, started: 0, phase: "" }); key("return"); key("T"); expect(state.queue.map(q => [q.action, q.value])).toEqual([["servers/speed", rows[0]!.server_id]]); } finally { state.tasks.clear(); await state.close(); }
});

test("selection refused by conflicting work is never queued or replayed", async () => {
  const { state, backend, key } = fixture();
  try {
    state.tasks.set("busy", { action: "start", measurement: false, started: 0, phase: "" });
    key("return"); key("return"); key("y");
    expect(backend.calls).toEqual([]); expect(state.queue).toHaveLength(0);
    state.tasks.clear(); state.drainQueue(); expect(backend.calls).toEqual([]);
  } finally { state.tasks.clear(); await state.close(); }
});

test("help lists registry row actions while the menu is open", async () => {
  const { state, key } = fixture();
  try {
    key("return"); key("down");
    const actions = state.groupedActions().flatMap(group => group.actions);
    expect(actions.map(action => action.id)).toEqual(expect.arrayContaining(["servers:select", "servers:single-speed", "servers:details"]));
    key("?"); expect(state.helpOpen).toBe(true); key("escape"); expect(state.serverMenu).toBe(true);
  } finally { await state.close(); }
});

test("unavailable menu action stays open and explains why", async () => {
  const { state, backend, key } = fixture();
  try { key("return"); state.catalogStale = true; key("return"); expect(state.serverMenu).toBe(true); expect(state.notice).toContain("Подписка изменена"); expect(backend.calls).toEqual([]); } finally { await state.close(); }
});

test("selection defaults to No and retains or rejects its captured identity", async () => {
  for (const change of ["cursor", "removed", "renamed"] as const) {
    const { state, backend, key } = fixture();
    try {
      key("return"); state.serverID = rows[1]!.server_id; key("return");
      expect(state.confirmation).toBe("select"); expect(state.selected).toBe(0); expect(backend.calls).toEqual([]);
      if (change === "removed") state.servers = [state.servers[1]!];
      if (change === "renamed") state.servers[0]!.display_name = "changed";
      key("down"); expect(state.selected).toBe(1); key("return");
      expect(backend.calls.filter(c => c.action === "servers/select").map(c => c.value)).toEqual(change === "cursor" ? [rows[0]!.server_id] : []);
      expect(state.queue).toHaveLength(0);
    } finally { await state.close(); }
  }
  const { state, backend, key } = fixture();
  try { key("return"); state.servers = [state.servers[1]!]; state.serverID = rows[1]!.server_id; key("return"); expect(backend.calls).toEqual([]); expect(state.notice).toContain("Сервер изменился"); } finally { await state.close(); }
});

test("confirmation defaults to the safe choice and horizontal arrows do not activate actions", async () => {
  for (const screen of ["confirm", "profile-remove", "retry-confirm", "cancel-confirm"] as const) {
    const { state, key } = fixture();
    try {
      state.open(screen, false, false); expect(state.items()[state.selected]?.key).toBe("n");
      key("tab"); key("left"); key("right"); expect(state.screen).toBe(screen); expect(state.items()[state.selected]?.key).toBe("y");
      key("up"); expect(state.items()[state.selected]?.key).toBe("n");
    } finally { await state.close(); }
  }
});


test("row check uses its captured server and records session history", async () => {
  const { state, backend, key } = fixture();
  try {
    key("return"); state.serverID = rows[1]!.server_id; key("down"); key("return");
    await settle();
    const calls = backend.calls.filter(call => call.action === "servers/check-batch");
    expect(calls).toHaveLength(1);
    expect(JSON.parse(calls[0]!.value!).ids).toEqual([rows[0]!.server_id]);
    expect(state.history.some(entry => entry.action === "servers/check-batch" && entry.count === 1)).toBe(true);
  } finally { await state.close(); }
});

test("row check queues behind a measurement with the captured server", async () => {
  const { state, backend, key } = fixture();
  let finish!: (result: ReturnType<typeof reply>) => void;
  backend.hold = { action: "servers/speed-batch", promise: new Promise(resolve => { finish = resolve; }) };
  try {
    const active = state.runBatch("speed"); await settle();
    key("return"); state.serverID = rows[1]!.server_id; key("down"); key("return");
    expect(state.queue).toHaveLength(1);
    expect(JSON.parse(state.queue[0]!.value!).ids).toEqual([rows[0]!.server_id]);
    expect(backend.calls.some(call => call.action === "servers/check-batch")).toBe(false);
    finish(reply()); await active; await settle();
    const call = backend.calls.find(item => item.action === "servers/check-batch");
    expect(JSON.parse(call!.value!).ids).toEqual([rows[0]!.server_id]);
    expect(state.history.some(entry => entry.action === "servers/check-batch" && entry.count === 1)).toBe(true);
  } finally { finish(reply()); await settle(); await state.close(); }
});
