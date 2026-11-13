import type { Action } from "./bridge";

// The bridge uses a shared gate for cached reads, probes and selection. Selection
// additionally holds its mutation lock; exclusive actions hold the write gate.
// Two probes are deliberately serialized: their catalog publications race and
// simultaneous downloads distort measured speeds.
export const requestClasses: Record<Action, "read" | "measurement" | "selection" | "exclusive"> = {
  status: "read", "subscription/read": "read", "servers/list": "read",
  "servers/ping": "measurement", "servers/speed": "measurement",
  "servers/availability": "measurement", "servers/check-batch": "measurement", "servers/speed-batch": "measurement",
  "servers/select": "selection",
  "profiles/list": "exclusive", "profiles/add": "exclusive", "profiles/open": "exclusive", "profiles/remove": "exclusive",
  "autostart/read": "exclusive", "autostart/set": "exclusive", subscription: "exclusive",
  "backend/start": "exclusive", "network/reapply": "exclusive", disconnect: "exclusive", start: "exclusive", stop: "exclusive",
  "retest/select": "exclusive", "toggle-mode": "exclusive", diagnostics: "exclusive", "servers/refresh": "exclusive",
};

export const compatibilityRules = {
  read: { read: true, measurement: true, selection: true, exclusive: false },
  measurement: { read: true, measurement: false, selection: true, exclusive: false },
  selection: { read: true, measurement: true, selection: false, exclusive: false },
  exclusive: { read: false, measurement: false, selection: false, exclusive: false },
} as const;

export const compatibilityReasons = {
  read: "Cached reads share the bridge gate.",
  measurement: "Only one pass may publish catalog measurements at a time.",
  selection: "The bridge serializes selections with its mutation lock.",
  exclusive: "The bridge write gate excludes all other work.",
} as const;

export function compatible(action: Action, other: Action): boolean {
  return compatibilityRules[requestClasses[action]][requestClasses[other]];
}
