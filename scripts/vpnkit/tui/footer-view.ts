import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { palette as p } from "./model";

export class FooterView {
  private footer: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.footer = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 }); root.add(this.footer);
  }
  update(state: AppState, editor: boolean) {
    if (state.helpOpen) { this.footer.content = "↑↓ прокрутка · Esc / ? закрыть"; return; }
    const actions = state.groupedActions().flatMap(group => group.actions).filter(action => action.available);
    if (isOverlay(state.screen) && !editor) {
      const choices = actions.filter(action => !["global:help", "global:back", "global:quit-control", "menu:previous", "menu:next"].includes(action.id));
      const hints = choices.map(action => `${action.keyHints[0]} ${action.label.toLowerCase()}`);
      hints.push("Esc назад");
      if (actions.some(action => action.id === "global:help")) hints.push("? помощь");
      while (hints.length > 1 && Bun.stringWidth(hints.join(" · ")) > this.renderer.terminalWidth - 2) hints.shift();
      this.footer.content = hints.join(" · "); return;
    }
    const priorities = editor ? ["editor:submit", "global:back", "global:quit-control"] : state.screen === "servers"
      ? ["global:recovery", "servers:select", "servers:p", "servers:t", "servers:r", "home:connect", "home:start-service", "home:x", "global:cancel", "servers:sort"]
      : ["global:recovery", "menu:select", "diagnostics:u", "home:connect", "home:start-service", "home:x", "global:cancel", "global:back", "global:tasks"];
    const short: Record<string,string> = { "global:recovery": "восстановить", "servers:select": "выбрать", "servers:p": "ping", "servers:t": "скорость", "servers:r": "обновить", "home:connect": "подключить", "home:start-service": "запустить", "home:x": "отключить", "global:cancel": "отменить", "servers:sort": "сортировка", "menu:select": "выбрать", "diagnostics:u": "обновить", "global:back": "назад", "global:tasks": "задачи", "editor:submit": "сохранить", "global:quit-control": "выход" };
    const hints = priorities.flatMap(id => { const a = actions.find(a => a.id === id); return a ? [`${a.keyHints[0]} ${short[id] ?? a.label.toLowerCase()}`] : []; });
    const help = actions.find(a => a.id === "global:help");
    const suffix = help ? "? помощь" : "";
    const width = this.renderer.terminalWidth - 2;
    while (hints.length && Bun.stringWidth([...hints, suffix].filter(Boolean).join(" · ")) > width) hints.pop();
    this.footer.content = [...hints, suffix].filter(Boolean).join(" · ");
  }
}
