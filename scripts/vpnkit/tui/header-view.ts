import { BoxRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { connection, palette as p } from "./model";
import { cell } from "./formatting";

export class HeaderView {
  private headline: TextRenderable;
  private facts: TextRenderable;
  constructor(private renderer: CliRenderer, root: BoxRenderable) {
    this.headline = new TextRenderable(renderer, { content: "", fg: p.text, height: 1, flexShrink: 0 });
    this.headline.attributes = TextAttributes.BOLD;
    root.add(this.headline);
    this.facts = new TextRenderable(renderer, { content: "", fg: p.muted, height: 1, flexShrink: 0 });
    root.add(this.facts);
  }
  update(state: AppState, demo: boolean) {
    const width = Math.max(10, this.renderer.terminalWidth - 2),
      connectionState = connection(state.status);
    this.headline.content = cell(
      `◈ VPNKIT${demo ? " · ДЕМО" : ""}${state.nativeMode() ? (state.nativeInstalled() ? " · NATIVE" : " · NATIVE PREVIEW") : ""}    ${connectionState.title}`,
      width,
    );
    this.headline.fg = connectionState.color;
    const compactServers = state.screen === "servers" && state.inlineSpeed();
    this.facts.visible = !compactServers;
    this.facts.height = compactServers ? 0 : 1;
    const gateway = state.status?.gateway_state ?? state.status?.vpn_state;
    const gatewayNames: Record<string, string> = {
      healthy: "готов",
      absent: "не запущен",
      inactive: "не запущен",
      stopped: "остановлен",
      starting: "запускается",
      unhealthy: "ошибка",
      running: "проверяется",
    };
    this.facts.content = cell(
      state.nativeMode()
        ? `Native-служба: ${gatewayNames[gateway ?? ""] ?? "неизвестно"} · runtime: native · Режим: ${state.status?.routing_mode === "smart" ? "умный" : state.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}`
        : `Docker: ${gatewayNames[gateway ?? ""] ?? "неизвестно"} · Режим: ${state.status?.routing_mode === "smart" ? "умный" : state.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}`,
      width,
    );
  }
}
