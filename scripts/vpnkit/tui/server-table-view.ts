import { BoxRenderable, InputRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import type { CheckProgress, Server } from "./bridge";
import { cell } from "./formatting";
import { palette as p } from "./model";
import { frames } from "./animation-frames";
import type { AppState } from "./app-state";

type BatchKind = "ping" | "speed" | "availability";
interface TableState {
  visible: boolean;
  width: number;
  servers: Server[];
  rowCount: number;
  serverID?: string;
  batch?: {
    kind: BatchKind;
    ids?: string[];
    running: Set<string>;
    completed: Set<string>;
    pinged: Set<string>;
    stages: Map<string, CheckProgress["stage"]>;
  };
  frame: number;
  liveSpeeds: Map<string, number>;
  sort: AppState["sort"];
  sortDescending: boolean;
  filter: string;
  filterEditing: boolean;
  total: number;
  shown: number;
}

/** Owns the table renderables; selection and viewport remain application state. */
export class ServerTableView {
  private header: TextRenderable;
  private filterLine: BoxRenderable;
  private filterInput: InputRenderable;
  private filterCount: TextRenderable;
  private rows: TextRenderable[] = [];

  constructor(private renderer: CliRenderer, parent: BoxRenderable, select: (index: number) => void, editFilter: (value: string) => void, sort: (column: AppState["sort"]) => void) {
    this.filterLine = new BoxRenderable(renderer, { height: 1, flexShrink: 0, flexDirection: "row", visible: false });
    this.filterLine.add(new TextRenderable(renderer, { content: "Фильтр: ", fg: p.accent, height: 1 }));
    this.filterInput = new InputRenderable(renderer, { flexGrow: 1, maxLength: 256, placeholder: "имя сервера", backgroundColor: p.base, textColor: p.text, focusedBackgroundColor: p.edge });
    this.filterLine.add(this.filterInput);
    this.filterCount = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 });
    this.filterLine.add(this.filterCount); parent.add(this.filterLine);
    this.filterInput.on("input", () => editFilter(this.filterInput.value));
    this.header = new TextRenderable(renderer, { content: "", fg: p.accent, height: 1, flexShrink: 0 });
    this.header.onMouseDown = event => { if (event.button !== 0) return; const x = event.x - this.header.x; const width = Math.max(10, this.header.width); const showStatus = width >= 78, showSite = width >= 65; const nameWidth = Math.max(10, width - (showStatus ? 48 : showSite ? 33 : 24)); sort(x < nameWidth + 5 ? "name" : x < nameWidth + 15 ? "ping" : x < nameWidth + 25 ? "speed" : "availability"); };
    parent.add(this.header);
    for (let i = 0; i < 100; i++) {
      const row = new TextRenderable(renderer, { content: "", fg: p.text, height: 1, flexShrink: 0 });
      row.onMouseDown = (event) => { if (event.button === 0) select(i); };
      parent.add(row);
      this.rows.push(row);
    }
  }

  update(state: TableState) {
    const { batch, frame, liveSpeeds } = state;
    const width = Math.max(10, state.width);
    const showStatus = width >= 78;
    const showSite = width >= 65;
    const nameWidth = Math.max(10, width - (showStatus ? 48 : showSite ? 33 : 24));
    this.filterLine.visible = state.visible && (!!state.filter || state.filterEditing);
    if (this.filterInput.value !== state.filter) this.filterInput.value = state.filter;
    this.filterCount.content = ` ${state.shown}/${state.total}`;
    if (state.filterEditing && state.visible) this.filterInput.focus(); else this.filterInput.blur();
    this.header.visible = state.visible;
    const right = (value: string, columns: number) => cell(value, columns).trimEnd().padStart(columns);
    const arrow = state.sortDescending ? "↓" : "↑";
    const nameLabel = `Сервер${state.sort === "name" ? ` · имя ${arrow}` : state.sort === "availability" && !showSite ? ` · сайт ${arrow}` : ""}`;
    this.header.content = `   ${cell(nameLabel, nameWidth)}  ${right(`Ping мс${state.sort === "ping" ? arrow : ""}`, 8)}  ${right(`Мбит/с${state.sort === "speed" ? arrow : ""}`, 8)}${showSite ? `  ${cell(`Сайт${state.sort === "availability" ? ` ${arrow}` : ""}`, 7)}` : ""}${showStatus ? "  Статус" : ""}`;
    this.rows.forEach((row, i) => {
      const s = state.servers[i];
      row.visible = state.visible && i < state.rowCount && !!s;
      if (!s) return;
      const queued = !!batch && batch.ids?.includes(s.server_id) && !batch.running.has(s.server_id) && !batch.completed.has(s.server_id);
      const active = batch?.running.has(s.server_id) ?? false;
      const checking = (kind: BatchKind) => active && (batch?.kind === kind || (kind === "availability" && !!batch));
      const ping = queued ? "ждёт" : (checking("ping") || checking("speed")) && !batch?.pinged.has(s.server_id)
        ? frames[frame % 10] : s.ping_status === "failed" ? "ошибка" : (s.latency_ms?.toString() ?? "—");
      const speed = liveSpeeds.has(s.server_id) ? `~${liveSpeeds.get(s.server_id)!.toFixed(1)}` : checking("speed") && batch?.stages.get(s.server_id) === "speed"
        ? liveSpeeds.has(s.server_id) ? `~${liveSpeeds.get(s.server_id)!.toFixed(1)}` : frames[frame % 10]
        : s.status === "failed" ? "ошибка" : (s.download_mbps?.toFixed(1) ?? "—");
      const site = queued ? "—" : checking("availability") && batch?.pinged.has(s.server_id) && s.ping_status === "ready" && !batch?.completed.has(s.server_id)
        ? frames[frame % 10] : s.availability === "ready" ? "да" : s.availability === "failed" ? "нет" : "—";
      row.content = `${s.server_id === state.serverID ? "›" : " "}${s.selected ? "●" : " "} ${cell(s.display_name, nameWidth)}  ${right(ping, 8)}  ${right(speed, 8)}${showSite ? `  ${cell(site, 7)}` : ""}${showStatus ? `  ${queued ? "в очереди" : active ? ({ start: "подготовка", ping: "сайт / ждёт", speed: "замер", complete: "готов" }[batch!.stages.get(s.server_id) ?? "start"]) : s.ping_status === "failed" || s.availability === "failed" || s.status === "failed" ? "ошибка" : s.availability === "ready" ? "готов" : s.status === "failed" ? "ошибка" : s.status === "ready" ? "готов" : "не проверен"}` : ""}`;
      row.fg = s.server_id === state.serverID ? p.accent : s.selected ? p.green : p.text;
      row.attributes = s.server_id === state.serverID ? TextAttributes.BOLD : 0;
    });
  }
}
