import { isOverlay, type AppKeyEvent, type AppState, type Screen } from "./app-state";
import { compatible } from "./request-policy";
import type { ActionDefinition, ActionScope } from "./menu-actions";

export const HOME_CONNECT_KEY = "s";

/** Keys use OpenTUI names; modified keys use ctrl+ / shift+ prefixes. */
export function actionRegistry(state: AppState, rowActions = false): ActionDefinition[] {
  const editor = ["subscription", "target", "profile-name", "profile-source", "server-rename"].includes(state.screen);
  const actions: ActionDefinition[] = [];
  const add = (id: string, keys: string[], label: string, scope: ActionScope, run: () => void,
    reason?: string, unavailableBehavior: "notice" | "none" = "none", preventDefault = false) => {
    actions.push({ id, key: keys[0]!, keys, label, scope, run, available: !reason,
      ...(reason ? { unavailableReason: reason, unavailableBehavior } : {}), preventDefault });
  };
  if (state.paletteOpen) {
    add("palette:close", ["escape"], "Закрыть команды", "editor", () => { state.paletteOpen = false; state.paint(); }, undefined, "none", true);
    add("palette:run", ["return"], "Выполнить команду", "editor", () => state.runPaletteAction(), state.filteredPaletteActions()[state.paletteIndex]?.action.available ? undefined : "Команда недоступна.", "none", true);
    add("palette:up", ["up"], "Предыдущая команда", "editor", () => { state.paletteIndex = Math.max(0, state.paletteIndex - 1); state.paintPalette(); }, state.paletteIndex > 0 ? undefined : "Начало списка.", "none", true);
    add("palette:down", ["down"], "Следующая команда", "editor", () => { state.paletteIndex = Math.min(state.filteredPaletteActions().length - 1, state.paletteIndex + 1); state.paintPalette(); }, state.paletteIndex + 1 < state.filteredPaletteActions().length ? undefined : "Конец списка.", "none", true);
    add("global:quit-control", ["ctrl+c"], "Выход", "global", () => void state.close(), undefined, "none", true);
    return actions;
  }
  if (state.filterEditing) {
    add("filter:accept", ["return"], "Применить фильтр", "editor", () => { state.filterEditing = false; state.paint(); }, undefined, "none", true);
    add("filter:clear", ["escape"], "Очистить фильтр", "editor", () => { state.filterEditing = false; state.editFilter(""); if (state.filterAnchor && state.servers.some(s => s.server_id === state.filterAnchor)) state.serverID = state.filterAnchor; state.filterAnchor = undefined; state.syncServerViewport(); state.paint(); }, undefined, "none", true);
    add("global:quit-control", ["ctrl+c"], "Выход", "global", () => void state.close(), undefined, "none", true);
    return actions;
  }
  if (state.helpOpen) {
    add("help:close", ["escape", "?"], "Закрыть помощь", "global", () => { state.helpOpen = false; state.paint(); }, undefined, "none", true);
    add("help:up", ["up", "pageup"], "Вверх", "global", () => { state.helpOffset = Math.max(0, state.helpOffset - 1); state.paint(); }, state.helpOffset === 0 ? "Начало списка." : undefined);
    add("help:down", ["down", "pagedown"], "Вниз", "global", () => { state.helpOffset++; state.paint(); });
    return actions;
  }
  if (state.serverMenu && !rowActions) {
    const entries = serverMenuActions(state);
    const move = (index: number) => { state.serverMenuIndex = index; state.paint(); };
    add("server-menu:close", ["escape"], "Закрыть меню", "menu", () => state.closeServerMenu(), undefined, "none", true);
    add("global:help", ["?"], "Помощь", "global", () => { state.helpOffset = 0; state.helpOpen = true; state.paint(); }, undefined, "none", true);
    for (const [key, next] of [["up", Math.max(0, state.serverMenuIndex - 1)], ["down", Math.min(entries.length - 1, state.serverMenuIndex + 1)], ["home", 0], ["end", entries.length - 1]] as const)
      add(`server-menu:${key}`, [key], "Выбор действия", "menu", () => move(next), next === state.serverMenuIndex || next < 0 ? "Край списка." : undefined, "none", true);
    const current = entries[state.serverMenuIndex];
    add("server-menu:run", ["return"], "Выполнить действие", "menu", () => current?.run(), current?.unavailableReason ?? (!current ? "Нет выбранного действия." : undefined), "notice", true);
    for (const entry of entries.filter(item => item.key !== "return")) actions.push(entry);
    add("global:quit-control", ["ctrl+c"], "Выход", "global", () => void state.close(), undefined, "none", true);
    return actions;
  }
  if (!editor) add("global:help", ["?"], "Помощь", "global", () => { state.helpOffset = 0; state.helpOpen = true; state.paint(); }, undefined, "none", true);
  add("global:back", ["escape"], state.screen === "servers" && state.filter ? "Очистить фильтр" : "Назад", editor ? "editor" : "global", () => { if (state.screen === "home") return; if (state.screen === "servers" && state.filter) { state.editFilter(""); if (state.filterAnchor && state.servers.some(s => s.server_id === state.filterAnchor)) state.serverID = state.filterAnchor; state.filterAnchor = undefined; state.syncServerViewport(); state.paint(); return; } state.open(
    state.screen.startsWith("profile-") ? "profiles"
      : state.screen === "server-rename" ? state.overlayBase
      : state.screen === "target" ? state.tab
      : state.screen === "confirm" && state.confirmation === "subscription" ? "subscription"
      : state.screen === "cancel-confirm" ? "tasks"
      : state.screen === "retry-confirm" ? "recovery"
      : state.screen === "tasks" ? "home"
      : ["autostart", "details", "confirm", "recovery", "subscription"].includes(state.screen) ? state.overlayBase : "home", state.screen !== "confirm"); }, state.closing ? "Закрываем приложение." : state.screen === "home" ? "Уже на главной." : undefined, "none", true);
  add("global:quit-control", ["ctrl+c"], "Выход", "global", () => void state.close(), state.closing || state.disposed ? "Закрываем приложение." : undefined, "none", true);
  if (editor) {
    add("editor:submit", ["return"], "Сохранить", "editor", () => {
      if (state.screen === "server-rename") { void state.saveServerRename(); return; }
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
    if (["confirm", "profile-remove", "retry-confirm", "cancel-confirm"].includes(state.screen)) {
      for (const [key, delta] of [["left", -1], ["right", 1], ["tab", 1], ["shift+tab", -1]] as const)
        add(`confirmation:${key}`, [key], "Выбор", "confirmation", () => move(delta), items.length <= 1 ? "Нет другого пункта." : undefined, "none", true);
    }
    add("menu:previous", ["up"], "Выбор", "menu", () => move(-1), items.length <= 1 ? "Нет другого пункта." : undefined);
    add("menu:next", ["down"], "Выбор", "menu", () => move(1), items.length <= 1 ? "Нет другого пункта." : undefined);
    const current = items[state.selected];
    add("menu:select", ["return"], "Выбрать", "menu", () => { current?.run(); state.paint(); }, current?.unavailableReason ?? (!current ? "Нет выбранного пункта." : undefined), current?.unavailableBehavior ?? "none");
    for (const item of items) actions.push({ ...item, preventDefault: true, run: () => { item.run(); state.paint(); } });
    return actions;
  }
  add("global:quit", ["q"], "Выход", "global", () => void state.close(), state.closing || state.disposed ? "Закрываем приложение." : undefined);
  if (state.closing) return actions;
  add("global:palette", [":", "ctrl+p"], "Команды", "global", () => { state.paletteOpen = true; state.paletteQuery = ""; state.paletteIndex = 0; state.paint(); }, undefined, "none", true);
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
  const cancellable = state.queue.length > 0 || [...state.tasks.values()].some(task => task.action !== "status");
  add("global:cancel", ["k"], "Отменить", "global", () => state.cancel(), cancellable ? undefined : "Нет операции для отмены.");
  const screens: [string, string, string, () => Screen][] = [
    ["servers", "v", "Серверы", () => "servers"],
    ["subscription", "c", state.opensProfiles() ? "Профили · подписки и серверы" : state.nativeProfileStale() ? "Подписка · повторить настройку native" : "Подписка", () => state.opensProfiles() ? "profiles" : "subscription"],
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
  const lifecycle = state.items().filter(item => [HOME_CONNECT_KEY, "x", "z", "m"].includes(item.key));
  state.screen = originalScreen;
  if (!["confirm", "profile-remove", "retry-confirm", "cancel-confirm"].includes(state.screen))
    for (const item of lifecycle) actions.push({ ...item, scope: "global", run: () => { item.run(); state.paint(); } });
  if (state.screen === "home") {
    for (const key of ["left", "right", "up", "down", "home", "end"]) {
      const cols = state.viewport().width >= 90 ? 3 : 2, i = state.homeIndex;
      const blocked = key === "left" ? i % cols === 0 : key === "right" ? i % cols === cols - 1 || i === 5 : key === "up" ? i < cols : key === "down" ? i + cols >= 6 : key === "home" ? i === 0 : i === 5;
      add(`tile:${key}`, [key], "Выбор", "home", () => state.moveHome(key), blocked ? "Край сетки." : undefined, "none", true);
    }
    const lifecycleAction = actions.find(action => ["home:connect", "home:start-service", "home:x"].includes(action.id));
    add("tile:open", ["return"], "Открыть", "home", () => state.activateHome(), state.homeIndex === 0 ? lifecycleAction?.unavailableReason : undefined, lifecycleAction?.unavailableBehavior ?? "none", true);
  }
  const items = state.items();
  if (state.screen === "servers") {
    add("servers:filter", ["/"], "Фильтр серверов", "server-table", () => { state.filterEditing = true; state.filterAnchor = state.serverID; state.paint(); }, state.ready() && state.servers.length ? undefined : "Список серверов недоступен.", "none", true);
    add("servers:fastest", ["b"], "Подключить самый быстрый", "server-table", () => state.confirmFastest());
    add("servers:details", ["l"], "Подробности сервера", "server-table", () => state.open("details"), state.serverID ? undefined : "Нет выбранного сервера.");
    if (rowActions) add("servers:rename", ["n"], "Переименовать", "server-table", () => state.openServerRename(), state.serverID ? undefined : "Нет выбранного сервера.", "notice", true);
    const ping = items.find(item => item.key === "p");
    add(ping?.id ?? "servers:ping-alias", ["p"], state.filter ? `Ping показанных (${state.filteredServers().length}) + сайт` : "Ping всех + сайт", "server-table", () => void state.runBatch("ping"),
      state.nativePreviewOnly() ? state.nativeUnavailable("servers/check-batch") : !state.ready() ? `Сначала запустите ${state.serviceObject()}.`
        : state.status?.subscription !== "configured" ? "Сначала добавьте подписку."
        : state.catalogLoaded && !state.servers.length ? "Список серверов пуст." : undefined, "notice");
    if (ping) actions[actions.length - 1]!.preventDefaultKeys = ["p"];
    add("servers:single-speed", ["shift+t"], "Скорость сервера", "server-table", () => { if (!state.serverID) state.notify("Нет выбранного сервера.", true); else void state.testHomeSpeed(state.serverID); }, !state.serverID ? "Нет выбранного сервера." : state.nativePreviewOnly() ? state.nativeUnavailable("servers/speed") : !state.ready() ? `Сначала запустите ${state.serviceObject()}.` : state.catalogStale ? "Подписка изменена. Обновите список серверов." : undefined, "notice");
    add("servers:sort", ["o"], "Сортировка", "server-table", () => {
      const sorts = ["name", "ping", "speed", "availability", "country"] as const;
      const next = sorts[(sorts.indexOf(state.sort) + 1) % sorts.length]!; state.setSort(next);
    }, state.servers.length ? undefined : "Список серверов пуст.");
    for (const [key, delta] of [["[", -1], ["]", 1]] as const) add(`servers:sort-${key}`, [key], "Столбец сортировки", "server-table", () => { const sorts = ["name", "ping", "speed", "availability", "country"] as const; state.setSort(sorts[(sorts.indexOf(state.sort) + delta + sorts.length) % sorts.length]!); }, state.servers.length ? undefined : "Список серверов пуст.");
    add("servers:sort-reverse", ["\\"], "Обратить сортировку", "server-table", () => state.setSort(state.sort), state.servers.length ? undefined : "Список серверов пуст.");
    if (rowActions || state.servers.length && state.ready() && !state.nativePreviewOnly()) {
      for (const [name, delta] of [["up", -1], ["down", 1], ["pageup", -state.rowCount()], ["pagedown", state.rowCount()], ["home", -Infinity], ["end", Infinity]] as const) {
        const list = state.sortedServers(), at = Math.max(0, list.findIndex(s => s.server_id === state.serverID));
        const next = Math.max(0, Math.min(list.length - 1, at + delta));
        add(`servers:${name}`, [name], name.startsWith("page") ? "Страница" : "Выбор", "server-table", () => {
          state.serverID = list[next]?.server_id; if (state.filter) state.filterAnchor = state.serverID; state.syncServerViewport(); state.paint();
        }, list[next]?.server_id === state.serverID ? "Край списка." : undefined);
      }
      add(rowActions ? "servers:select" : "servers:open-menu", ["return"], rowActions ? "Выбрать сервер" : "Действия сервера", "server-table", () => rowActions ? state.confirmServerSelection() : state.openServerMenu(), !state.serverID ? "Нет выбранного сервера." : rowActions && state.nativePreviewOnly() ? state.nativeUnavailable("servers/select") : rowActions && !state.ready() ? `Сначала запустите ${state.serviceObject()}.` : rowActions && state.catalogStale ? "Подписка изменена. Обновите список серверов." : undefined, "notice");
    }
  }
  // The list is ordered in the same precedence as the former dispatch path.
  const menuScope: ActionScope = ["confirm", "profile-remove", "retry-confirm", "cancel-confirm"].includes(state.screen) ? "confirmation" : "menu";
  const menuMove = (delta: number) => { state.selected = (state.selected + items.length + delta) % Math.max(1, items.length); state.paint(); };
  if (state.screen !== "servers" && state.screen !== "home") add("menu:previous", ["up"], "Выбор", menuScope, () => menuMove(-1), items.length <= 1 ? "Нет другого пункта." : undefined);
  if (state.screen !== "servers" && state.screen !== "home") add("menu:next", ["down"], "Выбор", menuScope, () => menuMove(1), items.length <= 1 ? "Нет другого пункта." : undefined);
  const current = items[state.selected];
  if (state.screen !== "home" && (state.screen !== "servers" || !state.ready() || state.nativePreviewOnly())) add("menu:select", ["return"], "Выбрать", menuScope, () => { current?.run(); state.paint(); }, current ? current.unavailableReason : "Нет выбранного пункта.", current?.unavailableBehavior ?? "none");
  if (!items.some(item => item.key === "u")) add("global:refresh", ["u"], state.screen === "diagnostics" ? "Обновить диагностику" : "Обновить состояние", "global", () => { void state.perform(state.screen === "diagnostics" ? "diagnostics" : "status"); state.paint(); }, state.backendOffline ? "Служба управления недоступна." : undefined, state.backendOffline ? "notice" : "none");
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

/** Row entries retain the registry's handlers and availability, bound to the captured server. */
export function serverMenuActions(state: AppState): ActionDefinition[] {
  const target = state.serverMenuTarget ?? (state.serverID ? state.servers.find(s => s.server_id === state.serverID) && { id: state.serverID, name: state.servers.find(s => s.server_id === state.serverID)!.display_name } : undefined);
  const previousID = state.serverID;
  if (target) state.serverID = target.id;
  const actions = actionRegistry(state, true);
  state.serverID = previousID;
  const ping = actions.find(item => item.id === "servers:p" || item.id === "servers:ping-alias");
  const singlePing = ping && { ...ping, id: "servers:single-check", key: "", keys: [], label: "Ping и сайт",
    unavailableReason: state.catalogStale ? "Подписка изменена. Обновите список серверов." : ping.unavailableReason,
    run: () => { if (target) void state.runBatch("ping", { ids: [target.id], target: state.target }); } };
  return ["servers:select", "servers:single-check", "servers:single-speed", "servers:details", "servers:rename"].flatMap(id => {
    const action = id === "servers:single-check" ? singlePing : actions.find(item => item.id === id);
    if (!action) return [];
    const valid = !!target && state.servers.some(s => s.server_id === target.id && s.display_name === target.name);
    const reason = !valid ? "Сервер изменился. Выберите сервер заново." : action.unavailableReason;
    return [{ ...action, available: !reason, ...(reason ? { unavailableReason: reason, unavailableBehavior: "notice" as const } : {}), run: () => {
      if (!target || !state.servers.some(s => s.server_id === target.id && s.display_name === target.name)) { state.notify("Сервер изменился. Выберите сервер заново.", true); return; }
      state.closeServerMenu(); state.serverID = target.id; action.run();
    } }];
  });
}

export function matchesAction(action: ActionDefinition, key: AppKeyEvent): boolean {
  return action.keys.some(candidate => candidate === (key.ctrl ? `ctrl+${key.name.toLowerCase()}` : key.shift && key.name === "tab" ? "shift+tab" : (key.name === "T" || key.shift && key.name.toLowerCase() === "t") ? "shift+t" : key.shift && key.name.toLowerCase() === "x" ? "X" : key.shift && key.name === "/" ? "?" : key.name));
}

/** Stable, ordered groups usable by a footer, help screen or command palette. */
export function groupedActions(state: AppState) {
  const hint: Record<string, string> = { escape: "Esc", return: "Enter", tab: "Tab", "shift+tab": "Shift+Tab", "ctrl+c": "Ctrl+C", "ctrl+p": "Ctrl+P", up: "↑", down: "↓", pageup: "PgUp", pagedown: "PgDn", "shift+t": "T", home: "Home", end: "End", left: "←", right: "→" };
  const groups: { scope: ActionScope; actions: (ActionDefinition & { keyHints: string[] })[] }[] = [];
  const listed = actionRegistry(state);
  if (state.serverMenu && !state.helpOpen) {
    const select = serverMenuActions(state).find(action => action.id === "servers:select");
    if (select) listed.push(select);
  }
  for (const action of listed) {
    let group = groups.find(group => group.scope === action.scope);
    if (!group) { group = { scope: action.scope, actions: [] }; groups.push(group); }
    group.actions.push({ ...action, keyHints: action.keys.map(key => hint[key] ?? key) });
  }
  return groups;
}
