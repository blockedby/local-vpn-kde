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
    // With no notice, a screen may show a quiet hint in the same place instead of above its list.
    const hint = state.screen === "profiles" ? "Выбор профиля не меняет активное VPN-подключение." : "";
    const message = state.closing ? "Завершаем текущую операцию перед выходом…" : state.notice ? state.notice.replace(/\s*\n\s*/g, " ") : hint;
    const lines = wrapText(message, Math.max(1, this.renderer.terminalWidth - 2));
    this.notification.content = (this.renderer.terminalWidth < 60 ? lines : lines.slice(0, 2)).join("\n");
    this.notification.fg = state.error && state.notice ? p.red : p.muted;
    const reserved = state.noticeReserved();
    const height = !reserved ? 0 : this.renderer.terminalWidth < 60 ? lines.length : 2;
    this.notification.visible = reserved; this.notification.height = height;
    return height;
  }
}
