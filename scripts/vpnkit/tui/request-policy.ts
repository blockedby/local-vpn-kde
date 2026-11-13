import type { Action } from "./bridge";

// The admission rule is independent of renderers and request execution.
export function compatible(action: Action, other: Action): boolean {
  const probe = (a: Action) => ["servers/ping", "servers/speed", "servers/availability", "servers/check-batch", "servers/speed-batch"].includes(a);
  const read = (a: Action) => ["status", "subscription/read"].includes(a);
  if ((action === "status" && other === "servers/list") || (other === "status" && action === "servers/list")) return true;
  return (read(action) && action !== other && (probe(other) || other === "servers/select")) ||
    (read(other) && action !== other && (probe(action) || action === "servers/select")) ||
    (action === "servers/select" && probe(other)) ||
    (other === "servers/select" && probe(action));
}
