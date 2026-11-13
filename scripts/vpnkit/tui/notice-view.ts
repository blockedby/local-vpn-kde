import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { palette as p } from "./model";
import { wrapText } from "./formatting";
export class NoticeView {
  private notification: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.notification = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 }); root.add(this.notification);
  }
  update(state: AppState) {
    const message = state.closing ? "Завершаем текущую операцию перед выходом…" : state.notice.replace(/\s*\n\s*/g, " ");
    const lines = wrapText(message, Math.max(1, this.renderer.terminalWidth - 2));
    this.notification.content = lines.join("\n");
    this.notification.fg = state.error ? p.red : p.muted;
    this.notification.visible = !!message; this.notification.height = lines.length;
  }
}
