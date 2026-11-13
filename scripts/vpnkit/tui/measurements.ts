import type { Batch, AppState } from "./app-state";
import { failure } from "./model";

export async function testHomeSpeed(state: AppState, sourceID?: string) {
    if ([...state.tasks.values()].some(task => task.action !== "status") || state.batch || state.homeTesting) {
      state.notify("Дождитесь завершения или отмените текущую операцию [k].");
      return;
    }
    if (state.nativePreviewOnly()) { state.notify(state.nativeUnavailable("servers/speed"), true); return; }
    if (!state.ready()) { state.notify(`Сначала запустите ${state.serviceObject()}.`, true); return; }
    state.homeTesting = true;
    state.cancelled = false;
    state.homeSpeed = undefined;
    state.homePing = undefined;
    state.homeSpeedAt = 0;
    state.homeSpeedServer = undefined;
    state.notice = "";
    state.noticeAttempt = "";
    state.error = false;
    state.homeSpeedLabel = "Определяем выбранный сервер…";
    try {
      const list = await state.perform("servers/list", undefined, true);
      if (state.cancelled || state.closing) return;
      if (!list?.ok) throw new Error("Не удалось загрузить серверы.");
      const server = list.catalog?.servers?.find(row => sourceID ? row.server_id === sourceID : row.selected);
      if (!server) throw new Error(sourceID ? "Сервер удалён из подписки. Выберите другой сервер." : "Сначала выберите сервер в списке.");
      state.homeSpeedServer = { id: server.server_id, name: server.display_name };
      state.homeSpeedLabel = `Ping · ${server.display_name}`;
      const ping = await state.perform("servers/ping", server.server_id, true);
      if (state.cancelled || state.closing) return;
      if (!ping?.ok || ping.catalog?.server?.ping_status !== "ready") throw new Error("Сервер недоступен: ping не прошёл.");
      state.homePing = ping.catalog?.server?.latency_ms;
      state.homeSpeedLabel = `Измеряем · ${server.display_name} · 3 с`;
      const result = await state.perform("servers/speed", server.server_id, true);
      if (state.cancelled || state.closing) return;
      const mbps = result?.catalog?.server?.download_mbps;
      if (!result?.ok || mbps === undefined || !Number.isFinite(mbps) || mbps < 0) throw new Error("Не удалось измерить скорость.");
      state.homeSpeed = mbps;
      state.homeSpeedAt = Date.now();
      state.homeSpeedLabel = `${server.display_name} · средняя скорость`;
    } catch (error) {
      state.homeSpeed = undefined;
      state.homeSpeedLabel = error instanceof Error ? error.message : "Ошибка измерения.";
      state.notify(state.homeSpeedLabel, true);
    } finally {
      if (state.cancelled) { state.homeSpeedLabel = "Тест отменён"; state.homeSpeed = undefined; }
      state.homeTesting = false;
      state.paint();
      if (state.closing && !state.tasks.size) await state.finishClose();
      else { await state.loadPendingSubscription(); await state.loadPendingScreenRead(); }
    }
  }

