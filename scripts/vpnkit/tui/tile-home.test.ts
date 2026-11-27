import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { FakeBackend, initial } from "./app-test-fixtures";
const key = (state: AppState, name: string) => state.onKey({ name, preventDefault() {} });

for (const [width, height, steps] of [
  [100, 24, [["left",0],["up",0],["right",1],["right",2],["right",2],["down",2],["left",1],["down",4],["right",4],["up",1],["home",0],["end",4]]] as const,
  [60, 24, [["left",0],["up",0],["right",1],["right",1],["down",3],["down",3],["left",2],["down",4],["right",4],["up",2],["home",0],["end",4]]] as const,
  [50, 30, [["left",0],["up",0],["right",0],["down",1],["down",2],["right",2],["end",4],["down",4],["up",3],["home",0],["end",4]]] as const,
] as const) test(`home navigation clamps in two dimensions at ${width}x${height}`, async () => {
  const state = new AppState(new FakeBackend());
  state.viewport = () => ({ width, height, terminalHeight: height });
  try { expect(state.screen).toBe("home"); for (const [name, index] of steps) { key(state, name); expect(state.homeIndex).toBe(index); } key(state, "escape"); expect(state.screen).toBe("home"); expect(state.homeIndex).toBe(4); }
  finally { await state.close(); }
});

test("each section tile opens its section and returns to the same tile", async () => {
  const state = new AppState(new FakeBackend()); state.status = { ...initial };
  try {
    for (const [index, screen] of [[1,"servers"],[2,"profiles"],[3,"diagnostics"],[4,"settings"]] as const) {
      state.homeIndex = index; key(state, "return"); expect(state.screen).toBe(screen); key(state, "escape"); expect(state.screen).toBe("home"); expect(state.homeIndex).toBe(index);
    }
  } finally { await state.close(); }
});

test("connection tile follows the letter key confirmation and never queues a mutation", async () => {
  for (const trigger of ["return", "x"]) {
    const backend = new FakeBackend(); const state = new AppState(backend); state.status = { ...initial, networkmanager_active: "yes" };
    try {
      key(state, trigger); expect(state.screen).toBe("confirm"); expect(state.confirmation).toBe("disconnect"); expect(state.selected).toBe(0); expect(backend.calls).toEqual([]);
      key(state, "return"); expect(state.screen).toBe("home"); expect(backend.calls).toEqual([]); expect(state.queue).toHaveLength(0);
      key(state, trigger); key(state, "y"); expect(backend.calls.map(call => call.action)).toEqual(["disconnect"]); expect(state.queue).toHaveLength(0);
    } finally { await state.close(); }
  }
  const backend = new FakeBackend(), state = new AppState(backend); state.status = { ...initial };
  try { key(state, "return"); expect(backend.calls[0]?.action).toBe("start"); expect(state.queue).toHaveLength(0); } finally { await state.close(); }
});

test("editors and filters retain escape precedence over returning home", async () => {
  const state = new AppState(new FakeBackend()); state.status = { ...initial };
  try {
    state.open("servers", false); state.open("subscription", false); key(state, "escape"); expect(state.screen).toBe("servers");
    state.filterEditing = true; state.filter = "example"; key(state, "escape"); expect(state.screen).toBe("servers"); expect(state.filter).toBe("");
    key(state, "escape"); expect(state.screen).toBe("home"); expect(state.homeIndex).toBe(1);
  } finally { await state.close(); }
});

test("connection tile refuses conflicting work without queueing or replaying it", async () => {
  const backend = new FakeBackend(), state = new AppState(backend); state.status = { ...initial };
  try {
    state.tasks.set("busy", { action: "start", measurement: false, started: 0, phase: "" });
    key(state, "return"); expect(backend.calls).toEqual([]); expect(state.queue).toHaveLength(0);
    state.tasks.clear(); state.drainQueue(); expect(backend.calls).toEqual([]);
  } finally { await state.close(); }
});
