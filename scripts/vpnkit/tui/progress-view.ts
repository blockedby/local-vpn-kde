import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { frames } from "./animation-frames";
import { names } from "./action-labels";
import { cell } from "./formatting";
import { palette as p } from "./model";

export class ProgressView {
  private progress: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.progress = new TextRenderable(renderer, { content: "", fg: p.accent, height: 1, flexShrink: 0 });
    root.add(this.progress);
  }
  update(state: AppState) {
    const width = Math.max(10, this.renderer.terminalWidth - 2);
    const activeTask = [...state.tasks.values()].find(task => task.measurement) ??
      [...state.tasks.values()].find(task => task.action !== "status");
    if (state.batch) {
      const detail = `${state.batch.kind === "speed" ? "Ping → сайт + скорость" : "Ping → сайт"} · ${state.batch.done}/${state.batch.total} · ${Math.floor((Date.now() - state.started) / 1000)} с`;
      this.progress.content = `${frames[state.frame % 10]} ${detail}${state.cancelled ? " · отмена…" : " · [k] отменить"}`;
    }
    else if (activeTask) {
      this.progress.content = `${frames[state.frame % 10]} ${state.actionLabel(activeTask.action)} · ${activeTask.phase || "выполняется"} · ${Math.floor((Date.now() - activeTask.started) / 1000)} с`;
    }
    else
      this.progress.content =
        state.checked && Date.now() - state.checked > 15000
          ? "Статус устарел · [u] проверить"
          : "";
    const switching = [...state.tasks.values()].find(task => task.action === "servers/select");
    if (switching && (state.batch || state.homeTesting))
      this.progress.content = `${this.progress.chunks.map(chunk => chunk.text).join("")} · Применяем сервер`;
    const activeRows = [...state.tasks.values()].filter(task => task.action !== "status");
    if (activeRows.length > 1) this.progress.content = activeRows.map(task =>
      cell(`${frames[state.frame % 10]} ${names[task.action] ?? "Ping → сайт"} · ${task.phase || "выполняется"} · [j] задачи`, width).trimEnd()).join("\n");
    const inlineProgress = state.screen === "home" && state.inlineSpeed() && state.homeTesting && !switching;
    this.progress.visible = this.progress.chunks.some(chunk => chunk.text.length > 0) && !inlineProgress;
    this.progress.height = this.progress.visible ? Math.max(1, activeRows.length) : 0;
  }
}
