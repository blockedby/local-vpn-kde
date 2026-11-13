import { isOverlay, type AppKeyEvent, type AppState, type Screen } from "./app-state";
import { compatible } from "./request-policy";
import type { ActionDefinition, ActionScope } from "./menu-actions";

/** Keys use OpenTUI names; modified keys use ctrl+ / shift+ prefixes. */
export function actionRegistry(state: AppState): ActionDefinition[] {
  const editor = ["subscription", "target", "profile-name", "profile-source"].includes(state.screen);
  const actions: ActionDefinition[] = [];
  const add = (id: string, keys: string[], label: string, scope: ActionScope, run: () => void,
    reason?: string, unavailableBehavior: "notice" | "none" = "none", preventDefault = false) => {
    actions.push({ id, key: keys[0]!, keys, label, scope, run, available: !reason,
      ...(reason ? { unavailableReason: reason, unavailableBehavior } : {}), preventDefault });
  };
  if (state.helpOpen) {
    add("help:close", ["escape", "?"], "Закрыть помощь", "global", () => { state.helpOpen = false; state.paint(); }, undefined, "none", true);
    add("help:up", ["up", "pageup"], "Вверх", "global", () => { state.helpOffset = Math.max(0, state.helpOffset - 1); state.paint(); }, state.helpOffset === 0 ? "Начало списка." : undefined);
    add("help:down", ["down", "pagedown"], "Вниз", "global", () => { state.helpOffset++; state.paint(); });
    return actions;
  }
  if (!editor) add("global:help", ["?"], "Помощь", "global", () => { state.helpOffset = 0; state.helpOpen = true; state.paint(); }, undefined, "none", true);
  add("global:back", ["escape"], "Назад", editor ? "editor" : "global", () => state.open(
    state.screen.startsWith("profile-") ? "profiles"
      : state.screen === "target" ? state.tab
      : state.screen === "confirm" && state.confirmation === "subscription" ? "subscription"
      : state.screen === "cancel-confirm" ? "tasks"
      : state.screen === "retry-confirm" ? "recovery"
      : ["autostart", "details", "confirm", "tasks", "recovery"].includes(state.screen) ? state.tab : "servers", state.screen !== "confirm"), state.closing ? "Закрываем приложение." : undefined, "none", true);
  add("global:quit-control", ["ctrl+c"], "Выход", "global", () => void state.close(), state.closing || state.disposed ? "Закрываем приложение." : undefined, "none", true);
  if (editor) {
    add("editor:submit", ["return"], "Сохранить", "editor", () => {
      if (state.screen === "profile-name") {
        const value = state.editorValue.trim();
        if (!value || [...value].length > 60) { state.notify("Введите имя профиля: до 60 символов.", true); return; }
        state.profileName = value; state.open("profile-source"); return;
      }
      if (state.screen === "profile-source") { void state.addProfile(state.editorValue.trim()); return; }
      if (state.screen === "subscription") {
        const value = state.editorValue.trim();
        if (state.nativeInstalled()) {
          if (!value || !/^(https?:|vless:|hysteria2:|hy2:)\/\//.test(value)) { state.notify("Введите ссылку подписки.", true); return; }
          state.confirmSubscription(value);
        } else void state.perform("subscription", value);
      } else {
        try {
          const u = new URL(state.editorValue.trim());
          if (u.protocol !== "https:" || u.username || u.password || u.href.length > 2048) throw new Error();
          state.target = u.href; state.servers.forEach(s => s.availability = "untested"); state.syncServerViewport(); state.open(state.tab); state.notify("Адрес сайта сохранён.");
        } catch { state.notify("Нужен HTTPS-адрес без логина и пароля.", true); }
      }
    }, undefined, "none", true);
    return actions;
  }
  if (isOverlay(state.screen)) {
    if (state.screen === "details") {
      add("details:up", ["up", "pageup"], "Прокрутить вверх", "menu", () => { state.detailsOffset = Math.max(0, state.detailsOffset - 1); state.paint(); });
      add("details:down", ["down", "pagedown"], "Прокрутить вниз", "menu", () => { state.detailsOffset++; state.paint(); });
      return actions;
    }
    const items = state.items();
    const move = (delta: number) => { state.selected = (state.selected + items.length + delta) % Math.max(1, items.length); state.paint(); };
    add("menu:previous", ["up"], "Выбор", "menu", () => move(-1), items.length <= 1 ? "Нет другого пункта." : undefined);
    add("menu:next", ["down"], "Выбор", "menu", () => move(1), items.length <= 1 ? "Нет другого пункта." : undefined);
    const current = items[state.selected];
    add("menu:select", ["return"], "Выбрать", "menu", () => { current?.run(); state.paint(); }, current?.unavailableReason ?? (!current ? "Нет выбранного пункта." : undefined), current?.unavailableBehavior ?? "none");
    for (const item of items) actions.push({ ...item, preventDefault: true, run: () => { item.run(); state.paint(); } });
    return actions;
  }
  add("global:quit", ["q"], "Выход", "global", () => void state.close(), state.closing || state.disposed ? "Закрываем приложение." : undefined);
  if (state.closing) return actions;
  add("global:tasks", ["j"], "Задачи", "global", () => state.open("tasks"));
  if (state.recovery) {
    const reapply = state.recovery.actions.includes("reapply");
    const conflict = [...state.tasks.values()].some(task => !compatible("network/reapply", task.action));
    const reason = !reapply ? undefined : state.backendOffline || state.reconnecting ? "Служба управления недоступна. [f] Восстановить связь"
      : (state.batch || state.homeTesting || conflict) ? "Эта операция конфликтует с текущей. Дождитесь завершения или отмените её [k]." : undefined;
    add("global:recovery", ["f"], reapply ? "Применить сетевые настройки VPN" : "Действия", "global", () => {
      if (state.recovery?.actions.includes("reapply")) void state.perform("network/reapply"); else state.open("recovery");
    }, reason, "notice");
  }
  const cancellable = [...state.tasks.values()].some(task => !["status", "subscription", "subscription/read"].includes(task.action));
  add("global:cancel", ["k"], "Отменить", "global", () => state.cancel(), cancellable ? undefined : "Нет операции для отмены.");
  const screens: [string, string, string, () => Screen][] = [
    ["servers", "v", "Серверы", () => "servers"],
    ["subscription", "c", state.nativeInstalled() && state.status?.subscription === "configured" ? "Профили · подписки и серверы" : "Подписка", () => state.nativeInstalled() && state.status?.subscription === "configured" ? "profiles" : "subscription"],
    ["diagnostics", "d", "Диагностика", () => "diagnostics"],
    ["settings", "i", "Настройки", () => "settings"],
  ];
  for (const [id, key, label, target] of screens) add(`global:${id}`, [key], label, "global", () => state.open(target()), id === "subscription" && !state.subscriptionAvailable() ? state.nativeUnavailable("subscription") : undefined, "notice", true);
  const serverNavigation = actions.find(action => action.id === "global:servers")!;
  serverNavigation.keys = ["v", "h"];
  const tabs = ["servers", "profiles", "diagnostics", "settings"] as const;
  for (const [key, delta] of [["tab", 1], ["shift+tab", -1]] as const) add(`global:${key}`, [key], "Сменить вкладку", "global", () => state.open(tabs[(tabs.indexOf(state.tab) + delta + tabs.length) % tabs.length]!), undefined, "none", true);
  if (state.screen !== "autostart") add("global:autostart", ["a"], "Автозапуск", "global", () => state.open("autostart"));
  // Reuse the established lifecycle actions and their availability rules on every tab.
  const originalScreen = state.screen;
  state.screen = "home";
  const lifecycle = state.items().filter(item => ["s", "x", "z", "m"].includes(item.key));
  state.screen = originalScreen;
  if (!["confirm", "profile-remove", "retry-confirm", "cancel-confirm"].includes(state.screen))
    for (const item of lifecycle) actions.push({ ...item, scope: "global", run: () => { item.run(); state.paint(); } });
  const items = state.items();
  if (state.screen === "servers") {
    add("servers:details", ["l"], "Подробности сервера", "server-table", () => state.open("details"), state.serverID ? undefined : "Нет выбранного сервера.");
    const ping = items.find(item => item.key === "p");
    add(ping?.id ?? "servers:ping-alias", ["p"], "Ping всех + сайт", "server-table", () => void state.runBatch("ping"),
      state.busy || state.batch || state.homeTesting ? "Дождитесь завершения или отмените текущую операцию [k]."
        : state.nativePreviewOnly() ? state.nativeUnavailable("servers/check-batch") : !state.ready() ? `Сначала запустите ${state.serviceObject()}.`
        : state.status?.subscription !== "configured" ? "Сначала добавьте подписку."
        : state.catalogLoaded && !state.servers.length ? "Список серверов пуст." : undefined, "notice");
    if (ping) actions[actions.length - 1]!.preventDefaultKeys = ["p"];
    add("servers:single-speed", ["shift+t"], "Скорость сервера", "server-table", () => { if (!state.serverID) state.notify("Нет выбранного сервера.", true); else void state.testHomeSpeed(state.serverID); }, !state.serverID ? "Нет выбранного сервера." : [...state.tasks.values()].some(task => task.action !== "status") || state.batch || state.homeTesting ? "Дождитесь завершения текущей операции." : state.nativePreviewOnly() ? state.nativeUnavailable("servers/speed") : !state.ready() ? `Сначала запустите ${state.serviceObject()}.` : state.catalogStale ? "Подписка изменена. Обновите список серверов." : undefined, "notice");
    add("servers:sort", ["o"], "Сортировка", "server-table", () => {
      const sorts = ["name", "ping", "speed", "availability"] as const;
      state.frozenOrder = undefined; state.sort = sorts[(sorts.indexOf(state.sort) + 1) % sorts.length]; state.viewStart = 0;
      if (state.batch) state.frozenOrder = state.sortedServers().map(s => s.server_id);
      state.syncServerViewport(); state.paint();
    }, state.servers.length ? undefined : "Список серверов пуст.");
    if (state.servers.length && state.ready() && !state.nativePreviewOnly()) {
      for (const [name, delta] of [["up", -1], ["down", 1], ["pageup", -state.rowCount()], ["pagedown", state.rowCount()], ["home", -Infinity], ["end", Infinity]] as const) {
        const list = state.sortedServers(), at = Math.max(0, list.findIndex(s => s.server_id === state.serverID));
        const next = Math.max(0, Math.min(list.length - 1, at + delta));
        add(`servers:${name}`, [name], name.startsWith("page") ? "Страница" : "Выбор", "server-table", () => {
          state.serverID = list[next]?.server_id; state.syncServerViewport(); state.paint();
        }, list[next]?.server_id === state.serverID ? "Край списка." : undefined);
      }
      add("servers:select", ["return"], "Выбрать сервер", "server-table", () => {
        if (state.catalogStale) { state.notify("Подписка изменена. Обновите список серверов.", true); return; }
        const server = state.servers.find(s => s.server_id === state.serverID);
        if (server) void state.perform("servers/select", server.server_id);
      }, state.catalogStale ? "Подписка изменена. Обновите список серверов." : undefined, "notice");
    }
  }
  // The list is ordered in the same precedence as the former dispatch path.
  const menuScope: ActionScope = ["confirm", "profile-remove", "retry-confirm", "cancel-confirm"].includes(state.screen) ? "confirmation" : "menu";
  const menuMove = (delta: number) => { state.selected = (state.selected + items.length + delta) % Math.max(1, items.length); state.paint(); };
  if (state.screen !== "servers") add("menu:previous", ["up"], "Выбор", menuScope, () => menuMove(-1), items.length <= 1 ? "Нет другого пункта." : undefined);
  if (state.screen !== "servers") add("menu:next", ["down"], "Выбор", menuScope, () => menuMove(1), items.length <= 1 ? "Нет другого пункта." : undefined);
  const current = items[state.selected];
  if (state.screen !== "servers" || !state.ready() || state.nativePreviewOnly()) add("menu:select", ["return"], "Выбрать", menuScope, () => { current?.run(); state.paint(); }, current ? current.unavailableReason : "Нет выбранного пункта.", current?.unavailableBehavior ?? "none");
  if (!items.some(item => item.key === "u")) add("global:refresh", ["u"], state.screen === "diagnostics" ? "Обновить диагностику" : "Обновить состояние", "global", () => { void state.perform(state.screen === "diagnostics" ? "diagnostics" : "status"); state.paint(); }, state.backendOffline ? "Служба управления недоступна." : [...state.tasks.values()].some(task => !compatible(state.screen === "diagnostics" ? "diagnostics" : "status", task.action)) ? "Эта операция конфликтует с текущей." : undefined, state.backendOffline ? "notice" : "none");
  for (const item of items) actions.push({ ...item, preventDefault: true, run: () => { item.run(); state.paint(); } });
  // A shortcut has one effective owner. Earlier definitions have the same
  // precedence as dispatch, including global navigation over menu shortcuts.
  const claimed = new Set<string>();
  return actions.flatMap(action => {
    const keys = action.keys.filter(key => !claimed.has(key));
    keys.forEach(key => claimed.add(key));
    return keys.length ? [{ ...action, key: keys[0]!, keys }] : [];
  });
}

export function matchesAction(action: ActionDefinition, key: AppKeyEvent): boolean {
  return action.keys.some(candidate => candidate === (key.ctrl && key.name === "c" ? "ctrl+c" : key.shift && key.name === "tab" ? "shift+tab" : (key.name === "T" || key.shift && key.name.toLowerCase() === "t") ? "shift+t" : key.shift && key.name.toLowerCase() === "x" ? "X" : key.shift && key.name === "/" ? "?" : key.name));
}

/** Stable, ordered groups usable by a footer, help screen or command palette. */
export function groupedActions(state: AppState) {
  const hint: Record<string, string> = { escape: "Esc", return: "Enter", tab: "Tab", "shift+tab": "Shift+Tab", "ctrl+c": "Ctrl+C", up: "↑", down: "↓", pageup: "PgUp", pagedown: "PgDn", "shift+t": "T", home: "Home", end: "End" };
  const groups: { scope: ActionScope; actions: (ActionDefinition & { keyHints: string[] })[] }[] = [];
  for (const action of actionRegistry(state)) {
    let group = groups.find(group => group.scope === action.scope);
    if (!group) { group = { scope: action.scope, actions: [] }; groups.push(group); }
    group.actions.push({ ...action, keyHints: action.keys.map(key => hint[key] ?? key) });
  }
  return groups;
}
