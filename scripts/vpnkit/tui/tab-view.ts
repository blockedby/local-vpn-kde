import { BoxRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { palette as p } from "./model";

export class TabView {
  private row: BoxRenderable;
  private title: TextRenderable;
  private activity: TextRenderable;
  constructor(renderer: CliRenderer, root: BoxRenderable, _open: (tab: AppState["tab"]) => void) {
    this.row = new BoxRenderable(renderer, { height: 1, flexShrink: 0, flexDirection: "row" }); root.add(this.row);
    this.row.add(new TextRenderable(renderer, { content: "‹ Главная › ", fg: p.muted, height: 1 }));
    this.title = new TextRenderable(renderer, { content: "", fg: p.text, height: 1, attributes: TextAttributes.BOLD }); this.row.add(this.title);
    this.activity = new TextRenderable(renderer, { content: "", fg: p.accent, height: 1, flexGrow: 1 }); this.row.add(this.activity);
  }
  update(state: AppState) {
    const screen = state.screen === "tasks" ? "tasks" : state.screen === "home" || isOverlay(state.screen) && state.overlayBase === "home" ? "home" : state.tab;
    this.row.visible = screen !== "home";
    const task = [...state.tasks.values()].find(t => t.action !== "status");
    this.activity.content = state.batch || task || state.queue.length ? `  ${state.batch ? `Проверка ${state.batch.done}/${state.batch.total}` : task ? state.actionLabel(task.action) : ""}${state.queue.length ? ` · в очереди ${state.queue.length}` : ""} [j]` : "";
    this.title.content = ({ servers: "Серверы", profiles: "Профили", diagnostics: "Диагностика", settings: "Настройки", tasks: "Замеры", home: "" })[screen];
  }
}
