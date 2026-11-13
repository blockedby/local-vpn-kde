import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { palette as p } from "./model";
import { cell } from "./formatting";

/** Absolute overlay host. Other modal views can use the same positioning pattern. */
export class OverlayView {
  private backdrop: BoxRenderable;
  private box: BoxRenderable;
  private text: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.backdrop = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 0, width: "100%", height: 1, zIndex: 99, backgroundColor: p.base, visible: false });
    root.add(this.backdrop);
    this.box = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 2, width: 54, height: 10, zIndex: 100, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, title: " Помощь · ↑↓ прокрутка · Esc закрыть ", overflow: "hidden", visible: false });
    this.text = new TextRenderable(renderer, { content: "", fg: p.text, width: "100%", height: "100%" });
    this.box.add(this.text); root.add(this.box);
  }
  update(state: AppState) {
    this.box.visible = state.helpOpen;
    this.backdrop.visible = state.helpOpen;
    if (!state.helpOpen) return;
    state.helpOpen = false;
    const groups = state.groupedActions();
    state.helpOpen = true;
    const width = Math.min(76, this.renderer.terminalWidth - 4), height = Math.min(this.renderer.terminalHeight - 4, 30);
    this.backdrop.height = Math.max(1, this.renderer.terminalHeight - 3);
    this.box.width = width; this.box.height = height;
    this.box.left = Math.floor((this.renderer.terminalWidth - width) / 2);
    this.box.top = Math.floor((this.renderer.terminalHeight - height) / 2);
    const names: Record<string,string> = { global: "Общие", "server-table": "Серверы", menu: "Выбор", editor: "Редактор", confirmation: "Подтверждение", servers: "Серверы", profiles: "Профили", diagnostics: "Диагностика", settings: "Настройки", autostart: "Автозапуск" };
    const wrap = (line: string) => {
      const result: string[] = []; let current = "";
      for (const word of line.split(/\s+/)) {
        const next = current ? `${current} ${word}` : word;
        if (Bun.stringWidth(next) > width - 4 && current) { result.push(current); current = `  ${word}`; }
        else current = next;
      }
      if (current) result.push(current);
      return result;
    };
    const lines = groups.flatMap(group => {
      const merged: typeof group.actions = [];
      for (const action of group.actions) {
        const existing = merged.find(a => a.label === action.label && a.available === action.available && a.unavailableReason === action.unavailableReason);
        if (existing) existing.keyHints.push(...action.keyHints);
        else merged.push({ ...action, keyHints: [...action.keyHints] });
      }
      return [names[group.scope] ?? group.scope, ...merged.flatMap(a => wrap(`${a.keyHints.join("/")}  ${a.label}${a.available ? "" : ` · ${a.unavailableReason ?? "недоступно"}`}`))];
    });
    state.helpOffset = Math.min(state.helpOffset, Math.max(0, lines.length - (height - 2)));
    this.text.content = lines.slice(state.helpOffset, state.helpOffset + height - 2).map(line => cell(line, width - 2).trimEnd()).join("\n");
  }
}
