import { BoxRenderable, fg, InputRenderable, TabSelectRenderable, TextTableRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { SummaryView } from "./summary-view";
import { MenuView } from "./menu-view";
import { EditorView } from "./editor-view";
import { MIN_COLS, MIN_ROWS, palette as p } from "./model";
import { cell, safeText, wrapText } from "./formatting";
import { serverMenuActions } from "./action-registry";

/** Absolute overlay host. Other modal views can use the same positioning pattern. */
export class OverlayView {
  private dialog: BoxRenderable;
  private dialogSummary: SummaryView;
  private dialogMenu: MenuView;
  private dialogEditor: EditorView;
  private backdrop: BoxRenderable;
  private box: BoxRenderable;
  private search: InputRenderable;
  private tabs: TabSelectRenderable;
  private keysTable: TextTableRenderable;
  private rowBackdrop: BoxRenderable;
  private rowMenu: BoxRenderable;
  private rowHeader: TextRenderable;
  private rowEntries: TextRenderable[] = [];
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.dialog = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 2, width: 54, height: 8, zIndex: 50, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, overflow: "hidden", flexDirection: "column", visible: false });
    root.add(this.dialog);
    this.rowBackdrop = new BoxRenderable(renderer, { position: "absolute", top: 0, left: 0, width: "100%", height: "100%", zIndex: 48, visible: false, onMouseDown: event => { if (event.button === 0) this.current?.closeServerMenu(); } });
    root.add(this.rowBackdrop);
    this.rowMenu = new BoxRenderable(renderer, { position: "absolute", top: 3, left: 2, width: 48, height: 7, zIndex: 49, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, overflow: "hidden", flexDirection: "column", visible: false });
    this.rowMenu.onMouseDown = event => event.stopPropagation();
    this.rowHeader = new TextRenderable(renderer, { content: "", height: 1, fg: p.muted, flexShrink: 0 }); this.rowMenu.add(this.rowHeader);
    for (let i = 0; i < 5; i++) {
      const row = new TextRenderable(renderer, { content: "", height: 1, flexShrink: 0, onMouseDown: event => {
        if (event.button !== 0 || !this.current) return;
        event.stopPropagation(); const state = this.current; state.serverMenuIndex = i;
        const action = serverMenuActions(state)[i];
        if (action?.available) action.run(); else if (action?.unavailableReason) state.notify(action.unavailableReason, true);
        state.paint();
      } });
      this.rowMenu.add(row); this.rowEntries.push(row);
    }
    root.add(this.rowMenu);
    this.dialogSummary = new SummaryView(renderer, this.dialog);
    this.dialogMenu = new MenuView(renderer, this.dialog, index => {
      if (!this.current || this.current.helpOpen) return;
      const state = this.current;
      index += this.listOffset;
      if (state.screen === "tasks") { const mapped = this.taskRows[index]; if (mapped === undefined || mapped < 0) return; index = mapped; }
      state.selected = index;
      const item = state.items()[index];
      const action = item && state.actions().find(candidate => candidate.keys.includes(item.key));
      if (action?.available) action.run();
      else if (action?.unavailableBehavior === "notice" && action.unavailableReason) state.notify(action.unavailableReason, true);
      state.paint();
    }, "overlay");
    this.dialogEditor = new EditorView(renderer, this.dialog, value => this.current?.edit(value));
    this.backdrop = new BoxRenderable(renderer, { position: "absolute", top: 0, left: 0, width: "100%", height: "100%", zIndex: 99, visible: false });
    root.add(this.backdrop);
    this.box = new BoxRenderable(renderer, { position: "absolute", zIndex: 100, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, title: " Клавиши ", overflow: "hidden", visible: false });
    root.add(this.box);
    this.search = new InputRenderable(renderer, { width: "100%", maxLength: 256, placeholder: "/ Поиск по действию или клавише", backgroundColor: p.panel, textColor: p.text, focusedBackgroundColor: p.edge,
      onMouseDown: () => { if (this.current?.helpOpen) { this.current.searchFocused = true; this.current.paint(); } } });
    this.search.on("input", () => { if (!this.current?.helpOpen || this.current.paletteQuery === this.search.value) return; this.current.paletteQuery = this.search.value; this.current.paletteIndex = 0; this.current.paintPalette(); });
    this.box.add(this.search);
    this.tabs = new TabSelectRenderable(renderer, { options: ["Этот экран", "Общие", "Все"].map(name => ({ name, description: "" })), tabWidth: 12, height: 1, showDescription: false, showUnderline: false, showScrollArrows: false, selectedBackgroundColor: p.edge, selectedTextColor: p.accent, textColor: p.muted });
    this.box.add(this.tabs);
    this.keysTable = new TextTableRenderable(renderer, { width: "100%", columnWidthMode: "full", columnFitter: "balanced", cellPadding: 0, columnGap: 1, border: false, wrapMode: "none", selectable: false });
    this.keysTable.focusable = true;
    this.box.add(this.keysTable);
  }
  private current?: AppState;
  private listOffset = 0;
  private taskRows: number[] = [];
  updatePalette(state: AppState) {
    this.current = state;
    const visible = state.helpOpen && this.renderer.width >= MIN_COLS && this.renderer.height >= MIN_ROWS;
    this.box.visible = this.backdrop.visible = visible;
    if (!visible) { this.search.blur(); this.keysTable.blur(); return; }
    const width = Math.min(104, this.renderer.terminalWidth - 2), height = state.keysPageSize() + 5;
    this.box.width = width; this.box.height = height;
    this.box.left = Math.floor((this.renderer.terminalWidth - width) / 2); this.box.top = Math.floor((this.renderer.terminalHeight - height - 2) / 2);
    if (this.search.value !== state.paletteQuery) this.search.value = state.paletteQuery;
    if (state.searchFocused) this.search.focus(); else this.keysTable.focus();
    this.tabs.setSelectedIndex(state.keysTab);
    const entries = state.filteredPaletteActions(), count = state.keysPageSize();
    state.paletteIndex = Math.max(0, Math.min(state.paletteIndex, entries.length - 1));
    const offset = Math.floor(state.paletteIndex / count) * count;
    const rows = entries.slice(offset, offset + count).map(({ action }, index) => {
      const selected = offset + index === state.paletteIndex;
      const color = selected ? p.accent : action.available ? p.text : p.muted;
      return [[fg(color)(`${selected ? "›" : " "} ${action.keyHints.join("/") || "—"}`)], [fg(color)(safeText(action.label))], [fg(p.muted)(safeText(action.unavailableReason ?? ""))]];
    });
    if (!rows.length) rows.push([[], [fg(p.muted)("Ничего не найдено")], []]);
    this.keysTable.content = [["Клавиша", "Действие", "Статус"].map(title => [fg(p.accent)(title)]), ...rows];
  }
  update(state: AppState) {
    this.current = state;
    this.updatePalette(state);
    const rowVisible = state.serverMenu && !state.helpOpen && this.renderer.terminalWidth >= MIN_COLS && this.renderer.terminalHeight >= MIN_ROWS;
    this.rowBackdrop.visible = rowVisible; this.rowMenu.visible = rowVisible;
    if (rowVisible) {
      const width = Math.min(50, this.renderer.terminalWidth - 4);
      const entries = serverMenuActions(state);
      const target = state.serverMenuTarget;
      const server = state.servers.find(s => s.server_id === target?.id && s.display_name === target.name);
      const title = `${safeText(target?.name ?? "Сервер")} · ${server?.latency_ms === undefined ? "—" : Math.round(server.latency_ms)} мс · ${server?.download_mbps === undefined ? "—" : server.download_mbps.toFixed(1)} Мбит/с`;
      this.rowHeader.content = cell(` ${title}`, width - 2).trimEnd();
      let height = 3;
      this.rowEntries.forEach((row, i) => {
        const action = entries[i]; row.visible = !!action; if (!action) return;
        const hint = action.key === "shift+t" ? "T" : action.key === "return" ? "Enter" : action.key;
        const label = `${i === state.serverMenuIndex ? "›" : " "} ${action.label}`;
        const padding = Math.max(1, width - 4 - Bun.stringWidth(label) - Bun.stringWidth(hint));
        const reason = action.unavailableReason && i === state.serverMenuIndex ? wrapText(action.unavailableReason, width - 4).map(line => `  ${line}`) : [];
        row.content = `${label}${" ".repeat(padding)}${hint}${reason.length ? `\n${reason.join("\n")}` : ""}`;
        row.height = 1 + reason.length; height += 1 + reason.length;
        row.fg = action.available ? i === state.serverMenuIndex ? p.accent : p.text : p.muted;
      });
      this.rowMenu.width = width; this.rowMenu.height = Math.min(height, this.renderer.terminalHeight - 3);
      this.rowMenu.left = Math.max(1, this.renderer.terminalWidth - width - 3);
      this.rowMenu.top = Math.min(3, this.renderer.terminalHeight - Number(this.rowMenu.height) - 1);
    }
    const modal = isOverlay(state.screen) && this.renderer.terminalWidth >= MIN_COLS && this.renderer.terminalHeight >= MIN_ROWS;
    const editor = ["subscription", "target", "profile-name", "profile-source", "server-rename"].includes(state.screen);
    this.dialog.visible = modal;
    this.dialogEditor.update(state.editorValue, modal && editor && !state.helpOpen, state.closing, state.screen);
    if (modal) {
      const width = Math.min(64, this.renderer.terminalWidth - 4);
      this.dialogSummary.update(state);
      const summaryHeight = this.dialogSummary.fit(width - 2);
      const items = state.items();
      let displayItems: { key: string; label: string; id?: string }[] = items.map(item => ({ key: item.key, label: item.label, id: item.id }));
      this.taskRows = [];
      if (state.screen === "tasks") {
        displayItems = [];
        const running = [...state.tasks.values()].filter(task => task.action !== "status").length;
        let at = 0;
        for (const [title, count] of [["Выполняются", running], ["В очереди", state.queue.length], ["Завершены", state.history.length]] as const) {
          if (!count) continue;
          displayItems.push({ key: "", label: title }); this.taskRows.push(-1);
          for (let i = 0; i < count; i++, at++) { const item = items[at]; if (item) { displayItems.push({ key: item.key, label: item.label, id: item.id }); this.taskRows.push(at); } }
        }
      }
      const height = Math.min(this.renderer.terminalHeight - 4, Math.max(5, 2 + summaryHeight + (editor ? 3 : displayItems.length)));
      if (state.screen === "details") {
        state.detailsOffset = Math.min(state.detailsOffset, Math.max(0, summaryHeight - (height - 2)));
        this.dialogSummary.update(state);
        this.dialogSummary.fit(width - 2, state.detailsOffset, height - 2);
      }
      this.dialog.width = width; this.dialog.height = height;
      this.dialog.left = Math.floor((this.renderer.terminalWidth - width) / 2);
      this.dialog.top = Math.floor((this.renderer.terminalHeight - height) / 2);
      this.dialog.title = state.screen === "server-rename" ? ` ${safeText(state.renameTarget?.original ?? "Сервер")} ` : ["confirm", "profile-remove", "cancel-confirm", "retry-confirm"].includes(state.screen) ? " Подтверждение " : state.screen === "autostart" ? " Автозапуск " : state.screen === "details" ? " Сервер " : state.screen === "tasks" ? " Задачи " : state.screen === "recovery" ? " Восстановление " : " Редактор ";
      const availableRows = Math.max(1, height - 2 - summaryHeight);
      const selected = state.screen === "tasks" ? Math.max(0, this.taskRows.indexOf(state.selected)) : state.selected;
      this.listOffset = Math.max(0, Math.min(Math.max(0, displayItems.length - availableRows), selected - availableRows + 1));
      this.dialogMenu.update(displayItems.slice(this.listOffset, this.listOffset + availableRows), editor, selected - this.listOffset, true, state.screen === "tasks");
    }
  }
}