export async function runBatch(state: AppState, kind: Batch, retry?: { ids: string[]; target: string }) {
    if (state.busy || state.batch || state.homeTesting) {
      state.notify("Дождитесь завершения или отмените текущую операцию [k].");
      return;
    }
    if (state.nativePreviewOnly()) {
      state.notify(state.nativeUnavailable("servers/check-batch"), true);
      return;
    }
    if (!state.ready()) {
      state.notify(`Сначала запустите ${state.serviceObject()}.`, true);
      return;
    }
    state.cancelled = false;
    state.notice = "";
    state.noticeAttempt = "";
    state.error = false;
    // One measurement pass at a time; server selection has an independent task.
    state.started = Date.now();
    state.batch = {
      kind,
      done: 0,
      total: state.servers.length,
      failed: 0,
      target: retry?.target ?? state.target,
      running: new Set(), stages: new Map(),
      pinged: new Set(), completed: new Set(), failures: new Set(),
    };
    if (!state.catalogLoaded || state.catalogStale) {
      const result = await state.perform(
        state.catalogStale ? "servers/refresh" : "servers/list",
        undefined,
        true,
      );
      if (!result?.ok) {
        state.batch = undefined;
        state.notify(
          result?.reason === "backend-outdated"
            ? failure("backend-outdated", null, state.status?.runtime)
            : `Не удалось загрузить серверы. Проверьте подписку и ${state.serviceLabel()}.`,
          true,
        );
        if (state.closing && !state.tasks.size && !state.homeTesting && !state.batch) await state.finishClose();
        else await state.loadPendingScreenRead();
        return;
      }
    }
    const ids = retry?.ids ?? state.sortedServers().map((s) => s.server_id);
    if (ids.some(id => !state.servers.some(s => s.server_id === id))) {
      state.batch = undefined;
      state.notify("Сервер удалён из подписки. Обновите список перед проверкой.", true);
      await state.loadPendingScreenRead();
      return;
    }
    state.frozenOrder = ids;
    state.batch.total = ids.length;
    const target = state.batch.target!;
    let stoppedReason: string | undefined;
    if (!state.cancelled && !state.closing && ids.length) {
      state.batch.ids = ids;
      const result = await state.perform(kind === "speed" ? "servers/speed-batch" : "servers/check-batch", JSON.stringify({ ids, url: target }), true);
      if (!state.cancelled && !state.closing) {
        for (const row of result?.catalog?.servers ?? []) {
          state.batch.completed.add(row.server_id);
          if (row.ping_status !== "ready" || row.availability !== "ready" || (kind === "speed" && row.status !== "ready")) state.batch.failures.add(row.server_id);
        }
        state.batch.done = state.batch.completed.size;
        state.batch.failed = state.batch.failures.size;
        if (!result?.ok) { stoppedReason = result?.reason ?? "unavailable"; state.cancelled = true; }
      }
    }
    const { done, total, failed } = state.batch;
    state.frozenOrder = undefined;
    state.batch = undefined;
    state.notify(
      stoppedReason
        ? `${failure(stoppedReason, null, state.status?.runtime)} Проверено: ${done}/${total}.`
        : `${state.cancelled || state.closing ? "Остановлено" : "Завершено"}: ${done}/${total} · ошибок ${failed}. ${(state.cancelled || state.closing) ? (kind === "speed" ? "Завершённые результаты сохранены." : "Показаны полученные результаты; проход не сохранён.") : "Результаты сохранены."}`,
      failed > 0 || !!stoppedReason,
    );
    if (state.closing && !state.tasks.size && !state.homeTesting && !state.batch) await state.finishClose();
    else { await state.loadPendingSubscription(); await state.loadPendingScreenRead(); }
  }
export function cancelTask(state: AppState, id: string) {
    const task = state.tasks.get(id);
    if (!task) return;
    if (task.measurement) state.cancelled = true;
    state.backend.cancel?.(id);
    state.notify("Отмена запрошена. Ожидаем завершения и восстановления состояния.");
  }
export function cancel(state: AppState) {
    const entries = [...state.tasks.entries()].filter(([, task]) => !["status", "subscription", "subscription/read"].includes(task.action));
    if (entries.length !== 1) { if (entries.length) state.open("tasks"); return; }
    const [id, task] = entries[0]!;
    if (task.measurement || task.action.startsWith("servers/") && task.action !== "servers/select") state.cancelTask(id);
    else { state.cancelTaskID = id; state.open("cancel-confirm"); }
  }
export async function retryCheck(state: AppState, recovery: NonNullable<AppState["recovery"]>) {
    if (recovery.homeServerID) {
      await state.testHomeSpeed(recovery.homeServerID);
      return;
    }
    if (recovery.batchRetry) {
      await state.runBatch(recovery.batchRetry.kind, recovery.batchRetry);
      return;
    }
    const { action, value } = recovery;
    if (action === "servers/speed") {
      const ping = await state.perform("servers/ping", value);
      if (!ping?.ok || ping.catalog?.server?.ping_status !== "ready") return;
    }
    await state.perform(action, value);
  }
export async function reconnectBackend(state: AppState) {
    if (state.reconnecting || state.tasks.size || state.batch || state.homeTesting || state.closing) return;
    if (!state.backend.reconnect) { state.notify("Перезапустите интерфейс: служба не поддерживает восстановление.", true); return; }
    state.reconnecting = true;
    state.paint();
    try {
      await state.backend.reconnect();
      if (state.closing || state.disposed) return;
      state.backendOffline = false;
      const status = await state.perform("status");
      if (!status?.ok) throw new Error();
      if (state.ready() && state.status?.subscription === "configured") {
        const list = await state.perform("servers/list");
        if (!list?.ok) throw new Error();
      }
      state.notify("Связь восстановлена. Состояние обновлено; действия не повторялись.");
    } catch { state.backendOffline = true; state.notify("Не удалось восстановить связь. Можно повторить [u].", true); }
    finally {
      state.reconnecting = false;
      state.paint();
      if (!state.backendOffline) await state.loadPendingScreenRead();
    }
  }
