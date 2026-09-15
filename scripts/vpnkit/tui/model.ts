import type { Status } from "./bridge";

export const palette = {
  base: "#0b1120",
  panel: "#111c30",
  edge: "#273953",
  text: "#e5edf8",
  muted: "#a0afc5",
  accent: "#70d8eb",
  green: "#87e6b0",
  amber: "#f3c880",
  red: "#ff9eab",
};

export function connection(status?: Status) {
  const gateway = status?.gateway_state ?? status?.vpn_state;
  if (!status || gateway === "unknown")
    return { title: "Статус недоступен", detail: "", color: palette.amber };
  if (gateway === "unhealthy")
    return {
      title: "Нужна проверка",
      detail: "Docker сообщает об ошибке",
      color: palette.red,
    };
  if (status.networkmanager_active === "yes" && gateway === "healthy")
    return { title: "VPN подключён", detail: "", color: palette.green };
  if (status.networkmanager_active === "yes")
    return {
      title: "VPN без готового Docker",
      detail: "",
      color: palette.amber,
    };
  if (status.networkmanager_active === "unknown")
    return {
      title: "VPN: статус неизвестен",
      detail: "",
      color: palette.amber,
    };
  return { title: "VPN отключён", detail: "", color: palette.muted };
}

export function failure(reason: string, code: number | null) {
  const explanations: Record<string, string> = {
    "backend-outdated":
      "Нужен обновлённый Docker-бэкенд. Остановите его и запустите заново.",
    stale: "Каталог изменился. Обновите список серверов.",
    recovery_required:
      "Предыдущая операция не завершила откат. Запуск заблокирован; нужна проверка журнала восстановления в диагностике.",
    "diagnostics-unavailable":
      "Не удалось создать журнал. Проверьте права на приватный каталог; действие не запускалось.",
    "route-conflict":
      "Маршрут идёт мимо локального VPN. Возможен конфликт с другим VPN; подробности записаны в журнал.",
    "route-invalid":
      "Не удалось разобрать маршрут VPN. Данные маршрутизации сохранены в журнал.",
    "dns-failed":
      "Проверка DNS после подключения не прошла. Подробности сохранены в журнал.",
    "ip-https-failed":
      "Не прошла проверка HTTPS по IP через VPN. Подробности сохранены в журнал.",
    "hostname-https-failed":
      "Не прошла проверка HTTPS по имени через VPN. Подробности сохранены в журнал.",
    "ping-failed":
      "Не прошла проверка ping через VPN. Подробности сохранены в журнал.",
    "ipv6-leak":
      "Проверка обнаружила доступный IPv6 в обход VPN. Подключение откатывается.",
    "host-smoke-failed":
      "Подключение не прошло проверку готовности. Причина и результат отката сохранены в журнал.",
    "nm-activation-failed":
      "KDE не смог подключить профиль OpenVPN. Журнал NetworkManager сохранён для разбора.",
    "docker-registry-failed":
      "Docker не смог загрузить сведения о базовом образе. Подробности — в приватном журнале.",
    "gateway-build-failed":
      "Не удалось собрать Docker-образ. Подробности — в приватном журнале.",
    "gateway-start-failed":
      "Не удалось запустить Docker-шлюз. Вывод запуска сохранён в журнал.",
    "gateway-unhealthy":
      "Шлюз не достиг готовности. Состояние и журнал контейнера сохранены до отката.",
    "underlay-not-ready":
      "Не прошла проверка обхода VPN для Docker. Подробности сохранены в журнал.",
  };
  if (explanations[reason]) return explanations[reason];
  if (reason === "gateway-not-ready")
    return "Не удалось подготовить Docker-бэкенд для проверки серверов. Откройте диагностику [d].";
  if (reason === "timeout")
    return "Время ожидания истекло. Проверьте состояние и повторите действие.";
  if (reason === "unavailable" || reason === "backend-unavailable")
    return "Служба управления недоступна. Проверьте установку и перезапустите интерфейс.";
  if (reason === "invalid-request")
    return "Не удалось сохранить подписку. Проверьте ссылку и права на приватный каталог.";
  if (reason === "cancelled")
    return "Действие отменено. Проверьте текущее состояние подключения.";
  return `Действие не выполнено${code === null ? "" : ` (код ${code})`}. Проверьте Docker, профиль KDE и диагностику [d].`;
}
