import type { AppKeyEvent, AppState } from "./app-state";
import { actionRegistry, matchesAction } from "./action-registry";

export function dispatchKey(state: AppState, key: AppKeyEvent) {
  if (key.eventType === "release") return;
  const action = actionRegistry(state).find(candidate => matchesAction(candidate, key));
  if (!action) return;
  if (action.preventDefault || action.preventDefaultKeys?.includes(key.name)) key.preventDefault();
  if (!action.available) {
    if (action.unavailableBehavior === "notice" && action.unavailableReason) state.notify(action.unavailableReason, true);
    return;
  }
  action.run();
}
