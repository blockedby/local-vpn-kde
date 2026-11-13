import type { Action, Reply } from "./bridge";
import type { AppState } from "./app-state";
import { compatible } from "./request-policy";
import { recoveryFor } from "./recovery";
import { failure } from "./model";

export function updateCatalog(state: AppState, reply: Reply, action: Action) {
    if ((action === "servers/check-batch" || action === "servers/speed-batch") && reply.catalog?.servers) {
      for (const row of reply.catalog.servers) {
        if (action === "servers/speed-batch" && row.download_mbps !== undefined) state.measurementTimes.set(row.server_id, Date.now());
        const at = state.servers.findIndex((s) => s.server_id === row.server_id);
        if (at >= 0)
          state.servers[at] = {
            ...state.servers[at],
            ...(action === "servers/speed-batch" ? { status: row.status, download_mbps: row.download_mbps, download_seconds: row.download_seconds, downloaded_bytes: row.downloaded_bytes } : {}),
            ping_status: row.ping_status,
            latency_ms: row.latency_ms,
            availability:
              state.batch?.target === state.target
                ? row.availability
                : state.servers[at].availability,
          };
      }
    } else if (reply.catalog?.servers) {
      const previous = new Map(state.servers.map(s => [s.server_id, s]));
      const oldOrder = state.sortedServers();
      const anchor = oldOrder[state.viewStart]?.server_id;
      const cursorIndex = oldOrder.findIndex(s => s.server_id === state.serverID);
      state.servers = reply.catalog.servers.filter(s => !state.profiles || (s.profile_id ?? "profile_legacy1") === state.profiles.active).map(s => {
        const measured = previous.get(s.server_id);
        return { ...s, status: measured?.status ?? "untested",
          ping_status: measured?.ping_status ?? "untested", latency_ms: measured?.latency_ms,
          download_mbps: measured?.download_mbps, download_seconds: measured?.download_seconds,
          downloaded_bytes: measured?.downloaded_bytes, availability: measured?.availability ?? "untested" };
      });
      const order = state.sortedServers();
      if (!state.servers.some(s => s.server_id === state.serverID))
        state.serverID = order[Math.max(0, Math.min(cursorIndex, order.length - 1))]?.server_id;
      const anchorIndex = order.findIndex(s => s.server_id === anchor);
      if (anchorIndex >= 0) state.viewStart = anchorIndex;
      state.catalogLoaded = true;
      state.catalogStale = false;
    }
    const row = reply.catalog?.server;
    if (row) {
      const at = state.servers.findIndex((s) => s.server_id === row.server_id);
      if (at >= 0) {
        const previous = state.servers[at];
        // A ping/site reply also contains cached speed fields. Update only
        // the measurement requested, so old failures cannot reappear.
        if (action === "servers/speed")
          state.servers[at] = {
            ...previous,
            status: row.status,
            download_mbps: row.download_mbps,
            download_seconds: row.download_seconds,
            downloaded_bytes: row.downloaded_bytes,
          };
        else if (action === "servers/ping")
          state.servers[at] = {
            ...previous,
            ping_status: row.ping_status,
            latency_ms: row.latency_ms,
          };
        else if (
          action === "servers/availability" &&
          state.batch?.target === state.target
        )
          state.servers[at] = { ...previous, availability: row.availability };
      }
    }
    if (!state.servers.some((s) => s.server_id === state.serverID))
      state.serverID = state.sortedServers()[0]?.server_id;
    state.syncServerViewport();
  }
