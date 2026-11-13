import type { Action } from "./bridge";
import type { AppState } from "./app-state";
import { names } from "./action-labels";
import { frames } from "./animation-frames";
import { compatible } from "./request-policy";

export interface MenuItem { key: string; label: string; run: () => void }
export interface ActionDefinition extends MenuItem {
  id: string;
  available: boolean;
  unavailableReason?: string;
}

export function menuItems(state: AppState): MenuItem[] {
    if (state.screen === "cancel-confirm" || state.screen === "retry-confirm") {
      const retry = state.screen === "retry-confirm";
      return [{ key: "n", label: "Нет", run: () => state.open(retry ? "recovery" : "tasks") },
        { key: "y", label: "Да", run: () => {
          if (retry) { const r = state.retryOperation; state.retryOperation = undefined; state.open("home"); if (r) void state.perform(r.action, r.value); }
          else { const id = state.cancelTaskID; state.open("tasks"); if (id) state.cancelTask(id); }
        } }];
    }
    if (state.screen === "tasks") return [...state.tasks].filter(([, task]) => task.action !== "status").map(([id, task], i) => ({
      key: String(i + 1), label: `Отменить: ${names[task.action] ?? (task.action === "servers/check-batch" ? "Ping → сайт" : "Операция")}`,
      run: () => {
        if (task.measurement || task.action.startsWith("servers/") && task.action !== "servers/select") state.cancelTask(id);
        else { state.cancelTaskID = id; state.open("cancel-confirm"); }
      },
    }));
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
      const pages=Math.max(1,Math.ceil(profiles.length/4));state.profilePage=Math.min(state.profilePage,pages-1);
      return [
        ...profiles.slice(state.profilePage*4,state.profilePage*4+4).map((profile,i)=>({key:String(i+1),label:`${profile.id===state.profiles?.active?"● ":""}${profile.name} · ${profile.kind==="server"?"сервер":"подписка"} · ${profile.count}${profile.stale?" · сохранённый список":""}`,run:()=>void state.browseProfile(profile.id)})),
        {key:"n",label:"Добавить профиль",run:()=>state.open("profile-name")},
        ...(state.profiles?.active?[{key:"x",label:"Удалить выбранный профиль",run:()=>state.open("profile-remove")}]:[]),
        ...(pages>1?[{key:"[",label:"Предыдущие профили",run:()=>{state.profilePage=(state.profilePage+pages-1)%pages;state.selected=0;state.paint()}},{key:"]",label:"Следующие профили",run:()=>{state.profilePage=(state.profilePage+1)%pages;state.selected=0;state.paint()}}]:[]),
      ];
    }
    if (state.screen === "confirm")
      return [
        {
          key: "n",
          label: "Нет",
          run: () => {
            if (state.confirmation === "subscription") {
              state.pendingSubscription = undefined;
              state.open("subscription");
            } else state.open("home");
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
            const action = state.confirmation;
            state.open("home");
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
          label: "Скорость + сайт · замер до 3 с",
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
      const tasks = [...state.tasks].filter(([, task]) => task.action !== "status");
      id = `task:cancel:${tasks[Number(item.key) - 1]?.[0] ?? item.key}`;
    } else if (screen === "profiles" && /^[1-4]$/.test(item.key)) {
      id = `profile:open:${state.profiles?.items[state.profilePage * 4 + Number(item.key) - 1]?.id ?? item.key}`;
    } else if (screen === "home" && item.key === "s") {
      id = state.ready() ? "home:connect" : "home:start-service";
    }
    let reason: string | undefined;
    const speed = screen === "home" && item.key === "t";
    const batch = screen === "servers" && (item.key === "p" || item.key === "t");
    if (speed || batch) {
      if (batch ? !!state.busy || !!state.batch || state.homeTesting : [...state.tasks.values()].some(task => task.action !== "status") || !!state.batch || state.homeTesting)
        reason = "Дождитесь завершения или отмените текущую операцию [k].";
      else if (state.nativePreviewOnly()) reason = state.nativeUnavailable(speed ? "servers/speed" : "servers/check-batch");
      else if (!state.ready()) reason = `Сначала запустите ${state.serviceObject()}.`;
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
        else if (((!!state.batch || state.homeTesting) && !["servers/select", "status", "subscription/read"].includes(request)) ||
          [...state.tasks.values()].some(task => !compatible(request, task.action))) {
          // A single action may be queued behind a status read.
          if (!(state.busy === "status" && !state.queued && !state.batch && !state.homeTesting))
            reason = "Эта операция конфликтует с текущей. Дождитесь завершения или отмените её [k].";
        }
      }
    }
    return { ...item, id, available: !reason, ...(reason ? { unavailableReason: reason } : {}) };
  });
}
