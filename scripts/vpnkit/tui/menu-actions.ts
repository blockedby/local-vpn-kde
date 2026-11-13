import type { Action } from "./bridge";
import type { AppState } from "./app-state";
import { frames } from "./animation-frames";
import { compatible, queueable } from "./request-policy";
import { cell } from "./formatting";

export interface MenuItem { key: string; label: string; run: () => void }
export type ActionScope = AppState["screen"] | "global" | "server-table" | "menu" | "editor" | "confirmation";
export interface ActionDefinition extends MenuItem {
  id: string;
  keys: readonly string[];
  scope: ActionScope;
  unavailableBehavior?: "notice" | "none";
  preventDefault?: boolean;
  preventDefaultKeys?: readonly string[];
  available: boolean;
  unavailableReason?: string;
}

export function menuItems(state: AppState): MenuItem[] {
    if (state.screen === "cancel-confirm" || state.screen === "retry-confirm") {
      const retry = state.screen === "retry-confirm";
      return [{ key: "n", label: "Нет", run: () => state.open(retry ? "recovery" : "tasks") },
        { key: "y", label: "Да", run: () => {
          if (retry) { const r = state.retryOperation; state.retryOperation = undefined; state.open(state.tab, false); if (r) void state.perform(r.action, r.value); }
          else { const id = state.cancelTaskID; state.open("tasks"); if (id) state.cancelTask(id); }
        } }];
    }
    if (state.screen === "tasks") {
      const running = [...state.tasks].filter(([, task]) => task.action !== "status");
      const columns = (status: string, action: string, progress: string, seconds: number) => {
        const width = Math.min(58, Math.max(42, state.viewport().width - 12));
        return `${cell(status, 17)} ${cell(action, Math.max(10, width - 35))} ${cell(progress, 9)} ${seconds} с`;
      };
      const taskLabel = (action: Action, count?: number) => count !== undefined && (action === "servers/speed-batch" || action === "servers/check-batch") ? `${action === "servers/speed-batch" ? "Скорость" : "Ping"} и сайт (${count})` : state.actionLabel(action);
      const entries = [
        ...running.map(([id, task]) => ({ id, action: task.action, label: columns(task.cancelRequested ? "Отмена запрошена" : "Выполняется", taskLabel(task.action, task.measurement ? state.batch?.total : undefined), state.batch && task.measurement ? `${state.batch.done}/${state.batch.total}` : task.phase || "—", Math.floor((Date.now() - task.started) / 1000)), direct: task.measurement || queueable.has(task.action) })),
        ...state.queue.map(entry => ({ id: entry.id, action: entry.action, label: columns("В очереди", taskLabel(entry.action, entry.kind === "batch" ? (entry.value ? (JSON.parse(entry.value) as {ids: string[]}).ids.length : state.servers.length) : undefined), "—", Math.floor((Date.now() - entry.started) / 1000)), direct: true })),
      ];
      return [
        ...entries.map((entry, i) => ({ key: String(i + 1), label: entry.label, run: () => { if (entry.direct) state.cancelTask(entry.id); else { state.cancelTaskID = entry.id; state.open("cancel-confirm"); } } })),
        ...state.history.map((entry, i) => ({ key: String(entries.length + i + 1), label: columns(entry.outcome, taskLabel(entry.action, entry.count), entry.phase || "—", Math.floor((Date.now() - entry.ended) / 1000)), run: () => {} })),
      ];
    }
    if (state.screen === "recovery") {
      if (state.backendOffline) return [{ key: "u", label: state.reconnecting ? "Восстанавливаем связь…" : "Восстановить связь", run: () => void state.reconnectBackend() }];
      return (state.recovery?.actions ?? ["status", "diagnostics"]).map(choice => ({
        key: { status: "u", diagnostics: "d", servers: "v", retry: "r", reapply: "f" }[choice],
        label: { status: "Проверить состояние", diagnostics: "Открыть диагностику", servers: "Выбрать другой сервер", retry: "Повторить действие", reapply: "Применить сетевые настройки VPN" }[choice],
        run: () => {
          if (choice === "reapply") void state.perform("network/reapply");
          else if (choice === "servers") state.open("servers");
          else if (choice === "diagnostics") state.open("diagnostics");
          else if (choice === "status") void state.perform("status");
          else if (state.recovery) {
            if (["servers/ping", "servers/speed", "servers/availability", "servers/check-batch", "servers/speed-batch", "servers/list", "status", "diagnostics"].includes(state.recovery.action))
              void state.retryCheck(state.recovery);
            else { state.retryOperation = { ...state.recovery }; state.open("retry-confirm"); }
          }
        },
      }));
    }
    if(state.screen==="profile-remove") return [
      {key:"y",label:"Удалить профиль из приложения",run:()=>void state.removeProfile()},
      {key:"n",label:"Отмена",run:()=>state.open("profiles")},
    ];
    if(state.screen==="profiles") {
      const profiles=state.profiles?.items ?? [];
      const pageSize=state.profilePageSize();
      const pages=Math.max(1,Math.ceil(profiles.length/pageSize));state.profilePage=Math.min(state.profilePage,pages-1);
      return [
        ...profiles.slice(state.profilePage*pageSize,state.profilePage*pageSize+pageSize).map((profile,i)=>({key:String(i+1),label:`${profile.id===state.profiles?.active?"● ":""}${profile.name} · ${profile.kind==="server"?"сервер":"подписка"} · серверов: ${profile.count} · ${profile.stale ? "сохранённый список" : "актуален"}`,run:()=>void state.browseProfile(profile.id)})),
        {key:"n",label:"Добавить профиль",run:()=>state.open("profile-name")},
        ...(state.profiles?.active?[{key:"X",label:"Удалить активный профиль",run:()=>state.open("profile-remove")}]:[]),
        ...(pages>1?[{key:"[",label:"Предыдущие профили",run:()=>{state.profilePage=(state.profilePage+pages-1)%pages;state.selected=0;state.paint()}},{key:"]",label:"Следующие профили",run:()=>{state.profilePage=(state.profilePage+1)%pages;state.selected=0;state.paint()}}]:[]),
      ];
    }
    if (state.screen === "confirm")
      return [
        {
          key: "n",
          label: "Нет",
          run: () => {
            if (state.confirmation === "fastest") state.fastestCandidate = undefined;
            if (state.confirmation === "subscription") {
              state.pendingSubscription = undefined;
              state.open("subscription");
            } else state.open(state.tab, false);
          },
        },
        {
          key: "y",
          label: "Да",
          run: () => {
            if (state.confirmation === "subscription") {
              const value = state.pendingSubscription;
              state.pendingSubscription = undefined;
              state.open("home");
              if (value) void state.perform("subscription", value);
              return;
            }
            if (state.confirmation === "fastest") {
              const candidate = state.fastestCandidate;
              state.fastestCandidate = undefined;
              state.open(state.tab, false);
              if (!candidate || !state.ready() || state.catalogStale || !state.servers.some(server => server.server_id === candidate.id)) state.notify("Сервер или состояние изменились. Выберите сервер заново.", true);
              else if (state.servers.find(server => server.server_id === candidate.id)?.selected) state.notify("Этот сервер уже выбран.");
              else void state.perform("servers/select", candidate.id);
              return;
            }
            const action = state.confirmation;
            state.open(state.tab, false);
            void state.perform(action);
          },
        },
      ];
    const run = (action: Action) => () => void state.perform(action);
    if (state.screen === "home")
      return [
        ...(!state.ready()
          ? [{ key: "s", label: `Запустить ${state.serviceObject()}`, run: run("backend/start") }]
          : []),
        ...(state.ready()
          ? [
              state.status?.networkmanager_active === "yes"
                ? {
                    key: "x",
                    label: "Отключить VPN",
                    run: () => state.confirm("disconnect"),
                  }
                : {
                    key: "s",
                    label: "Подключить VPN",
                    run: () => {
                      if (state.status?.subscription !== "configured") {
                        state.open("subscription");
                        state.notify("Добавьте подписку перед подключением.");
                      } else void state.perform("start");
                    },
                  },
            ]
          : []),
        { key: "t", label: state.nativePreviewOnly() ? "Тест скорости · недоступно в native preview" : state.inlineSpeed() ? `Тест скорости · ${state.homeTesting ? `${frames[state.frame % 10]} ${state.measurementAction === "servers/ping" ? "ping" : state.measurementAction === "servers/speed" ? "замер" : "подготовка"} · ` : ""}${state.homePing === undefined ? "—" : Math.round(state.homePing)}ms, ${state.homeSpeed === undefined ? "—" : state.homeSpeed.toFixed(1)} Мбит/с` : "Тест скорости", run: () => void state.testHomeSpeed() },
        { key: "v", label: state.nativePreviewOnly() ? "Серверы · native preview" : "Серверы", run: () => state.open("servers") },
        { key: "c", label: state.nativeInstalled() && state.status?.subscription==="configured" ? "Профили · подписки и серверы" : state.nativeMode() ? (state.nativeInstalled() ? "Подписка · настройка native-пакета" : "Подписка · недоступна в native preview") : "Подписка", run: () => state.open(state.nativeInstalled() && state.status?.subscription==="configured"?"profiles":"subscription") },
        { key: "d", label: state.nativePreviewOnly() ? "Диагностика · недоступна в native" : "Диагностика", run: () => state.open("diagnostics") },
        { key: "a", label: "Автозапуск", run: () => state.open("autostart") },
        {
          key: "m",
          label: `Режим: ${state.status?.routing_mode === "smart" ? "умный" : state.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}${state.nativePreviewOnly() ? " · недоступен в native" : ""}`,
          run: () => {
            if (state.nativeMode()) void state.perform("toggle-mode");
            else if (state.ready()) void state.perform("toggle-mode");
            else state.notify(`Смена режима доступна после запуска ${state.nativeMode() ? "сервиса" : "Docker"}.`);
          },
        },
        ...(["healthy", "unhealthy", "running", "stopped", "starting"].includes(
          state.status?.gateway_state ?? state.status?.vpn_state ?? "",
        )
          ? [
              {
                key: "z",
                label: state.nativeMode() ? "Остановить сервис" : "Остановить Docker и VPN",
                run: () => state.confirm("stop"),
              },
            ]
          : []),
      ];
    if (state.screen === "settings") return [
      { key: "a", label: `Автозапуск: ${state.autostart ? `шлюз ${state.autostart.gateway ? "да" : "нет"}, VPN ${state.autostart.connect ? "да" : "нет"}` : "не проверен"} · служба ${state.serviceState ? `${state.activeServiceLabel(state.serviceState.active)}/${({ enabled: "вкл", disabled: "выкл", masked: "маска" } as Record<string, string>)[state.serviceState.enabled] ?? "неизвестно"}` : "неизвестно"}`, run: () => state.open("autostart") },
      { key: "m", label: `Режим: ${state.status?.routing_mode === "smart" ? "умный" : state.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}`, run: () => void state.perform("toggle-mode") },
      { key: "c", label: "Профили и источники", run: () => state.open(state.nativeInstalled() && state.status?.subscription === "configured" ? "profiles" : "subscription") },
      { key: "e", label: `Сайт для проверки: ${state.target}`, run: () => state.open("target") },
      ...(!state.ready() ? [{ key: "s", label: `Запустить ${state.serviceObject()}`, run: run("backend/start") }] : []),
      ...(["healthy", "unhealthy", "running", "stopped", "starting"].includes(state.status?.gateway_state ?? state.status?.vpn_state ?? "") ? [{ key: "z", label: state.nativeMode() ? "Остановить сервис" : "Остановить Docker и VPN", run: () => state.confirm("stop") }] : []),
    ];
    if (state.screen === "autostart") {
      if (!state.autostart) return [{ key: "u", label: "Загрузить настройки", run: run("autostart/read") }];
      return [
        { key: "g", label: `${state.nativeMode() || state.autostart.runtime === "native" ? "Запускать native-службу" : "Запускать шлюз"}: ${state.autostart.gateway ? "да" : "нет"}`, run: () => void state.perform("autostart/set", state.autostartValue(!state.autostart!.gateway, false)) },
        { key: "a", label: `Подключать VPN: ${state.autostart.connect ? "да" : "нет"}`, run: () => void state.perform("autostart/set", state.autostartValue(state.autostart!.gateway || !state.autostart!.connect, !state.autostart!.connect)) },
      ];
    }
    if (state.screen === "diagnostics")
      return [
        { key: "u", label: state.nativePreviewOnly() ? "Обновить диагностику · недоступно в native" : "Обновить диагностику", run: run("diagnostics") },
      ];
    if (state.screen === "servers") {
      if (!state.ready())
        return [
          { key: "s", label: `Запустить ${state.serviceObject()}`, run: run("backend/start") },
        ];
      if (state.nativePreviewOnly())
        return [
          { key: "i", label: "Серверы и проверки · недоступны в native preview", run: () => state.notify(state.nativeUnavailable("servers/list"), true) },
        ];
      if (state.status?.subscription !== "configured")
        return [
          {
            key: "c",
            label: "Добавить подписку",
            run: () => state.open("subscription"),
          },
        ];
      return [
        {
          key: "p",
          label: "Ping всех + сайт",
          run: () => void state.runBatch("ping"),
        },
        {
          key: "t",
          label: state.filter ? `Скорость показанных (${state.filteredServers().length}) + сайт` : "Скорость + сайт · замер до 3 с",
          run: () => void state.runBatch("speed"),
        },
        { key: "e", label: "Адрес сайта", run: () => state.open("target") },
        {
          key: "r",
          label: "Обновить подписку и список",
          run: run("servers/refresh"),
        },
      ];
    }
    return [];
  }

