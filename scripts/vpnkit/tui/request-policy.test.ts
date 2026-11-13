import { expect, test } from "bun:test";
import { compatible, compatibilityReasons, requestClasses } from "./request-policy";
import type { Action } from "./bridge";

test("all action pairs follow the bridge gate and protect measurement publication", () => {
  const actions = Object.keys(requestClasses) as Action[];
  for (const action of actions) for (const other of actions) {
    const a = requestClasses[action], b = requestClasses[other];
    const expected = a !== "exclusive" && b !== "exclusive" &&
      !(a === "measurement" && b === "measurement") && !(a === "selection" && b === "selection");
    expect(compatible(action, other)).toBe(expected);
    expect(compatible(action, other)).toBe(compatible(other, action));
  }
  expect(Object.values(compatibilityReasons).every(Boolean)).toBe(true);
});
