import { expect, test } from "bun:test";
import { compatible } from "./request-policy";

test("status and measurements may overlap without admitting duplicate status reads", () => {
  expect(compatible("status", "servers/list")).toBe(true);
  expect(compatible("servers/list", "status")).toBe(true);
  expect(compatible("status", "servers/speed")).toBe(true);
  expect(compatible("status", "status")).toBe(false);
});

test("mutations conflict while selection and measurement may overlap", () => {
  expect(compatible("start", "servers/speed")).toBe(false);
  expect(compatible("servers/select", "servers/speed")).toBe(true);
  expect(compatible("servers/speed", "servers/select")).toBe(true);
});
