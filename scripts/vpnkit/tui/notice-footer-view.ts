import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { palette as p } from "./model";

export class NoticeFooterView {
  private notification: TextRenderable;
  private footer: TextRenderable;
  constructor(renderer: CliRenderer, root: BoxRenderable) {
    this.notification = new TextRenderable(renderer, { content: "", fg: p.muted, height: 2, flexShrink: 0 });
    root.add(this.notification);
    this.footer = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 });
    root.add(this.footer);
  }
  update(state: AppState, editor: boolean) {
    this.notification.content = state.closing
      ? "Завершаем текущую операцию перед выходом…"
      : state.notice + (state.error && state.recovery && !state.recovery.actions.includes("reapply") ? "\n[f] Действия" : "") +
        (state.error && state.noticeAttempt
          ? `\nПопытка: ${state.noticeAttempt}`
          : "");
    this.notification.fg = state.error ? p.red : p.muted;
    const hasNotice = state.closing || !!state.notice;
    this.notification.visible = hasNotice;
    this.notification.height = hasNotice ? (state.error && state.recovery && state.noticeAttempt ? 3 : 2) : 0;
    this.footer.content = editor
      ? "Esc назад · Ctrl+C выход"
      : state.screen === "servers"
        ? "Tab меню/список · ↑↓ · Enter · o сортировка · j задачи · Esc назад · q выход"
        : "↑↓ / Tab выбор · Enter · Esc назад · k отменить · q выход";
  }
}
