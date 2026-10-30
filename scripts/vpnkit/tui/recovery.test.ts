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

test("native failures retain typed recovery and batch speed retry scope", () => {
  const outdated = recoveryFor("servers/speed-batch", "backend-outdated", "native");
  expect(outdated.stage).toBe("management");
  expect(outdated.message).toContain("native");
  expect(outdated.message).not.toContain("Docker");
  const profile = recoveryFor("profiles/open", "profiles-failed", "native");
  expect(profile.message).toContain("Не удалось сохранить профиль");
  expect(profile.actions).not.toContain("retry");
  const speed = recoveryFor("servers/speed-batch", "failed", "native");
  expect(speed.stage).toBe("server");
  expect(speed.actions).toContain("retry");
});

test("only native network drift offers explicit network reapplication", () => {
  const drift = recoveryFor("backend/start", "native-network-settings-changed", "native");
  expect(drift.message).toBe("Сетевые настройки VPN поменялись, нажмите [f] чтобы применить их ещё раз.");
  expect(drift.actions).toEqual(["reapply"]);
  for (const reason of ["failed", "foreign-profile", "profile-invalid", "backend-unavailable"]) {
    expect(recoveryFor("start", reason, "native").actions).not.toContain("reapply");
  }
  expect(recoveryFor("start", "native-network-settings-changed", "docker").actions).not.toContain("reapply");
});
