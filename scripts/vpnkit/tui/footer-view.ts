import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { palette as p } from "./model";
import { isListMenu } from "./menu-actions";

export class FooterView {
  private footer: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.footer = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 }); root.add(this.footer);
  }
  update(state: AppState, editor: boolean) {
    const suffix = !editor && !state.filterEditing && !state.helpOpen ? ["? все клавиши"] : [];
    const hints = this.hints(state, editor).split(" · ").filter(Boolean);
    const line = () => [...hints, ...suffix].join(" · ");
    while (hints.length && Bun.stringWidth(line()) > this.renderer.terminalWidth - 2) {
      if (isOverlay(state.screen) && !editor && !state.helpOpen && state.screen !== "tasks") hints.shift();
      else hints.pop();
    }
    this.footer.content = line();
  }
  private hints(state: AppState, editor: boolean): string {
    if (state.helpOpen) { return `↑↓ выбор · Enter выполнить${state.searchFocused ? "" : " · / поиск"} · Esc ${state.paletteQuery ? "очистить" : "закрыть"}`; }
    if (state.screen === "server-rename") { return "Enter сохранить · Esc отмена"; }
    if (state.filterEditing) { return "Enter применить · Esc очистить"; }
    if (state.serverMenu) { return "↑↓ действие · Enter выполнить · Esc закрыть"; }
    if (state.screen === "servers" && state.hasServerTable()) {
      const base = "↑↓ сервер · Enter действия · / фильтр · Esc главная";
      let result = base;
      const actions = state.actions();
      for (const [id, hint] of [["global:cancel", "k отменить"], ["servers:p", "p ping"], ["servers:t", "t скорость"], ["servers:r", "r обновить"], ["servers:sort", "o сортировка"]] as const)
        if (actions.some(action => action.id === id && action.available)) result += ` · ${hint}`;
      return result;
    }
    if (state.screen === "tasks") { return `↑↓ выбор${state.items()[state.selected]?.available ? " · Enter отменить" : ""} · Esc назад`; }
    // Servers without a table (service stopped, no subscription) show a plain menu: drive it with arrows too.
    if (state.screen === "servers") {
      const rows = state.visibleMenuItems(), item = rows[state.selected];
      return `${rows.length > 1 ? "↑↓ выбор · " : ""}Enter ${(item?.title ?? item?.label ?? "выбрать").toLowerCase()} · Esc главная`;
    }
    if (isListMenu(state.screen)) { const kind = state.items()[state.selected]?.kind; return `↑↓ выбор${kind === "toggle" ? " · ←→ изменить" : kind === "open" ? " · Enter открыть" : " · Enter выполнить"} · Esc назад`; }
    const actions = state.groupedActions().flatMap(group => group.actions).filter(action => action.available);
    if (state.screen === "home") {
      const has = (id: string) => state.groupedActions().flatMap(group => group.actions).some(action => action.id === id);
      const hints = [has("tile:left") && (state.homeColumns() === 1 ? "↑↓ выбор" : "←↑↓→ выбор"), has("tile:open") && "Enter открыть", has("global:quit") && "q выход"].filter(Boolean);
      return hints.join(" · ");
    }
    if (isOverlay(state.screen) && !editor) {
      const choices = actions.filter(action => !["global:help", "global:palette", "global:back", "global:quit-control", "menu:previous", "menu:next"].includes(action.id));
      const hints = choices.map(action => `${action.keyHints[0]} ${action.label.toLowerCase()}`);
      hints.push("Esc назад");
      return hints.join(" · ");
    }
    const priorities = editor ? ["editor:submit", "global:back", "global:quit-control"]
      : ["global:recovery", "menu:select", "diagnostics:u", "home:connect", "home:start-service", "home:x", "global:cancel", "global:back", "global:tasks"];
    const short: Record<string,string> = { "global:recovery": "восстановить", "servers:select": "выбрать", "servers:filter": "фильтр", "servers:p": state.filter ? `ping ${state.filteredServers().length}` : "ping", "servers:t": state.filter ? `скорость ${state.filteredServers().length}` : "скорость", "servers:r": "обновить", "servers:details": "подробно", "home:connect": "подключить", "home:start-service": "запустить", "home:x": "отключить", "global:cancel": "отменить", "servers:sort": "сортировка", "menu:select": "выбрать", "diagnostics:u": "обновить", "global:back": "назад", "global:tasks": "задачи", "editor:submit": "сохранить", "global:quit-control": "выход" };
    const hints = priorities.flatMap(id => { const a = actions.find(a => a.id === id); return a ? [`${a.keyHints[0]} ${short[id] ?? a.label.toLowerCase()}`] : []; });
    return hints.join(" · ");
  }
}
