import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { palette as p } from "./model";

export class FooterView {
  private footer: TextRenderable;
  constructor(renderer: CliRenderer, root: BoxRenderable) {
    this.footer = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 });
    root.add(this.footer);
  }
  update(state: AppState, editor: boolean) {
    this.footer.content = editor
      ? "Esc назад · Ctrl+C выход"
      : state.screen === "servers"
        ? "Tab меню/список · ↑↓ · Enter · o сортировка · j задачи · Esc назад · q выход"
        : "↑↓ / Tab выбор · Enter · Esc назад · k отменить · q выход";
  }
}
