import { expect, test } from "bun:test";
import { AppState } from "./app-state";
import { FakeBackend, initial } from "./app-test-fixtures";
import { menuItems } from "./menu-actions";

function native(profile?: string) {
  const state = new AppState(new FakeBackend());
  state.status = { ...initial, runtime: "native", installed: true, subscription: "configured", native_profile: profile };
  state.screen = "home";
  return state;
}

test("stale native profile detection", () => {
  expect(native("stale").nativeProfileStale()).toBe(true);
  expect(native("stale").opensProfiles()).toBe(false);
  expect(native().nativeProfileStale()).toBe(false);
  expect(native().opensProfiles()).toBe(true);
});

test("c opens the setup editor while stale and profiles otherwise", () => {
  for (const [profile, screen] of [["stale", "subscription"], [undefined, "profiles"]] as const) {
    const state = native(profile);
    const item = menuItems(state).find(entry => entry.key === "c");
    expect(item).toBeDefined();
    if (profile === "stale") expect(item!.label).toContain("повторить настройку native");
    item!.run();
    expect(state.screen).toBe(screen);
  }
});
