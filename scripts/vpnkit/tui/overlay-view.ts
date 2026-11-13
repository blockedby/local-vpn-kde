import { BoxRenderable, InputRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { SummaryView } from "./summary-view";
import { MenuView } from "./menu-view";
import { EditorView } from "./editor-view";
import { palette as p } from "./model";
import { cell } from "./formatting";

/** Absolute overlay host. Other modal views can use the same positioning pattern. */
export class OverlayView {
  private dialog: BoxRenderable;
  private dialogSummary: SummaryView;
  private dialogMenu: MenuView;
  private dialogEditor: EditorView;
  private backdrop: BoxRenderable;
  private box: BoxRenderable;
  private text: TextRenderable;
  private paletteBox: BoxRenderable;
  private paletteInput: InputRenderable;
  private paletteRows: TextRenderable[] = [];
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.dialog = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 2, width: 54, height: 8, zIndex: 50, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, overflow: "hidden", flexDirection: "column", visible: false });
    root.add(this.dialog);
    this.dialogSummary = new SummaryView(renderer, this.dialog);
    this.dialogMenu = new MenuView(renderer, this.dialog, index => {
      if (!this.current || this.current.helpOpen) return;
      const state = this.current;
      index += this.listOffset;
      state.selected = index;
      const item = state.items()[index];
      const action = item && state.actions().find(candidate => candidate.keys.includes(item.key));
      if (action?.available) action.run();
      else if (action?.unavailableBehavior === "notice" && action.unavailableReason) state.notify(action.unavailableReason, true);
      state.paint();
    });
    this.dialogEditor = new EditorView(renderer, this.dialog, value => this.current?.edit(value));
    this.backdrop = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 0, width: "100%", height: 1, zIndex: 99, backgroundColor: p.base, visible: false });
    root.add(this.backdrop);
    this.box = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 2, width: 54, height: 10, zIndex: 100, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, title: " Помощь · ↑↓ прокрутка · Esc закрыть ", overflow: "hidden", visible: false });
    this.text = new TextRenderable(renderer, { content: "", fg: p.text, width: "100%", height: "100%" });
    this.box.add(this.text); root.add(this.box);
    this.paletteBox = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 2, width: 54, height: 12, zIndex: 110, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, title: " Команды · ↑↓ выбор · Esc закрыть ", overflow: "hidden", visible: false });
    this.paletteInput = new InputRenderable(renderer, { width: "100%", maxLength: 256, placeholder: "Название или клавиша", backgroundColor: p.panel, textColor: p.text, focusedBackgroundColor: p.edge });
    this.paletteBox.add(this.paletteInput);
    this.paletteInput.on("input", () => { if (!this.current?.paletteOpen) return; this.current.paletteQuery = this.paletteInput.value; this.current.paletteIndex = 0; this.current.paintPalette(); });
    for (let i = 0; i < 24; i++) { const row = new TextRenderable(renderer, { content: "", height: 1, flexShrink: 0 }); this.paletteBox.add(row); this.paletteRows.push(row); }
    root.add(this.paletteBox);
  }
  private current?: AppState;
  private listOffset = 0;
  updatePalette(state: AppState) {
    this.current = state;
    this.paletteBox.visible = state.paletteOpen;
    if (!state.paletteOpen) { this.paletteInput.blur(); return; }
    const width = Math.min(76, this.renderer.terminalWidth - 4), height = Math.min(18, this.renderer.terminalHeight - 4);
    this.paletteBox.width = width; this.paletteBox.height = height;
    this.paletteBox.left = Math.floor((this.renderer.terminalWidth - width) / 2); this.paletteBox.top = Math.floor((this.renderer.terminalHeight - height) / 2);
    if (this.paletteInput.value !== state.paletteQuery) this.paletteInput.value = state.paletteQuery;
    this.paletteInput.focus();
    const entries = state.filteredPaletteActions(), count = height - 3;
    const offset = Math.max(0, state.paletteIndex - count + 1);
    this.paletteRows.forEach((row, i) => {
      const entry = entries[offset + i]; row.visible = i < count && !!entry;
      if (!entry) return;
      const action = entry.action;
      const server = ["servers:select", "servers:single-speed", "servers:details"].includes(action.id) ? state.servers.find(s => s.server_id === state.serverID)?.display_name : undefined;
      row.content = cell(`${offset + i === state.paletteIndex ? "›" : " "} ${action.label}${server ? ` · ${server}` : ""}  [${action.key}]${action.available ? "" : ` · ${action.unavailableReason ?? "недоступно"}`}`, width - 2).trimEnd();
      row.fg = action.available ? (offset + i === state.paletteIndex ? p.accent : p.text) : p.muted;
    });
  }
  update(state: AppState) {
    this.current = state;
    this.updatePalette(state);
    const modal = isOverlay(state.screen) && this.renderer.terminalWidth >= 60 && this.renderer.terminalHeight >= 16;
    const editor = ["subscription", "target", "profile-name", "profile-source"].includes(state.screen);
    this.dialog.visible = modal;
    this.dialogEditor.update(state.editorValue, modal && editor && !state.helpOpen, state.closing, state.screen);
    if (modal) {
      const width = Math.min(64, this.renderer.terminalWidth - 4);
      this.dialogSummary.update(state);
      const summaryHeight = this.dialogSummary.fit(width - 2);
      const height = Math.min(this.renderer.terminalHeight - 4, Math.max(5, 2 + summaryHeight + (editor ? 3 : state.items().length)));
      if (state.screen === "details") {
        state.detailsOffset = Math.min(state.detailsOffset, Math.max(0, summaryHeight - (height - 2)));
        this.dialogSummary.update(state);
        this.dialogSummary.fit(width - 2, state.detailsOffset, height - 2);
      }
      this.dialog.width = width; this.dialog.height = height;
      this.dialog.left = Math.floor((this.renderer.terminalWidth - width) / 2);
      this.dialog.top = Math.floor((this.renderer.terminalHeight - height) / 2);
      this.dialog.title = ["confirm", "profile-remove", "cancel-confirm", "retry-confirm"].includes(state.screen) ? " Подтверждение " : state.screen === "autostart" ? " Автозапуск " : state.screen === "details" ? " Сервер " : state.screen === "tasks" ? " Задачи " : state.screen === "recovery" ? " Восстановление " : " Редактор ";
      const items = state.items();
      const availableRows = Math.max(1, height - 2 - summaryHeight);
      this.listOffset = Math.max(0, Math.min(Math.max(0, items.length - availableRows), state.selected - availableRows + 1));
      this.dialogMenu.update(items.slice(this.listOffset, this.listOffset + availableRows), editor, state.selected - this.listOffset, true, state.screen === "tasks");
    }
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
    const names: Record<string,string> = { global: "Общие", "server-table": "Серверы", menu: "Выбор", editor: "Редактор", confirmation: "Подтверждение", confirm: "Подтверждение", "profile-remove": "Подтверждение", "cancel-confirm": "Подтверждение", "retry-confirm": "Подтверждение", tasks: "Задачи", recovery: "Восстановление", details: "Сервер", servers: "Серверы", profiles: "Профили", diagnostics: "Диагностика", settings: "Настройки", autostart: "Автозапуск" };
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
