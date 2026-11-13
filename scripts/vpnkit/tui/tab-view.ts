import { BoxRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { frames } from "./animation-frames";
import { palette as p } from "./model";
import { cell } from "./formatting";

export class TabView {
  private row: BoxRenderable;
  private labels: TextRenderable[] = [];
  private titles = ["Серверы", "Профили", "Диагностика", "Настройки"];
  private activity: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable, open: (tab: AppState["tab"]) => void) {
    this.row = new BoxRenderable(renderer, { height: 1, flexShrink: 0, flexDirection: "row" }); root.add(this.row);
    for (const [tab, label] of [["servers", "Серверы"], ["profiles", "Профили"], ["diagnostics", "Диагностика"], ["settings", "Настройки"]] as const) {
      const text = new TextRenderable(renderer, { content: ` ${label} `, height: 1, flexShrink: 0, onMouseDown: e => { if (e.button === 0) open(tab); } });
      this.labels.push(text); this.row.add(text);
    }
    this.activity = new TextRenderable(renderer, { content: "", fg: p.accent, height: 1, flexGrow: 1, flexShrink: 1 }); this.row.add(this.activity);
  }
  update(state: AppState) {
    const tabs = ["servers", "profiles", "diagnostics", "settings"];
    this.labels.forEach((label, i) => { label.content = state.tab === tabs[i] ? `[${this.titles[i]}]` : ` ${this.titles[i]} `; label.fg = state.tab === tabs[i] ? p.base : p.muted; label.bg = state.tab === tabs[i] ? p.accent : p.base; label.attributes = state.tab === tabs[i] ? TextAttributes.BOLD : 0; });
    const task = [...state.tasks.values()].find(t => t.action !== "status");
    const message = state.batch ? `Проверка ${state.batch.done}/${state.batch.total}` : task ? state.actionLabel(task.action) : "";
    const width = Math.max(0, this.renderer.terminalWidth - 2 - 44);
    const suffix = state.queue.length && width < 20 ? `очередь ${state.queue.length} [j]` : `${state.queue.length ? ` · в очереди ${state.queue.length}` : ""} [j]`;
    const prefix = message && !(state.queue.length && width < 20) ? `${frames[state.frame % 10]} ${message}` : "";
    const text = message || state.queue.length ? `${cell(prefix, Math.max(0, width - Bun.stringWidth(suffix))).trimEnd()}${suffix}` : "";
    this.activity.content = width > 0 && text ? cell(text, width).trimEnd().padStart(width) : "";
  }
}
