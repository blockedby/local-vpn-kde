import type { Action } from "./bridge";
import { failure } from "./model";

export type RecoveryAction = "status" | "diagnostics" | "servers" | "retry";
export interface Recovery {
  stage: string;
  message: string;
  actions: RecoveryAction[];
}

// Only reason codes cross this boundary. Never display arbitrary backend output
// or offer automatic repair: recovery choices are explicit user actions.
const groups: Record<string, readonly string[]> = {
  docker: ["docker-unavailable", "docker-registry-failed", "gateway-build-failed", "gateway-start-failed", "gateway-unhealthy", "gateway-not-ready", "backend-outdated"],
  routes: ["route-conflict", "route-invalid", "ipv6-leak", "underlay-install-failed", "underlay-verify-failed", "underlay-not-ready"],
  profile: ["foreign-profile", "previous-profile-active", "profile-import-failed", "profile-invalid", "nm-activation-failed"],
  dns: ["dns-failed"],
  server: ["stale", "ip-https-failed", "hostname-https-failed", "ping-failed"],
  management: ["unavailable", "backend-unavailable", "diagnostics-unavailable", "recovery_required", "host-smoke-failed", "assets-failed"],
  input: ["invalid-request"],
};
const messages: Record<string, string> = {
  "docker-unavailable": "Docker недоступен. Проверьте его состояние и установку.",
  "underlay-install-failed": "Не удалось установить маршруты обхода VPN. Подробности — в диагностике.",
  "underlay-verify-failed": "Проверка маршрутов обхода VPN не прошла. Подробности — в диагностике.",
  "foreign-profile": "Обнаружен чужой профиль с таким же именем. Проверьте профили NetworkManager.",
  "previous-profile-active": "Предыдущий локальный профиль ещё подключён. Проверьте состояние VPN.",
  "profile-import-failed": "Не удалось импортировать профиль KDE. Подробности — в диагностике.",
  "profile-invalid": "Профиль KDE изменён или не принадлежит приложению. Нужна проверка в диагностике.",
  "assets-failed": "Не удалось подготовить ключи и конфигурацию. Подробности — в диагностике.",
  "invalid-request": "Проверьте введённые данные и повторите действие.",
};

export function recoveryFor(action: Action, reason: string): Recovery {
  let stage = Object.entries(groups).find(([, reasons]) => reasons.includes(reason))?.[0] ?? "unknown";
  const measurement = ["servers/ping", "servers/speed", "servers/availability", "servers/check-batch"].includes(action);
  if (stage === "unknown" && measurement && reason === "failed") stage = "server";
  const known = stage !== "unknown" || ["timeout", "cancelled"].includes(reason);
  const message = (Object.hasOwn(messages, reason) ? messages[reason] : undefined) ?? (measurement && reason === "failed"
    ? "Проверка сервера не прошла. Можно выбрать другой сервер или повторить проверку."
    : failure(known ? reason : "failed", null));
  const actions: RecoveryAction[] = ["status", "diagnostics"];
  if (stage === "server" || stage === "dns") actions.push("servers");
  // A generic retry must not bypass repair/ownership checks or invite a
  // repeated destructive action. Lifecycle repair stays in diagnostics.
  if (stage === "input" || (measurement && stage === "server" && reason !== "stale") || reason === "timeout") actions.push("retry");
  return { stage, message, actions };
}
