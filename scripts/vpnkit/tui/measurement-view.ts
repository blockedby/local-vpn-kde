import { BoxRenderable, StyledText, TextRenderable, t, type CliRenderer } from "@opentui/core";
import type { Server } from "./bridge";
import { cell } from "./formatting";
import { palette as p } from "./model";
import { speedometer } from "./speedometer";

interface MeasurementState {
  home: boolean;
  inline: boolean;
  servers: Server[];
  testing: boolean;
  speed?: number;
  speedAt: number;
  speedServer?: { id: string; name: string };
  speedLabel: string;
}

export class MeasurementView {
  private info: TextRenderable;
  private gauge: TextRenderable;

  constructor(private renderer: CliRenderer, parent: BoxRenderable) {
    this.info = new TextRenderable(renderer, { content: "", fg: p.muted, height: 2, flexShrink: 0 });
    parent.add(this.info);
    this.gauge = new TextRenderable(renderer, { content: "", fg: p.accent, height: 14, flexShrink: 0 });
    this.gauge.marginTop = 1;
    parent.add(this.gauge);
  }

  update(state: MeasurementState) {
    const width = Math.max(10, this.renderer.terminalWidth - 2);
    this.info.visible = state.home && state.inline && !!state.speedServer;
    this.info.height = this.info.visible ? 2 : 0;
    const measuredElsewhere = state.speedServer && state.servers.some(s => s.selected && s.server_id !== state.speedServer!.id);
    const timeLabel = state.speedAt ? new Date(state.speedAt).toLocaleTimeString("ru-RU", { hour12: false }) : state.testing ? "выполняется" : "не завершён";
    this.info.content = `Замер: ${timeLabel}${measuredElsewhere ? " · другой сервер" : ""}\n${cell(state.speedServer?.name ?? "", width).trimEnd()}`;
    const compactGauge = this.renderer.height < 25 || width < 29;
    this.gauge.visible = state.home && !state.inline;
    this.gauge.height = this.gauge.visible ? (compactGauge ? 4 : 11) + (state.speedAt ? 1 : 0) : 0;
    this.gauge.marginTop = this.gauge.visible ? 1 : 0;
    this.gauge.fg = state.speed === undefined ? p.accent : p.green;
    const dial = speedometer(state.speed, state.speed ?? 0, compactGauge);
    this.gauge.content = new StyledText([...dial.chunks,
      ...t`\n${cell(state.speedLabel, width).trimEnd()}${state.speedAt ? `\nПоследний замер: ${new Date(state.speedAt).toLocaleTimeString("ru-RU", { hour12: false })}` : ""}`.chunks,
    ]);
  }
}
