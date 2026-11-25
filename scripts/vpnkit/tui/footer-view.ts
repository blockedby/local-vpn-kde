import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { palette as p } from "./model";

export class FooterView {
  private footer: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.footer = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 }); root.add(this.footer);
  }
  update(state: AppState, editor: boolean) {
    // Drop whole hints from the end rather than letting the terminal cut one mid-word.
    const hints = this.hints(state, editor).split(" · ");
    while (hints.length > 1 && Bun.stringWidth(hints.join(" · ")) > this.renderer.terminalWidth - 2) hints.pop();
    this.footer.content = hints.join(" · ");
  }
  private hints(state: AppState, editor: boolean): string {
    if (state.helpOpen) { return "↑↓ прокрутка · Esc / ? закрыть"; }
    if (state.screen === "server-rename") { return "Enter сохранить · Esc отмена"; }
    if (state.filterEditing) { return "Enter применить · Esc очистить"; }
    if (state.serverMenu) { return "↑↓ действие · Enter выполнить · Esc закрыть"; }
    if (state.screen === "servers" && state.ready() && !state.nativePreviewOnly() && state.servers.length) {
      const base = "↑↓ сервер · Enter действия · / фильтр · Esc главная";
      let result = base;
      const actions = state.actions();
      for (const [id, hint] of [["global:cancel", "k отменить"], ["servers:p", "p ping"], ["servers:t", "t скорость"], ["servers:sort", "o сортировка"], ["global:help", "? помощь"]] as const)
        if (actions.some(action => action.id === id && action.available) && Bun.stringWidth(`${result} · ${hint}`) <= this.renderer.terminalWidth - 2) result += ` · ${hint}`;
      return result;
    }
    if (state.screen === "tasks") { return `↑↓ выбор${state.items()[state.selected]?.available ? " · Enter отменить" : ""} · Esc назад · ? помощь`; }
    const actions = state.groupedActions().flatMap(group => group.actions).filter(action => action.available);
    if (state.screen === "home") {
      const has = (id: string) => state.groupedActions().flatMap(group => group.actions).some(action => action.id === id);
      const hints = [has("tile:left") && "←↑↓→ выбор", has("tile:open") && "Enter открыть", has("global:help") && "? все клавиши", has("global:quit") && "q выход"].filter(Boolean);
      while (hints.length > 1 && Bun.stringWidth(hints.join(" · ")) > this.renderer.terminalWidth - 2) hints.pop();
      return hints.join(" · ");
    }
    if (isOverlay(state.screen) && !editor) {
      const choices = actions.filter(action => !["global:help", "global:back", "global:quit-control", "menu:previous", "menu:next"].includes(action.id));
      const hints = choices.map(action => `${action.keyHints[0]} ${action.label.toLowerCase()}`);
      hints.push("Esc назад");
      if (actions.some(action => action.id === "global:help")) hints.push("? помощь");
      while (hints.length > 1 && Bun.stringWidth(hints.join(" · ")) > this.renderer.terminalWidth - 2) hints.shift();
      return hints.join(" · ");
    }
    const priorities = editor ? ["editor:submit", "global:back", "global:quit-control"] : state.screen === "servers"
      ? ["global:recovery", "servers:select", "servers:filter", "global:cancel", "servers:details", "servers:p", "servers:t", "servers:r", "home:connect", "home:start-service", "home:x", "servers:sort"]
      : ["global:recovery", "menu:select", "diagnostics:u", "home:connect", "home:start-service", "home:x", "global:cancel", "global:back", "global:tasks"];
    const short: Record<string,string> = { "global:recovery": "восстановить", "servers:select": "выбрать", "servers:filter": "фильтр", "servers:p": state.filter ? `ping ${state.filteredServers().length}` : "ping", "servers:t": state.filter ? `скорость ${state.filteredServers().length}` : "скорость", "servers:r": "обновить", "servers:details": "подробно", "home:connect": "подключить", "home:start-service": "запустить", "home:x": "отключить", "global:cancel": "отменить", "servers:sort": "сортировка", "menu:select": "выбрать", "diagnostics:u": "обновить", "global:back": "назад", "global:tasks": "задачи", "editor:submit": "сохранить", "global:quit-control": "выход" };
    const hints = priorities.flatMap(id => { const a = actions.find(a => a.id === id); return a ? [`${a.keyHints[0]} ${short[id] ?? a.label.toLowerCase()}`] : []; });
    const help = actions.find(a => a.id === "global:help");
    const suffix = help ? "? помощь" : "";
    const width = this.renderer.terminalWidth - 2;
    while (hints.length && Bun.stringWidth([...hints, suffix].filter(Boolean).join(" · ")) > width) hints.pop();
    return [...hints, suffix].filter(Boolean).join(" · ");
  }
}