export async function perform(
    state: AppState,
    action: Action,
    value?: string,
    insideBatch = false,
  ): Promise<Reply | undefined> {
    if (state.closing || state.disposed) return;
    if (state.backendOffline || state.reconnecting && !["status", "servers/list"].includes(action)) {
      state.notify("Служба управления недоступна. [f] Восстановить связь", true); return;
    }
    if (!state.canPerform(action)) return;
    const measurementActive = !!state.batch || state.homeTesting;
    const allowedDuringMeasurement = ["servers/select", "status", "subscription/read"].includes(action);
    if ((measurementActive && !insideBatch && !allowedDuringMeasurement) ||
        [...state.tasks.values()].some(task => !compatible(action, task.action))) {
      if (state.busy === "status" && action !== "status" && !state.queued && !measurementActive) {
        state.queued = { action, value };
        return;
      }
      if (action !== "status") state.notify("Эта операция конфликтует с текущей. Дождитесь завершения или отмените её [k].");
      return;
    }
    if (action === "subscription" && (!value || !/^(https?:|vless:|hysteria2:|hy2:)\/\//.test(value))) {
      state.notify("Введите ссылку подписки.", true);
      return;
    }
    const requestID = `ui-${++state.nextTask}`;
    const requestRuntime = state.status?.runtime;
    const nativeSubscriptionSetup = action === "subscription" && state.nativeInstalled();
    state.tasks.set(requestID, { action, measurement: insideBatch, started: Date.now(), phase: "", value });
    const selectionRevision = state.selectionRevision;
    const revision = state.editorRevision,
      navigation = state.navigationRevision;
    state.paint();
    let reply: Reply | undefined;
    try {
      reply = await state.backend.request(action, value, requestID);
      if (!action.startsWith("servers/") && !action.startsWith("autostart/") && action !== "subscription/read" && !action.startsWith("profiles/")) {
        state.status = reply.status;
        state.checked = Date.now();
        clearTimeout(state.staleTimer);
        state.staleTimer = setTimeout(() => state.paintProgress(), 15001);
      }
      if (reply.ok && reply.autostart) state.autostart = reply.autostart;
      if (action === "autostart/read" || action === "autostart/set") {
        state.serviceState = reply.ok ? reply.autostart_service : undefined;
        if (!reply.ok && action === "autostart/set") state.autostart = undefined;
      }
      const previousMeasurements = state.homeTesting && action === "servers/list"
        ? new Map(state.servers.map(row => [row.server_id, row])) : undefined;
      const currentSelection = state.servers.find(row => row.selected)?.server_id;
      state.updateCatalog(reply, action);
      if (selectionRevision !== state.selectionRevision && reply.catalog?.servers)
        state.servers.forEach(row => row.selected = row.server_id === currentSelection);
      if (previousMeasurements) state.servers = state.servers.map(row => {
        const previous = previousMeasurements.get(row.server_id);
        return previous ? { ...row, status: previous.status, ping_status: previous.ping_status,
          latency_ms: previous.latency_ms, availability: previous.availability,
          download_mbps: previous.download_mbps, download_seconds: previous.download_seconds,
          downloaded_bytes: previous.downloaded_bytes } : row;
      });
      if (previousMeasurements) state.syncServerViewport();
      if(reply.ok && reply.profiles) state.profiles=reply.profiles;
      if (action === "subscription/read" && reply.ok) {
        state.loadedSubscription = true;
        if (revision === state.editorRevision) {
          state.draft = reply.value ?? "";
          if (state.screen === "subscription") state.editorValue = state.draft;
        }
      }
      if (!reply.ok && reply.reason === "cancelled") state.recovery = undefined;
      else if (!reply.ok) state.recovery = { ...recoveryFor(action, reply.reason, state.status?.runtime), action, value,
        homeServerID: state.homeTesting ? state.homeSpeedServer?.id : undefined,
        batchRetry: state.batch ? { kind: state.batch.kind, ids: [...(state.batch.ids ?? [])], target: state.batch.target ?? state.target } : undefined,
      };
      if (!reply.ok && !insideBatch) {
        state.noticeIdentity = { action, reason: reply.reason, ok: false };
        state.notice = nativeSubscriptionSetup
          ? "Не удалось настроить native-пакет. Проверьте подтверждение администратора и повторите."
          : reply.reason === "autostart-unavailable" ? "Не удалось настроить автозапуск. Проверьте пользовательскую службу systemd." : state.recovery?.message ?? failure(reply.reason, reply.code, state.status?.runtime);
        state.noticeAttempt = reply.reason === "native-network-settings-changed" ? "" : [
          "start",
          "backend/start",
          "stop",
          "disconnect",
          "toggle-mode",
          "diagnostics",
        ].includes(action)
          ? (reply.status.last_attempt ?? "")
          : "";
        state.error = true;
      } else if (
        reply.ok &&
        action !== "status" &&
        action !== "subscription/read" &&
        !insideBatch
      ) {
        if (state.recovery?.actions.includes("reapply") && ["network/reapply", "backend/start", "start", "disconnect", "stop"].includes(action)) state.recovery = undefined;
        const done: Partial<Record<Action, string>> = {
          "network/reapply": "Сетевые настройки VPN применены.",
          "profiles/list":"", "profiles/open":"Профиль открыт. Активный сервер не изменён.", "profiles/add":"Профиль добавлен.", "profiles/remove":"Профиль удалён. Активное подключение не изменено.",
          "autostart/read": "",
          "autostart/set": state.nativeMode() || state.autostart?.runtime === "native"
            ? "Сохранено. Настройка применяется при входе в графическую сессию."
            : "Сохранено. Настройка применяется при следующем входе в KDE.",
          "backend/start": state.nativeMode()
            ? "Native-служба готова. Доступны VPN и автозапуск; серверы пока не подключены."
            : "Docker готов. Можно проверять серверы.",
          start: "",
          disconnect: state.nativeMode() ? "VPN отключён. Native-служба работает." : "VPN отключён. Docker работает.",
          stop: state.nativeMode() ? "Native-служба и VPN остановлены." : "Docker и VPN остановлены.",
          "servers/list": "Список загружен.",
          "servers/refresh": "Каталог обновлён.",
          "servers/select": "Сервер выбран.",
          subscription: nativeSubscriptionSetup
            ? "Native-пакет настроен; первый сервер выбран, это могло занять несколько минут.\nVPN не подключён — нажмите «Подключить VPN» отдельно. Автозапуск не изменён — настройте его отдельно при желании."
            : "Подписка сохранена. Обновите список серверов.",
          diagnostics: "Состояние компонентов обновлено.",
          "toggle-mode": "Режим изменён.",
        };
        state.noticeIdentity = { action, reason: reply.reason, ok: true };
        state.notice = done[action] ?? "Готово.";
        state.noticeAttempt = "";
        state.error = false;
        if (action === "subscription") {
          state.catalogStale = true;
          state.catalogLoaded = false;
          state.servers = [];
          state.serverID = undefined;
          state.viewStart = 0;
          state.loadedSubscription = true;
          if (revision === state.editorRevision) {
            state.draft = value ?? "";
            if (navigation === state.navigationRevision) state.open("home");
          }
        }
        if (action === "servers/select" && value) {
          state.selectionRevision++;
          state.servers.forEach((s) => (s.selected = s.server_id === value));
        }
      }
    } catch {
      state.backendOffline = true;
      state.cancelled = true;
      state.queued = undefined;
      state.recovery = { ...recoveryFor(action, "backend-unavailable", state.status?.runtime), action, value };
      state.forgetStatusHealth();
      if (action === "autostart/read" || action === "autostart/set") {
        state.serviceState = undefined;
        if (action === "autostart/set") state.autostart = undefined;
      }
      state.notify(
        nativeSubscriptionSetup
          ? "Не удалось настроить native-пакет. Проверьте подтверждение администратора и повторите."
          : failure("backend-unavailable", null, requestRuntime),
        true,
      );
    } finally {
      state.tasks.delete(requestID);
      if (action === "servers/speed" && value) { state.liveSpeeds.delete(value); if (reply?.ok) state.measurementTimes.set(value, Date.now()); }
      state.paint();
      if (state.closing && !state.batch && !state.homeTesting && !state.tasks.size) await state.finishClose();
      else if (state.queued) {
        const queued = state.queued;
        state.queued = undefined;
        await state.perform(queued.action, queued.value);
      } else if (
        action === "backend/start" &&
        reply?.ok &&
        state.screen === "servers" &&
        state.status?.subscription === "configured" &&
        !state.catalogLoaded &&
        !state.batch &&
        !state.nativePreviewOnly()
      ) {
        await state.perform(
          state.catalogStale ? "servers/refresh" : "servers/list",
        );
      }
    }
    if(action==="servers/refresh" && reply?.ok && state.profiles){
      await state.perform("profiles/list");
      if(state.profiles.items.find(p=>p.id===state.profiles?.active)?.stale) state.notify("Источник недоступен. Показаны ранее сохранённые серверы.");
    }
    await state.loadPendingSubscription();
    await state.loadPendingScreenRead();
    return reply;
  }
