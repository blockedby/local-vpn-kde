import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { palette as p } from "./model";
import { safeText } from "./formatting";
import { detailsLines } from "./details-view";

function wrapDialogLine(input: string, width: number): string[] {
  const lines: string[] = [];
  let line = "";
  for (const word of safeText(input).split(/\s+/).filter(Boolean)) {
    if (line && Bun.stringWidth(`${line} ${word}`) <= width) { line += ` ${word}`; continue; }
    if (line) { lines.push(line); line = ""; }
    for (const { segment } of new Intl.Segmenter().segment(word)) {
      if (line && Bun.stringWidth(line + segment) > width) { lines.push(line); line = ""; }
      line += segment;
    }
  }
  if (line) lines.push(line);
  return lines;
}

export class SummaryView {
  private summary: TextRenderable;
  constructor(renderer: CliRenderer, parent: BoxRenderable) {
    this.summary = new TextRenderable(renderer, { content: "", fg: p.muted, height: 2, flexShrink: 0 });
    this.summary.minHeight = 0;
    parent.add(this.summary);
  }
  fit(width: number, offset = 0, limit = Infinity) {
    const lines = this.summary.chunks.map(chunk => chunk.text).join("").split("\n").flatMap(line => wrapDialogLine(line, width));
    this.summary.content = lines.slice(offset, offset + limit).join("\n");
    this.summary.height = Math.min(limit, Math.max(0, lines.length - offset));
    return lines.length;
  }
  hide() { this.summary.visible = false; this.summary.height = 0; }
  update(state: AppState) {
    const gateway = state.status?.gateway_state ?? state.status?.vpn_state;
    const gatewayNames: Record<string, string> = { healthy: "готов", absent: "не запущен", inactive: "не запущен", stopped: "остановлен", starting: "запускается", unhealthy: "ошибка", running: "проверяется" };
    this.summary.height = state.screen === "diagnostics" ? 6 : 2;
    if (state.screen === "details") this.summary.content = detailsLines(state).join("\n");
    if (state.screen === "tasks") this.summary.content = state.tasks.size || state.queue.length || state.history.length ? "Состояние        Операция               Этап      Время" : "Нет выполняющихся задач";
    if (state.screen === "cancel-confirm") this.summary.content = "Точно отменить эту операцию?";
    if (state.screen === "retry-confirm") this.summary.content = "Точно повторить изменение VPN?";
    if (state.screen === "recovery") {
      this.summary.height = 3;
      const stages: Record<string, string> = { docker: "Docker", routes: "Маршруты", profile: "Профиль KDE", dns: "DNS", server: "Сервер", management: "Служба управления", input: "Ввод данных", unknown: "Операция" };
      this.summary.content = `${stages[state.recovery?.stage ?? "unknown"]}\n${state.recovery?.message ?? "Проверьте состояние."}`;
    }
    if (state.screen === "confirm") {
      if (state.confirmation === "fastest" && state.fastestCandidate) {
        const candidate = state.fastestCandidate;
        const age = Math.max(0, Math.floor((Date.now() - candidate.measuredAt) / 1000));
        this.summary.content = `${candidate.name} · ${candidate.speed.toFixed(1)} Мбит/с · ${age} с назад${state.filter ? " · среди показанных" : ""}\nПереключение прервёт текущие соединения. Выбрать сервер?`;
      } else if (state.confirmation === "subscription") {
        this.summary.height = 4;
        this.summary.content =
          "Настроить native-пакет?\nСистемное окно Polkit попросит подтверждение администратора.\nПервичный выбор сервера может занять несколько минут.\nVPN не запускается; автозапуск не изменяется.";
      } else {
        this.summary.content =
          state.confirmation === "disconnect"
            ? "Точно хотите отключить VPN?"
            : `Точно хотите остановить ${state.serviceObject()} и отключить VPN?`;
      }
    }
    if (state.screen === "home")
      this.summary.content = !state.status
        ? "Проверяем состояние…"
        : state.nativePreviewOnly()
          ? state.nativePreview()
          : !state.ready()
            ? `Подписку можно настроить до запуска ${state.nativeMode() ? "сервиса" : "Docker"}.`
            : state.status.subscription !== "configured"
              ? "Добавьте подписку, затем откройте серверы."
              : "";
    this.summary.visible = state.screen !== "home" || this.summary.chunks.some(chunk => chunk.text.length > 0);
    if (!this.summary.visible) this.summary.height = 0;
    if (state.screen === "autostart") {
      const nativeAutostart = state.nativeMode() || state.autostart?.runtime === "native";
      const results: Record<string, string> = { ready: "выполнен", failed: "ошибка, откройте диагностику", "prerequisites-unavailable": `${nativeAutostart ? "Native-служба" : "Docker"} или сеть не готовы`, cancelled: "отменён", disabled: "отключён", running: "выполняется" };
      const login = nativeAutostart
        ? "Автозапуск native-службы при входе в графическую сессию"
        : "При входе в KDE";
      this.summary.height = nativeAutostart ? 5 : 4;
      this.summary.content = `${login}\nОтключение автозапуска не отключает работающий VPN.\nПоследний запуск: ${results[state.autostart?.last_result ?? ""] ?? "не выполнялся"}${nativeAutostart ? "\nРежим: native-служба" : ""}\n${state.serviceStateLabel().replace("; ", ";\n")}`;
    }
    if (state.screen === "subscription") {
      this.summary.height = state.nativeInstalled() ? 4 : state.nativeMode() ? 3 : 2;
      this.summary.content = state.nativeInstalled()
        ? "Первичная настройка native-пакета\nСистемное окно Polkit попросит подтверждение администратора.\nВыбор первого сервера может занять несколько минут.\nПосле успеха нажмите «Подключить VPN» отдельно; автозапуск включается отдельно и не включается сам."
        : state.nativeMode()
          ? "Подписка\nВ native preview чтение и применение подписки пока не подключены."
          : state.busy === "subscription/read"
            ? "Подписка · загрузка сохранённого адреса…"
            : "Подписка";
    }
    if(state.screen==="profiles") { this.summary.height = 1; this.summary.content="Выбор профиля не меняет активное VPN-подключение."; }
    if(state.screen==="profile-name") this.summary.content="Новый профиль · имя\nНапример: Моя VPS или Основная подписка";
    if(state.screen==="profile-source") this.summary.content=`${state.profileName} · ссылка\nHTTPS-подписка, vless://, hysteria2:// или hy2://`;
    if(state.screen==="profile-remove") this.summary.content="Удалить активный профиль?\nЭто не останавливает уже работающий сервер.";
    if (state.screen === "target")
      this.summary.content =
        "Адрес для проверки доступности\nHTTPS-запрос через каждый сервер; это не тест скорости.";
    if (state.screen === "diagnostics")
      this.summary.content = state.nativeMode()
        ? `OpenVPN / KDE: ${state.status?.networkmanager_active === "yes" ? "активен" : state.status?.networkmanager_active === "no" ? "отключён" : "неизвестно"}\nNative-служба: ${gatewayNames[gateway ?? ""] ?? "неизвестно"}\nПрофиль KDE: ${state.status?.networkmanager_configured === "yes" ? "настроен" : "не подтверждён"}\nПроверка: ${state.status?.diagnostics === "available" ? "выполнена" : "не запускалась"}\nПоследняя попытка: ${state.status?.last_attempt ?? "—"}`
        : `OpenVPN / KDE: ${state.status?.networkmanager_active === "yes" ? "активен" : state.status?.networkmanager_active === "no" ? "отключён" : "неизвестно"}\nDocker: ${gatewayNames[gateway ?? ""] ?? "неизвестно"}\nПрофиль KDE: ${state.status?.networkmanager_configured === "yes" ? "настроен" : "не подтверждён"}\nПроверка: ${state.status?.diagnostics === "available" ? "выполнена" : "не запускалась"}\nПоследняя попытка: ${state.status?.last_attempt ?? "—"}`;
    if (state.screen === "servers")
      this.summary.content = state.nativePreviewOnly()
        ? !state.ready()
          ? `Native-служба не готова. Запустите ${state.serviceObject()} для жизненного цикла VPN.\nВыбор серверов и проверки пока не подключены.`
          : "Native preview: выбор серверов и проверки пока не подключены.\nЗапуск/остановка VPN и автозапуск доступны с клавиатуры."
        : !state.ready()
          ? `Для проверок запустите ${state.serviceObject()}. VPN подключать не требуется.`
          : state.status?.subscription !== "configured"
            ? "Добавьте подписку для загрузки серверов."
            : state.catalogStale ? "Подписка изменена. Обновите список серверов [r]." : state.catalogLoaded ? "Список серверов пуст. Обновите источник [r] или откройте профили [c]." : "Загружаем серверы… Обновить список [r].";
    if (state.screen === "settings") { this.summary.content = ""; this.summary.visible = false; this.summary.height = 0; }
    if (state.screen === "servers" && state.ready() && !state.nativePreviewOnly() && state.status?.subscription === "configured" && state.servers.length) { this.summary.visible = false; this.summary.height = 0; }

  }
}
