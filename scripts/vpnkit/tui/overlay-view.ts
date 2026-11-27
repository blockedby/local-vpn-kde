import { BoxRenderable, InputRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { isOverlay, type AppState } from "./app-state";
import { SummaryView } from "./summary-view";
import { MenuView, menuHeight } from "./menu-view";
import { EditorView } from "./editor-view";
import { MIN_COLS, MIN_ROWS, palette as p } from "./model";
import { cell, safeText, wrapText } from "./formatting";
import { serverMenuActions } from "./action-registry";
import { detailsLines } from "./details-view";

/** Absolute overlay host. Other modal views can use the same positioning pattern. */
export class OverlayView {
  private dialog: BoxRenderable;
  private dialogSummary: SummaryView;
  private dialogMenu: MenuView;
  private dialogEditor: EditorView;
  private backdrop: BoxRenderable;
  private box: BoxRenderable;
  private search: InputRenderable;
  private keysRows: TextRenderable[] = [];
  private rowBackdrop: BoxRenderable;
  private rowMenu: BoxRenderable;
  private rowHeader: TextRenderable;
  private rowEntries: TextRenderable[] = [];
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.dialog = new BoxRenderable(renderer, { position: "absolute", top: 2, left: 2, width: 54, height: 8, zIndex: 50, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, overflow: "hidden", flexDirection: "column", visible: false });
    root.add(this.dialog);
    this.rowBackdrop = new BoxRenderable(renderer, { position: "absolute", top: 0, left: 0, width: "100%", height: "100%", zIndex: 48, visible: false, onMouseDown: event => { if (event.button === 0) this.current?.closeServerMenu(); } });
    root.add(this.rowBackdrop);
    this.rowMenu = new BoxRenderable(renderer, { position: "absolute", top: 3, left: 2, width: 48, height: 7, zIndex: 49, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, title: " Сервер ", overflow: "hidden", flexDirection: "column", visible: false });
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
    this.dialogMenu = new MenuView(renderer, this.dialog, index => this.current?.activateMenuRow(index), "overlay");
    this.dialogEditor = new EditorView(renderer, this.dialog, value => this.current?.edit(value));
    this.backdrop = new BoxRenderable(renderer, { position: "absolute", top: 0, left: 0, width: "100%", height: "100%", zIndex: 99, visible: false });
    root.add(this.backdrop);
    this.box = new BoxRenderable(renderer, { position: "absolute", zIndex: 100, border: true, borderStyle: "rounded", borderColor: p.accent, backgroundColor: p.panel, title: " Клавиши ", bottomTitleAlignment: "left", overflow: "hidden", flexDirection: "column", visible: false });
    root.add(this.box);
    this.search = new InputRenderable(renderer, { width: "100%", flexShrink: 0, maxLength: 256, placeholder: "действие или клавиша", backgroundColor: p.panel, textColor: p.text, focusedBackgroundColor: p.edge,
      onMouseDown: () => { if (this.current?.helpOpen) { this.current.searchFocused = true; this.current.paint(); } } });
    this.search.on("input", () => { if (!this.current?.helpOpen || this.current.paletteQuery === this.search.value) return; const flipped = !this.current.paletteQuery !== !this.search.value; this.current.paletteQuery = this.search.value; this.current.paletteIndex = 0; if (flipped) this.current.paint(); else this.current.paintPalette(); });
    this.box.add(this.search);
    for (let i = 0; i < 24; i++) { const row = new TextRenderable(renderer, { content: "", height: 1, flexShrink: 0 }); this.box.add(row); this.keysRows.push(row); }
  }
  private current?: AppState;
  private keysOffset = 0;
  updatePalette(state: AppState) {
    this.current = state;
    const visible = state.helpOpen && this.renderer.width >= MIN_COLS && this.renderer.height >= MIN_ROWS;
    this.box.visible = this.backdrop.visible = visible;
    if (!visible) { this.search.blur(); return; }
    const entries = state.filteredPaletteActions();
    state.paletteIndex = Math.max(0, Math.min(state.paletteIndex, entries.length - 1));
    // One list: a section header before each change of section, then its rows.
    type Line = { header: string } | { index: number };
    const lines: Line[] = [];
    entries.forEach((entry, index) => { if (index && entries[index - 1]!.section !== entry.section) lines.push({ header: entry.section }); lines.push({ index }); });
    const keyWidth = Math.min(8, Math.max(1, ...entries.map(({ action }) => Bun.stringWidth(action.keyHints.join("/") || "—"))));
    const searching = state.searchFocused || !!state.paletteQuery;
    // Fixed size while open, so typing a query does not make the window jump.
    const width = Math.min(56, this.renderer.terminalWidth - 4);
    const height = Math.min(this.keysRows.length, state.keysPageSize()) + 2;
    const count = height - 2 - (searching ? 1 : 0);
    this.box.width = width; this.box.height = height;
    this.box.left = Math.floor((this.renderer.terminalWidth - width) / 2); this.box.top = Math.max(1, Math.floor((this.renderer.terminalHeight - height - 1) / 2));
    const here = state.keysSectionName();
    this.box.title = cell(` Клавиши · ${state.paletteQuery ? "поиск" : here} `, width - 14).trimEnd() + (searching ? " " : " · / поиск ");
    this.search.visible = searching;
    if (this.search.value !== state.paletteQuery) this.search.value = state.paletteQuery;
    if (state.searchFocused) this.search.focus(); else this.search.blur();
    const at = lines.findIndex(line => "index" in line && line.index === state.paletteIndex);
    if (at >= 0 && at < this.keysOffset) this.keysOffset = at > 0 && "header" in lines[at - 1]! ? at - 1 : at;
    if (at >= this.keysOffset + count) this.keysOffset = at - count + 1;
    const top = this.keysOffset = Math.max(0, Math.min(this.keysOffset, lines.length - count));
    this.keysRows.forEach((row, i) => {
      const line = lines[top + i]; row.visible = i < count;
      if (i >= count) return;
      if (!entries.length) { row.content = i ? "" : "  Ничего не найдено"; row.fg = p.muted; return; }
      if (!line) { row.content = ""; return; }
      if ("header" in line) { const label = `── ${line.header} `; row.content = label + "─".repeat(Math.max(0, width - 2 - Bun.stringWidth(label))); row.fg = p.muted; return; }
      const { action } = entries[line.index]!, selected = line.index === state.paletteIndex;
      const keys = action.keyHints.join("/") || "—";
      row.content = cell(`${selected ? "›" : " "} ${Bun.stringWidth(keys) > keyWidth ? keys : cell(keys, keyWidth)}  ${safeText(action.label)}`, width - 2).trimEnd();
      row.fg = selected ? p.accent : action.available ? p.text : p.muted;
    });
    const chosen = entries[state.paletteIndex]?.action;
    this.box.bottomTitle = chosen && !chosen.available ? cell(` ${safeText(chosen.unavailableReason ?? "Недоступно.")} `, width - 4).trimEnd() + " " : undefined;
  }
  /** Geometry shared by the server row menu and the server details: right side, fixed size. */
  private serverPanel(state: AppState) {
    const width = Math.min(50, this.renderer.terminalWidth - 4);
    const menuHeight = 3 + serverMenuActions(state).length;
    const detailsHeight = 2 + detailsLines(state).flatMap(line => wrapText(line, width - 2)).length;
    const height = Math.min(this.renderer.terminalHeight - 3, Math.max(menuHeight, detailsHeight));
    return { width, height, left: Math.max(1, this.renderer.terminalWidth - width - 3), top: Math.max(0, Math.min(3, this.renderer.terminalHeight - height - 1)) };
  }
  update(state: AppState) {
    this.current = state;
    this.updatePalette(state);
    const rowVisible = state.serverMenu && !state.helpOpen && this.renderer.terminalWidth >= MIN_COLS && this.renderer.terminalHeight >= MIN_ROWS;
    this.rowBackdrop.visible = rowVisible; this.rowMenu.visible = rowVisible;
    if (rowVisible) {
      const { width, left, top, height: panelHeight } = this.serverPanel(state);
      const entries = serverMenuActions(state);
      const target = state.serverMenuTarget;
      const server = state.servers.find(s => s.server_id === target?.id && s.display_name === target.name);
      const title = `${safeText(target?.name ?? "Сервер")} · ${server?.latency_ms === undefined ? "—" : Math.round(server.latency_ms)} мс · ${server?.download_mbps === undefined ? "—" : server.download_mbps.toFixed(1)} Мбит/с`;
      this.rowHeader.content = cell(` ${title}`, width - 2).trimEnd();
      let height = 3;
      this.rowEntries.forEach((row, i) => {
        const action = entries[i]; row.visible = !!action; if (!action) return;
        const label = `${i === state.serverMenuIndex ? "›" : " "} ${action.label}`;
        const reason = action.unavailableReason && i === state.serverMenuIndex ? wrapText(action.unavailableReason, width - 4).map(line => `  ${line}`) : [];
        row.content = `${label}${reason.length ? `\n${reason.join("\n")}` : ""}`;
        row.height = 1 + reason.length; height += 1 + reason.length;
        row.fg = action.available ? i === state.serverMenuIndex ? p.accent : p.text : p.muted;
      });
      // The row menu and «Подробности сервера» share one panel: same place, same size.
      this.rowMenu.width = width; this.rowMenu.height = Math.max(panelHeight, Math.min(height, this.renderer.terminalHeight - 3));
      this.rowMenu.left = left; this.rowMenu.top = Math.min(top, this.renderer.terminalHeight - Number(this.rowMenu.height) - 1);
    }
    const modal = isOverlay(state.screen) && this.renderer.terminalWidth >= MIN_COLS && this.renderer.terminalHeight >= MIN_ROWS;
    const editor = ["subscription", "target", "profile-name", "profile-source", "server-rename"].includes(state.screen);
    this.dialog.visible = modal;
    this.dialogEditor.update(state.editorValue, modal && editor && !state.helpOpen, state.closing, state.screen);
    if (modal) {
      const width = Math.min(64, this.renderer.terminalWidth - 4);
      this.dialogSummary.update(state);
      const items = state.items();
      const listHeight = menuHeight(items);
      // A confirmation must show its consequence; on a tiny terminal it may cover the header and footer.
      const confirmation = ["confirm", "profile-remove", "retry-confirm", "cancel-confirm"].includes(state.screen);
      const maxHeight = this.renderer.terminalHeight - (confirmation && this.renderer.terminalHeight < 16 ? 0 : 4);
      const summaryLimit = editor ? Math.max(0, maxHeight - 2 - 3) : listHeight ? Math.max(1, maxHeight - 2 - Math.min(listHeight, 2)) : Infinity;
      const summaryHeight = Math.min(summaryLimit, this.dialogSummary.fit(width - 2, 0, summaryLimit));
      const height = Math.min(maxHeight, Math.max(5, 2 + summaryHeight + (editor ? 3 : listHeight)));
      if (state.screen === "details") {
        state.detailsOffset = Math.min(state.detailsOffset, Math.max(0, summaryHeight - (height - 2)));
        this.dialogSummary.update(state);
        this.dialogSummary.fit(width - 2, state.detailsOffset, height - 2);
      }
      this.dialog.width = width; this.dialog.height = height;
      this.dialog.left = Math.floor((this.renderer.terminalWidth - width) / 2);
      this.dialog.top = Math.floor((this.renderer.terminalHeight - height) / 2);
      if (state.screen === "details") {
        const panel = this.serverPanel(state);
        this.dialog.width = panel.width; this.dialog.height = panel.height; this.dialog.left = panel.left; this.dialog.top = panel.top;
        state.detailsOffset = Math.min(state.detailsOffset, Math.max(0, this.dialogSummary.fit(panel.width - 2) - (panel.height - 2)));
        this.dialogSummary.update(state); this.dialogSummary.fit(panel.width - 2, state.detailsOffset, panel.height - 2);
      }
      this.dialog.title = state.screen === "server-rename" ? ` ${safeText(state.renameTarget?.original ?? "Сервер")} ` : ["confirm", "profile-remove", "cancel-confirm", "retry-confirm"].includes(state.screen) ? " Подтверждение " : state.screen === "autostart" ? " Автозапуск " : state.screen === "details" ? " Сервер " : state.screen === "tasks" ? " Задачи " : state.screen === "recovery" ? " Восстановление " : " Редактор ";
      const availableRows = Math.max(1, height - 2 - summaryHeight);
      this.dialogMenu.update(items, editor, state.selected, width - 2, availableRows);
    }
  }
}
