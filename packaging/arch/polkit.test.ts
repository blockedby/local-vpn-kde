import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

const source = readFileSync(new URL("49-local-vpn-kde.rules", import.meta.url), "utf8");
let rule: (action: any, subject: any) => unknown;
let currentGroups = "";
let lookupFails = false;
new Function("polkit", source)({ spawn: (args: string[]) => {
  expect(args).toEqual(["/usr/bin/id", "-nG", "--", "fixture-user"]);
  if (lookupFails) throw new Error("lookup failed");
  return currentGroups;
}, Result: { YES: "yes" }, addRule: (fn: typeof rule) => { rule = fn; } });

function decision(id: string, unit: string, verb: string, enrolled = true) {
  currentGroups = enrolled ? "users local-vpn-kde-users\n" : "users\n";
  return rule({ id, lookup: (key: string) => ({ unit, verb })[key] }, {
    user: "fixture-user",
    active: false, // user-manager jobs need not belong to a logind session
    isInGroup: () => false, // stale session must not require another login
  });
}

test("only enrolled users may start/stop the two fixed units", () => {
  for (const unit of ["local-vpn-kde-network.service", "local-vpn-kde-gateway.service"]) {
    for (const verb of ["start", "stop"]) {
      expect(decision("org.freedesktop.systemd1.manage-units", unit, verb)).toBe("yes");
      expect(decision("org.freedesktop.systemd1.manage-units", unit, verb, false)).toBeUndefined();
    }
    for (const verb of ["restart", "reload", "enable", "disable", "kill", ""]) {
      expect(decision("org.freedesktop.systemd1.manage-units", unit, verb)).toBeUndefined();
    }
  }
  for (const unit of ["sshd.service", "local-vpn-kde-network@root.service", "local-vpn-kde-network.service/other", ""]) {
    expect(decision("org.freedesktop.systemd1.manage-units", unit, "start")).toBeUndefined();
  }
  expect(decision("org.freedesktop.systemd1.manage-unit-files", "local-vpn-kde-network.service", "start")).toBeUndefined();
});

test("native operations require explicit enrollment", () => {
  expect(decision("org.local-vpn-kde.operation", "", "")).toBe("yes");
  expect(decision("org.local-vpn-kde.operation", "", "", false)).toBeUndefined();
  expect(decision("org.local-vpn-kde.configure", "", "")).toBe("yes");
  expect(decision("org.local-vpn-kde.configure", "", "", false)).toBeUndefined();
});

test("lookup errors and similarly named groups do not authorize", () => {
  lookupFails = true;
  expect(decision("org.local-vpn-kde.operation", "", "")).toBeUndefined();
  lookupFails = false;
  currentGroups = "local-vpn-kde-users-other";
  expect(rule({id:"org.local-vpn-kde.operation"}, {user:"fixture-user"})).toBeUndefined();
});
