import { BoxRenderable, TextAttributes, TextRenderable, type CliRenderer } from "@opentui/core";
import { HOME_TILES, type AppState } from "./app-state";
import { cell, displayCheckAddress, pluralRu } from "./formatting";
import { connection, palette as p } from "./model";

const TILES = HOME_TILES.length;
const LAST = TILES - 1;

export class HomeView {
  private root: BoxRenderable;
  private rows: BoxRenderable[] = [];
  private columns = 0;
  private firstRow = 0;
  private more: TextRenderable;
  private tiles: { box: BoxRenderable; title: TextRenderable; lines: TextRenderable[] }[] = [];
  constructor(private renderer: CliRenderer, parent: BoxRenderable, activate: (index: number) => void) {
    this.root = new BoxRenderable(renderer, { flexDirection: "column", flexGrow: 1, minHeight: 0 }); parent.add(this.root);
    for (let i = 0; i < 6; i++) { const row = new BoxRenderable(renderer, { flexDirection: "row", flexShrink: 0 }); this.root.add(row); this.rows.push(row); }
    this.more = new TextRenderable(renderer, { content: "", height: 1, flexShrink: 0, fg: p.muted, visible: false });
    this.root.add(this.more);
    for (let i = 0; i < TILES; i++) {
      const box = new BoxRenderable(renderer, { flexDirection: "column", flexGrow: 1, flexBasis: 0, minWidth: 0, border: true, borderStyle: "rounded", onMouseDown: e => { if (e.button === 0) activate(i); } });
      const title = new TextRenderable(renderer, { height: 1, fg: p.text, attributes: TextAttributes.BOLD }); box.add(title);
      const lines = [0, 1, 2, 3].map(() => { const line = new TextRenderable(renderer, { height: 1, fg: p.muted }); box.add(line); return line; });
      this.tiles.push({ box, title, lines });
    }
  }
  update(state: AppState, visible: boolean) {
    this.root.visible = visible;
    if (!visible) return;
    const cols = state.homeColumns(), rowsTotal = Math.ceil(TILES / cols);
    // Full tiles take five rows, compact four, title-only three. Header, footer and
    // a two-line notice take four; when even title-only rows do not fit, scroll.
    const room = this.renderer.height - 2 - (state.noticeReserved() ? 2 : 0);
    const compact = this.renderer.height < rowsTotal * 5 + 5;
    const tileHeight = !compact ? 5 : rowsTotal * 4 <= room ? 4 : 3;
    const fitsAll = rowsTotal * tileHeight <= room;
    // A scrolled grid spends one row saying how many tiles are out of view.
    const visibleRows = fitsAll ? rowsTotal : Math.max(1, Math.min(rowsTotal, Math.floor((room - 1) / tileHeight)));
    const selectedRow = Math.floor(state.homeIndex / cols);
    if (selectedRow < this.firstRow) this.firstRow = selectedRow;
    if (selectedRow >= this.firstRow + visibleRows) this.firstRow = selectedRow - visibleRows + 1;
    this.firstRow = Math.max(0, Math.min(this.firstRow, rowsTotal - visibleRows));
    const above = this.firstRow * cols, below = Math.max(0, TILES - (this.firstRow + visibleRows) * cols);
    this.more.visible = !fitsAll;
    this.more.content = [above && `↑ ещё ${above}`, below && `↓ ещё ${below}`].filter(Boolean).join(" · ");
    const selected = state.catalogLoaded && !state.catalogStale ? state.servers.find(s => s.selected) : undefined;
    const best = state.catalogLoaded && !state.catalogStale ? state.servers.filter(s => s.ping_status === "ready" && s.latency_ms !== undefined && Number.isFinite(s.latency_ms)).sort((a, b) => a.latency_ms! - b.latency_ms!)[0] : undefined;
    const profile = state.profiles?.items.find(item => item.id === state.profiles?.active);
    const gateway = state.status?.gateway_state ?? state.status?.vpn_state;
    const health: Record<string, string> = { healthy: "готов", "egress-unavailable": "нет выхода в сеть", unhealthy: "ошибка", absent: "не запущен", inactive: "не запущен", stopped: "остановлен", starting: "запускается", running: "проверяется" };
    const mode = state.status?.routing_mode === "smart" ? "умный" : state.status?.routing_mode === "strict" ? "строгий" : "—";
    const actions = state.actions();
    const lifecycle = actions.find(a => ["home:connect", "home:start-service", "home:x"].includes(a.id));
    const lifecycleTask = [...state.tasks.values()].find(task => ["backend/start", "start", "disconnect", "stop"].includes(task.action));
    const glyphs = ["▶", "≡", "◇", "+", "*"];
    const data = [
      ["Подключение", lifecycleTask ? state.actionLabel(lifecycleTask.action) : connection(state.status).title, `Enter — ${lifecycle?.label ?? "—"}`],
      ["Серверы", `${state.catalogLoaded && !state.catalogStale ? `${state.servers.length} ${pluralRu(state.servers.length, ["сервер", "сервера", "серверов"])}` : "— серверов"} · выбран: ${selected?.display_name ?? "—"}`, `Лучший ping: ${best ? `${best.display_name} · ${best.latency_ms} мс` : "—"}`],
      ["Профили", state.profiles ? `${state.profiles.items.length} ${pluralRu(state.profiles.items.length, ["профиль", "профиля", "профилей"])}` : "Профилей: —", `Активен: ${profile?.name ?? "—"}`],
      ["Диагностика", `Шлюз: ${health[gateway ?? ""] ?? "—"}`, `Сеть: ${state.status?.networkmanager_active === "yes" ? "активна" : state.status?.networkmanager_active === "no" ? "отключена" : "—"}`],
      ["Настройки", `Режим: ${mode} · автозапуск: ${state.autostart ? (state.autostart.connect ? "VPN" : state.autostart.gateway ? "шлюз" : "нет") : "—"}`, `Сайт: ${state.target ? displayCheckAddress(state.target) : "не задан"}`],
    ];
    // The last row may hold fewer tiles, which then grow wider.
    const tileWidth = (i: number) => Math.floor((this.renderer.width - 2) / Math.min(cols, TILES - Math.floor(i / cols) * cols)) - 2;
    const width = tileWidth(LAST);
    // Preserve the redacted address and complete status when extra rows are available.
    const wrap = (text: string): string[] => {
      const lines: string[] = [];
      let line = "";
      for (const word of text.split(" ")) {
        const next = line ? `${line} ${word}` : word;
        if (Bun.stringWidth(next) <= width) { line = next; continue; }
        if (line) { lines.push(line); line = ""; }
        for (const part of new Intl.Segmenter().segment(word)) {
          if (Bun.stringWidth(line + part.segment) > width) { lines.push(line); line = ""; }
          line += part.segment;
        }
      }
      if (line) lines.push(line);
      return lines;
    };
    const settingsLines = compact ? [data[LAST]![1]!] : [...wrap(data[LAST]![1]!), ...wrap(data[LAST]![2]!)];
    const extra = compact ? 0 : Math.min(2, Math.max(0, this.renderer.height - (rowsTotal * 5) - 2), Math.max(0, settingsLines.length - 2));
    this.rows.forEach((row, i) => { row.visible = i >= this.firstRow && i < this.firstRow + visibleRows; row.height = compact ? tileHeight : 5 + (i === Math.floor(LAST / cols) ? extra : 0); });
    // Rebuild every row when the column count changes: appending only the moved
    // tiles would leave them after the ones that stayed, out of navigation order.
    if (cols !== this.columns) {
      this.columns = cols;
      for (const tile of this.tiles) tile.box.parent?.remove(tile.box);
      this.tiles.forEach((tile, i) => this.rows[Math.floor(i / cols)]!.add(tile.box));
    }
    this.tiles.forEach((tile, i) => {
      tile.box.height = compact ? tileHeight : 5 + (Math.floor(i / cols) === Math.floor(LAST / cols) ? extra : 0); tile.box.borderColor = i === state.homeIndex ? p.accent : p.edge;
      // Centred tiles: the highlighted border and title colour mark the selection, so no cursor glyph.
      // Letter shortcuts are not drawn; they live in the «Клавиши» window.
      const own = tileWidth(i);
      const center = (text: string) => { const fitted = cell(text, own).trimEnd(); return " ".repeat(Math.max(0, Math.floor((own - Bun.stringWidth(fitted)) / 2))) + fitted; };
      tile.title.content = center(`${glyphs[i]} ${data[i]![0]!}`);
      tile.title.fg = i === state.homeIndex ? p.accent : p.text;
      const lines = i === LAST && extra ? settingsLines : data[i]!.slice(1);
      tile.lines.forEach((line, n) => { line.visible = n < (compact ? tileHeight - 3 : 2 + (Math.floor(i / cols) === Math.floor(LAST / cols) ? extra : 0)) && n < lines.length; line.content = center(lines[n] ?? ""); });
    });
  }
}
