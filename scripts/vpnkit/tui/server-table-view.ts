import { BoxRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import type { CheckProgress, Server } from "./bridge";
import { cell } from "./formatting";
import { palette as p } from "./model";
import { frames } from "./animation-frames";

type BatchKind = "ping" | "speed" | "availability";
interface TableState {
  visible: boolean;
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
}

/** Owns the table renderables; selection and viewport remain application state. */
export class ServerTableView {
  private header: TextRenderable;
  private rows: TextRenderable[] = [];

  constructor(private renderer: CliRenderer, parent: BoxRenderable, select: (index: number) => void) {
    this.header = new TextRenderable(renderer, { content: "", fg: p.accent, height: 1, flexShrink: 0 });
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
    const width = Math.max(10, this.renderer.terminalWidth - 2);
    const nameWidth = Math.max(10, width - 45);
    this.header.visible = state.visible;
    this.header.content = `   ${cell("Сервер", nameWidth)} ${cell("Ping мс", 8)} ${cell("Мбит/с", 8)} ${cell("Сайт", 7)} Статус`;
    this.rows.forEach((row, i) => {
      const s = state.servers[i];
      row.visible = state.visible && i < state.rowCount && !!s;
      if (!s) return;
      const queued = !!batch && batch.ids?.includes(s.server_id) && !batch.running.has(s.server_id) && !batch.completed.has(s.server_id);
      const active = batch?.running.has(s.server_id) ?? false;
      const checking = (kind: BatchKind) => active && (batch?.kind === kind || (kind === "availability" && !!batch));
      const ping = queued ? "ждёт" : (checking("ping") || checking("speed")) && !batch?.pinged.has(s.server_id)
        ? frames[frame % 10] : s.ping_status === "failed" ? "ошибка" : (s.latency_ms?.toString() ?? "—");
      const speed = checking("speed") && batch?.stages.get(s.server_id) === "speed"
        ? liveSpeeds.has(s.server_id) ? `~${liveSpeeds.get(s.server_id)!.toFixed(1)}` : frames[frame % 10]
        : s.status === "failed" ? "ошибка" : (s.download_mbps?.toFixed(1) ?? "—");
      const site = queued ? "—" : checking("availability") && batch?.pinged.has(s.server_id) && s.ping_status === "ready" && !batch?.completed.has(s.server_id)
        ? frames[frame % 10] : s.availability === "ready" ? "да" : s.availability === "failed" ? "нет" : "—";
      row.content = `${s.server_id === state.serverID ? "›" : " "}${s.selected ? "●" : " "} ${cell(s.display_name, nameWidth)} ${cell(ping, 8)} ${cell(speed, 8)} ${cell(site, 7)} ${queued ? "в очереди" : active ? ({ start: "подготовка", ping: "сайт / ждёт", speed: "замер", complete: "готов" }[batch!.stages.get(s.server_id) ?? "start"]) : s.ping_status === "failed" || s.availability === "failed" || s.status === "failed" ? "ошибка" : s.availability === "ready" ? "готов" : s.status === "failed" ? "ошибка" : s.status === "ready" ? "готов" : "не пров."}`;
      row.fg = s.server_id === state.serverID ? p.accent : s.selected ? p.green : p.text;
      row.attributes = s.server_id === state.serverID ? TextAttributes.BOLD : 0;
    });
  }
}
