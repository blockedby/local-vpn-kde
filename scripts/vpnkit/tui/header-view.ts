import { BoxRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { connection, palette as p } from "./model";
import { cell } from "./formatting";

export class HeaderView {
  private line: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.line = new TextRenderable(renderer, { content: "", fg: p.text, height: 1, flexShrink: 0, attributes: TextAttributes.BOLD });
    root.add(this.line);
  }
  update(state: AppState, demo: boolean) {
    const width = this.renderer.terminalWidth - 2;
    const status = connection(state.status);
    const server = state.servers.find(s => s.selected);
    const mode = state.status?.routing_mode === "smart" ? "умный" : state.status?.routing_mode === "strict" ? "строгий" : "неизвестно";
    const stale = state.checked && Date.now() - state.checked > 15000 ? " · устарел" : "";
    const symbol = status.title === "VPN подключён" ? "●" : status.title === "VPN отключён" ? "○" : "◌";
    const runtime = state.nativePreviewOnly() ? "NATIVE PREVIEW" : state.status?.runtime === "native" ? "NATIVE" : state.status ? "Docker" : "?";
    const title = status.title === "VPN подключён" ? "подключён" : status.title === "VPN отключён" ? "отключён" : status.title;
    const core = `◈ VPNKIT · ${runtime}${demo ? " · ДЕМО" : ""} ${symbol} ${title}${stale}`;
    const live = server && state.liveSpeeds.get(server.server_id);
    const ping = server?.latency_ms === undefined ? undefined : `${server.latency_ms} мс`;
    const speed = live !== undefined ? `~${live.toFixed(1)} Мбит/с` : server?.download_mbps === undefined ? undefined : `${server.download_mbps.toFixed(1)} Мбит/с`;
    const measured = state.homeTesting && state.homeSpeedServer ? state.servers.find(s => s.server_id === state.homeSpeedServer!.id) : undefined;
    const measuredLive = measured && state.liveSpeeds.get(measured.server_id);
    const activeSpeed = measured && measuredLive !== undefined
      ? measured.server_id === server?.server_id ? `~${measuredLive.toFixed(1)} Мбит/с` : `тест ${cell(measured.display_name, 12).trimEnd()}: ~${measuredLive.toFixed(1)}`
      : live !== undefined ? `~${live.toFixed(1)} Мбит/с` : undefined;
    const name = server?.display_name ?? "сервер неизвестен";
    const available = Math.max(1, width - Bun.stringWidth(core) - 3 - (activeSpeed ? Bun.stringWidth(activeSpeed) + 3 : 0));
    const parts = [cell(name, available).trimEnd(), ...(activeSpeed ? [activeSpeed] : []), `режим: ${mode}`];
    const at = server && state.measurementTimes.get(server.server_id);
    const age = at ? `${Math.floor((Date.now() - at) / 60000)} мин` : undefined;
    for (const part of [ping, activeSpeed ? undefined : speed]) if (part) parts.push(part);
    let result = core;
    for (const part of parts) {
      if (Bun.stringWidth(`${result} · ${part}`) > width) break;
      result += ` · ${part}`;
    }
    if (speed && age && result.includes(speed) && Bun.stringWidth(`${result} · ${age}`) <= width) result += ` · ${age}`;
    this.line.content = cell(result, Math.max(1, width)).trimEnd();
    this.line.fg = status.color;
  }
}