/** Describe the same menu operations consumed by keyboard and mouse dispatch. */
export function defineMenuActions(state: AppState, items: MenuItem[]): ActionDefinition[] {
  return items.map(item => {
    const screen = state.screen;
    let id = `${screen}:${item.key}`;
    if (screen === "tasks") {
      const tasks = [...state.tasks].filter(([, task]) => task.action !== "status").map(([id]) => id).concat(state.queue.map(entry => entry.id));
      id = `task:cancel:${tasks[Number(item.key) - 1] ?? item.key}`;
    } else if (screen === "profiles" && /^[1-9]$/.test(item.key)) {
      id = `profile:open:${state.profiles?.items[state.profilePage * state.profilePageSize() + Number(item.key) - 1]?.id ?? item.key}`;
    } else if (screen === "home" && item.key === "s") {
      id = state.ready() ? "home:connect" : "home:start-service";
    }
    let reason: string | undefined;
    let unavailableBehavior: "notice" | "none" = "notice";
    if (screen === "tasks" && Number(item.key) > [...state.tasks.values()].filter(task => task.action !== "status").length + state.queue.length) {
      reason = "Операция завершена.";
      unavailableBehavior = "none";
    }
    const speed = screen === "home" && item.key === "t";
    const batch = screen === "servers" && (item.key === "p" || item.key === "t");
    if (screen === "recovery" && item.key === "u" && state.backendOffline && (state.reconnecting || state.tasks.size > 0 || !!state.batch || state.homeTesting || state.closing)) {
      reason = "Восстановление связи уже выполняется или другая операция ещё не завершена.";
      unavailableBehavior = "none";
    } else if (speed || batch) {
      if (state.nativePreviewOnly()) reason = state.nativeUnavailable(speed ? "servers/speed" : "servers/check-batch");
      else if (!state.ready()) reason = `Сначала запустите ${state.serviceObject()}.`;
      else if (batch && state.catalogLoaded && !state.servers.length) reason = "Список серверов пуст.";
    } else if (screen === "servers" && item.key === "i") {
      reason = state.nativeUnavailable("servers/list");
    } else if (screen === "home" && item.key === "m" && !state.nativeMode() && !state.ready()) {
      reason = "Смена режима доступна после запуска Docker.";
    } else {
      const request: Action | undefined = screen === "home" && item.key === "m" ? "toggle-mode"
        : screen === "home" && item.key === "s" && (!state.ready() || state.status?.subscription === "configured") ? state.ready() ? "start" : "backend/start"
        : screen === "autostart" && item.key === "u" ? "autostart/read"
        : screen === "autostart" && (item.key === "g" || item.key === "a") ? "autostart/set"
        : screen === "diagnostics" && item.key === "u" ? "diagnostics"
        : screen === "servers" && item.key === "r" ? "servers/refresh"
        : screen === "servers" && item.key === "s" ? "backend/start" : undefined;
      if (request) {
        if (state.backendOffline || state.reconnecting && !["status", "servers/list"].includes(request))
          reason = "Служба управления недоступна. [f] Восстановить связь";
        else if (state.nativePreviewOnly() && !["status", "backend/start", "network/reapply", "start", "disconnect", "stop", "autostart/read", "autostart/set"].includes(request))
          reason = state.nativeUnavailable(request);
        else if (!queueable.has(request) && (((!!state.batch || state.homeTesting) && !["servers/select", "status", "subscription/read"].includes(request)) ||
          [...state.tasks.values()].some(task => !compatible(request, task.action)))) {
          reason = "Эта операция конфликтует с текущей. Дождитесь завершения или отмените её [k].";
        }
      }
    }
    return { ...item, id, keys: [item.key], scope: screen, available: !reason, ...(reason ? { unavailableReason: reason, unavailableBehavior } : {}) };
  });
}
