import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { AppState } from "./app-state";
import { palette as p } from "./model";

export const DETAILS_THRESHOLD = 120;
export const DETAILS_WIDTH = 36;

/** Only present values already published for the selected table row. */
export function detailsLines(state: AppState): string[] {
  const server = state.servers.find(row => row.server_id === state.serverID);
  if (!server) return ["Нет выбранного сервера."];
  const profile = state.profiles?.items.find(item => item.id === server.profile_id)?.name;
  const live = state.liveSpeeds.get(server.server_id);
  const time = state.measurementTimes.get(server.server_id);
  const active = state.batch?.running.has(server.server_id);
  const queued = state.batch?.ids?.includes(server.server_id) && !active && !state.batch.completed.has(server.server_id);
  const stage = state.batch?.stages.get(server.server_id);
  const status = queued ? "в очереди" : active ? ({ start: "подготовка", ping: "проверка ping", speed: "замер скорости", complete: "завершение" }[stage ?? "start"]) : state.homeTesting && state.homeSpeedServer?.id === server.server_id ? "замер скорости" : server.ping_status === "failed" || server.status === "failed" || server.availability === "failed" ? "ошибка проверки" : server.availability === "ready" || server.status === "ready" ? "готов" : "не проверен";
  const site = server.availability === "ready" ? "доступен" : server.availability === "failed" ? "недоступен" : "не проверен";
  const speed = live !== undefined ? `~${live.toFixed(1)} Мбит/с · сейчас` : server.download_mbps === undefined ? "нет данных" : `${server.download_mbps.toFixed(1)} Мбит/с${time ? ` · ${new Date(time).toLocaleString("ru-RU")}` : " · время неизвестно"}`;
  return [server.display_name, `Профиль: ${profile ?? "неизвестен"}`, `Выбран для VPN: ${server.selected ? "да" : "нет"}`, `Ping: ${server.ping_status === "failed" ? "ошибка" : server.latency_ms === undefined ? "нет данных" : `${server.latency_ms} мс`}`, `Скорость: ${speed}`, `Сайт: ${site}`, ...(server.availability === "ready" || server.availability === "failed" ? [`Адрес проверки: ${state.target}`] : []), `Состояние: ${status}`];
}

export class DetailsView {
  private box: BoxRenderable;
  private text: TextRenderable;
  constructor(renderer: CliRenderer, parent: BoxRenderable) {
    this.box = new BoxRenderable(renderer, { width: DETAILS_WIDTH, height: "100%", flexShrink: 0, border: true, borderStyle: "rounded", borderColor: p.edge, title: " Сервер ", paddingX: 1, overflow: "hidden" });
    this.text = new TextRenderable(renderer, { content: "", fg: p.text, width: "100%", height: "100%" });
    this.box.add(this.text); parent.add(this.box);
  }
  update(state: AppState, visible: boolean) {
    this.box.visible = visible;
    if (visible) this.text.content = detailsLines(state).join("\n");
  }
}
