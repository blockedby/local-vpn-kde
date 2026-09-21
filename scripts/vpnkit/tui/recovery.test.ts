import { expect, test } from "bun:test";
import { recoveryFor } from "./recovery";

test("recovery classifies real failure codes and gives bounded choices", () => {
  for (const [reason, stage] of Object.entries({
    "gateway-start-failed": "docker", "docker-unavailable": "docker",
    "route-conflict": "routes", "underlay-install-failed": "routes",
    "profile-import-failed": "profile", "profile-invalid": "profile",
    "dns-failed": "dns", stale: "server", recovery_required: "management",
    "invalid-request": "input",
  })) {
    const result = recoveryFor("start", reason);
    expect(result.stage).toBe(stage);
    expect(result.actions).toContain("diagnostics");
    expect(result.message.length).toBeGreaterThan(10);
  }
});

test("raw errors and prototype keys cannot leak into recovery text", () => {
  for (const reason of ["https://secret.example/token", "constructor", "__proto__"]) {
    const result = recoveryFor("start", reason);
    expect(result.stage).toBe("unknown");
    expect(typeof result.message).toBe("string");
    expect(result.message).not.toContain(reason);
    expect(result.actions).toEqual(["status", "diagnostics"]);
  }
});

test("retry is explicit and excludes ownership or recovery-required failures", () => {
  expect(recoveryFor("servers/speed", "failed").actions).toContain("retry");
  expect(recoveryFor("servers/speed", "failed").actions).toContain("servers");
  expect(recoveryFor("start", "dns-failed").actions).toContain("servers");
  expect(recoveryFor("servers/select", "stale").actions).not.toContain("retry");
  for (const reason of ["recovery_required", "foreign-profile", "profile-invalid"])
    expect(recoveryFor("start", reason).actions).not.toContain("retry");
});
