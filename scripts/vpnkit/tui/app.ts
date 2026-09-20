import {
  BoxRenderable,
  TextRenderable,
  InputRenderable,
  TextAttributes,
  type CliRenderer,
  type KeyEvent,
} from "@opentui/core";
import type { Action, Backend, Reply, Server, Status } from "./bridge";
import { connection, failure, palette as p } from "./model";

import { StyledText, t } from "@opentui/core";
import { speedometer } from "./speedometer";

type Screen =
  | "home"
  | "servers"
  | "subscription"
  | "diagnostics"
  | "target"
  | "confirm";
type Batch = "ping" | "speed" | "availability";
type Item = { key: string; label: string; run: () => void };
const names: Partial<Record<Action, string>> = {
  "backend/start": "Запускаем Docker",
  start: "Подключаем VPN",
  disconnect: "Отключаем VPN",
  stop: "Останавливаем Docker",
  "toggle-mode": "Меняем режим",
  subscription: "Сохраняем подписку",
  "subscription/read": "Читаем подписку",
  diagnostics: "Проверяем компоненты",
  "servers/list": "Загружаем серверы",
  "servers/refresh": "Обновляем каталог",
  "servers/select": "Применяем сервер",
  "servers/ping": "Ping",
  "servers/speed": "Тест скорости",
  "servers/availability": "Доступность сайта",
};
const phases: Record<string, string> = {
  preparing: "Подготовка",
  prepared: "Подготовка завершена",
  render: "Конфигурация",
  "compose-up": "Запуск контейнера",
  "compose-up-done": "Контейнер запущен",
  "nm-work": "Подключение OpenVPN",
  "host-smoke": "Проверка соединения",
  "nm-disconnect": "Отключение OpenVPN",
  "compose-down": "Остановка контейнера",
  "runtime-wait": "Ожидание готовности",
  committing: "Сохранение состояния",
  committed: "Готово",
  compensating: "Восстановление состояния",
};
const frames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const safeText = (s: string) =>
  s.replace(/[\x00-\x1f\x7f-\x9f\u202a-\u202e\u2066-\u2069]/g, "");
export function cell(text: string, width: number) {
  let out = "";
  for (const part of new Intl.Segmenter().segment(safeText(text))) {
    if (Bun.stringWidth(out + part.segment) > width) break;
    out += part.segment;
  }
  if (out !== safeText(text) && width > 1) {
    while (Bun.stringWidth(out) > width - 1)
      out = Array.from(out).slice(0, -1).join("");
    out += "…";
  }
  return out + " ".repeat(Math.max(0, width - Bun.stringWidth(out)));
}

export class App {
  private status?: Status;
  private screen: Screen = "home";
  private busy?: Action;
  private queued?: { action: Action; value?: string };
  private batch?: {
    kind: Batch;
    done: number;
    total: number;
    id?: string;
    ids?: string[];
    failed: number;
    target?: string;
    running: Set<string>;
    pinged: Set<string>;
    completed: Set<string>;
    failures: Set<string>;
  };
  private cancelled = false;
  private closing = false;
  private disposed = false;
  private frame = 0;
  private started = 0;
  private checked = 0;
  private phase = "";
  private notice = "";
  private noticeAttempt = "";
  private error = false;
  private selected = 0;
  private serverMenu = false;
  private confirmation: "disconnect" | "stop" = "disconnect";
  private serverID?: string;
  private servers: Server[] = [];
  private catalogLoaded = false;
  private catalogStale = false;
  private sort: "name" | "ping" | "speed" | "availability" = "name";
  private target = "https://www.youtube.com/";
  private editorRevision = 0;
  private navigationRevision = 0;
  private loadedSubscription = false;
  private pendingSubscriptionRevision?: number;
  private draft = "";
  private timer?: ReturnType<typeof setInterval>;
  private poll?: ReturnType<typeof setInterval>;
  private root: BoxRenderable;
  private headline: TextRenderable;
  private facts: TextRenderable;
  private content: BoxRenderable;
  private summary: TextRenderable;
  private controls: BoxRenderable;
  private controlRows: BoxRenderable[] = [];
  private controlLabels: TextRenderable[] = [];
  private tableHeader: TextRenderable;
  private rows: TextRenderable[] = [];
  private input: InputRenderable;
  private editorHint: TextRenderable;
  private progress: TextRenderable;
  private notification: TextRenderable;
  private footer: TextRenderable;

  private gauge: TextRenderable;
  private homeTesting = false;
  private homeSpeed?: number;
  private homePing?: number;
  private homeSpeedAt = 0;
  private homeSpeedLabel = "[t] Проверить скорость выбранного сервера";

  constructor(
    private renderer: CliRenderer,
    private backend: Backend,
    private demo = false,
  ) {
    this.root = new BoxRenderable(renderer, {
      width: "100%",
      height: "100%",
      paddingX: 1,
      flexDirection: "column",
      backgroundColor: p.base,
    });
    renderer.root.add(this.root);
    const text = (
      parent: BoxRenderable,
      content = "",
      color = p.text,
      height = 1,
    ) => {
      const t = new TextRenderable(renderer, {
        content,
        fg: color,
        height,
        flexShrink: 0,
      });
      parent.add(t);
      return t;
    };
    this.headline = text(this.root);
    this.headline.attributes = TextAttributes.BOLD;
    this.facts = text(this.root, "", p.muted);
    this.content = new BoxRenderable(renderer, {
      flexDirection: "column",
      flexGrow: 1,
      minHeight: 0,
      overflow: "hidden",
      marginTop: 1,
    });
    this.root.add(this.content);
    this.summary = text(this.content, "", p.muted, 2);
    this.summary.minHeight = 0;
    this.controls = new BoxRenderable(renderer, {
      flexDirection: "column",
      flexShrink: 0,
    });
    this.content.add(this.controls);
    for (let i = 0; i < 8; i++) {
      const row = new BoxRenderable(renderer, {
        height: 1,
        flexShrink: 0,
        onMouseDown: (e) => {
          if (e.button === 0) {
            this.serverMenu = true;
            this.selected = i;
            this.items()[i]?.run();
            this.paint();
          }
        },
      });
      this.controls.add(row);
      this.controlRows.push(row);
      this.controlLabels.push(text(row));
    }
    this.gauge = text(this.content, "", p.accent, 14);
    this.gauge.marginTop = 1;
    this.tableHeader = text(this.content, "", p.accent);
    for (let i = 0; i < 100; i++) {
      const row = text(this.content);
      row.onMouseDown = (e) => {
        if (e.button === 0) {
          const server = this.visibleServers()[i];
          if (server) {
            this.serverMenu = false;
            this.serverID = server.server_id;
            this.paint();
          }
        }
      };
      this.rows.push(row);
    }
    this.input = new InputRenderable(renderer, {
      width: "100%",
      maxLength: 16384,
      placeholder: "https://",
      backgroundColor: p.panel,
      textColor: p.text,
      focusedBackgroundColor: p.edge,
    });
    this.content.add(this.input);
    this.input.on("input", () => {
      const clean = safeText(this.input.value);
      if (clean !== this.input.value) this.input.value = clean;
      this.editorRevision++;
      if (this.screen === "subscription") this.draft = this.input.value;
    });
    this.editorHint = text(this.content, "", p.muted, 2);
    this.progress = text(this.root, "", p.accent);
    this.notification = text(this.root, "", p.muted, 2);
    this.footer = text(this.root, "", p.muted);
    backend.onProgress = (phase) => {
      this.phase = phases[phase] ?? "";
      this.paint();
    };
    backend.onCheckProgress = (row) => {
      const batch = this.batch;
      if (!batch || batch.kind !== "ping" || this.cancelled || this.closing || !batch.ids?.includes(row.server_id) || batch.completed.has(row.server_id)) return;
      const server = this.servers.find(s => s.server_id === row.server_id);
      if (!server) return;
      batch.running.add(row.server_id);
      if (row.stage === "start") {
        server.ping_status = "untested";
        server.latency_ms = undefined;
        server.availability = "untested";
        this.paint();
        return;
      }
      server.ping_status = row.ping_status;
      server.latency_ms = row.ping_status === "ready" ? row.latency_ms : undefined;
      batch.pinged.add(row.server_id);
      if (row.stage === "complete") {
        if (batch.target === this.target) server.availability = row.availability;
        batch.running.delete(row.server_id);
        batch.completed.add(row.server_id);
        if (row.ping_status !== "ready" || row.availability !== "ready") batch.failures.add(row.server_id);
        batch.done = batch.completed.size;
        batch.failed = batch.failures.size;
      }
      this.paint();
    };
    renderer.keyInput.on("keypress", this.onKey);
    renderer.on("resize", this.paint);
    this.paint();
  }
  start() {
    this.timer = setInterval(() => {
      this.frame++;
      this.paint();
    }, 100);
    this.poll = setInterval(() => {
      if (!this.busy && !this.batch && !this.closing)
        void this.perform("status");
    }, 5000);
    void this.perform("status");
  }
  private ready() {
    return (this.status?.gateway_state ?? this.status?.vpn_state) === "healthy";
  }
  private async loadPendingSubscription() {
    if (this.busy || this.batch || this.closing || this.disposed) return;
    const revision = this.pendingSubscriptionRevision;
    this.pendingSubscriptionRevision = undefined;
    if (revision !== undefined && revision === this.editorRevision &&
        this.screen === "subscription" && !this.loadedSubscription)
      await this.perform("subscription/read");
  }
  private notify(text: string, error = false) {
    this.notice = text;
    this.noticeAttempt = "";
    this.error = error;
    this.paint();
  }
  private open(screen: Screen) {
    if (this.closing) return;
    if (this.screen === "subscription") this.draft = this.input.value;
    this.screen = screen;
    this.selected = 0;
    this.serverMenu = false;
    this.navigationRevision++;
    if (screen === "subscription") {
      this.input.value = this.draft;
      if (!this.loadedSubscription) {
        this.pendingSubscriptionRevision = this.editorRevision;
        void this.loadPendingSubscription();
      }
    }
    if (screen === "target") this.input.value = this.target;
    this.paint();
    if (
      screen === "servers" &&
      this.ready() &&
      this.status?.subscription === "configured" &&
      !this.catalogLoaded &&
      !this.busy &&
      !this.batch
    )
      void this.perform(this.catalogStale ? "servers/refresh" : "servers/list");
    // Diagnostics immediately displays the cached evidence. Refresh is explicit.
  }
  private confirm(action: "disconnect" | "stop") {
    this.confirmation = action;
    this.open("confirm");
  }
  private items(): Item[] {
    if (this.screen === "confirm")
      return [
        { key: "n", label: "Нет", run: () => this.open("home") },
        {
          key: "y",
          label: "Да",
          run: () => {
            const action = this.confirmation;
            this.open("home");
            void this.perform(action);
          },
        },
      ];
    const run = (action: Action) => () => void this.perform(action);
    if (this.screen === "home")
      return [
        ...(!this.ready()
          ? [{ key: "s", label: "Запустить Docker", run: run("backend/start") }]
          : []),
        ...(this.ready()
          ? [
              this.status?.networkmanager_active === "yes"
                ? {
                    key: "x",
                    label: "Отключить VPN",
                    run: () => this.confirm("disconnect"),
                  }
                : {
                    key: "s",
                    label: "Подключить VPN",
                    run: () => {
                      if (this.status?.subscription !== "configured") {
                        this.open("subscription");
                        this.notify("Добавьте подписку перед подключением.");
                      } else void this.perform("start");
                    },
                  },
            ]
          : []),
        { key: "t", label: this.inlineSpeed() ? `Тест скорости · ${this.homeTesting ? `${frames[this.frame % 10]} ${this.busy === "servers/ping" ? "ping" : this.busy === "servers/speed" ? "замер" : "подготовка"} · ` : ""}${this.homePing === undefined ? "—" : Math.round(this.homePing)}ms, ${this.homeSpeed === undefined ? "—" : (this.homeSpeed / 8.388608).toFixed(1)} MiB/s` : "Тест скорости", run: () => void this.testHomeSpeed() },
        { key: "v", label: "Серверы", run: () => this.open("servers") },
        { key: "c", label: "Подписка", run: () => this.open("subscription") },
        { key: "d", label: "Диагностика", run: () => this.open("diagnostics") },
        {
          key: "m",
          label: `Режим: ${this.status?.routing_mode === "smart" ? "умный" : "строгий"}`,
          run: () => {
            if (this.ready()) void this.perform("toggle-mode");
            else this.notify("Смена режима доступна после запуска Docker.");
          },
        },
        ...(["healthy", "unhealthy", "running", "stopped", "starting"].includes(
          this.status?.gateway_state ?? "",
        )
          ? [
              {
                key: "z",
                label: "Остановить Docker и VPN",
                run: () => this.confirm("stop"),
              },
            ]
          : []),
      ];
    if (this.screen === "diagnostics")
      return [
        { key: "u", label: "Обновить диагностику", run: run("diagnostics") },
      ];
    if (this.screen === "servers") {
      if (!this.ready())
        return [
          { key: "s", label: "Запустить Docker", run: run("backend/start") },
        ];
      if (this.status?.subscription !== "configured")
        return [
          {
            key: "c",
            label: "Добавить подписку",
            run: () => this.open("subscription"),
          },
        ];
      return [
        {
          key: "p",
          label: "Ping всех + сайт",
          run: () => void this.runBatch("ping"),
        },
        {
          key: "t",
          label: "Скорость всех · 3 с/сервер",
          run: () => void this.runBatch("speed"),
        },
        { key: "e", label: "Адрес сайта", run: () => this.open("target") },
        {
          key: "r",
          label: "Обновить подписку и список",
          run: run("servers/refresh"),
        },
      ];
    }
    return [];
  }
  private onKey = (key: KeyEvent) => {
    if (key.eventType === "release") return;
    const editor = this.screen === "subscription" || this.screen === "target";
    if (key.name === "escape") {
      key.preventDefault();
      this.open(editor && this.screen === "target" ? "servers" : "home");
      return;
    }
    if (key.ctrl && key.name === "c") {
      key.preventDefault();
      void this.close();
      return;
    }
    if (editor) {
      if (key.name === "return") {
        key.preventDefault();
        if (this.screen === "subscription")
          void this.perform("subscription", this.input.value.trim());
        else {
          try {
            const u = new URL(this.input.value.trim());
            if (
              u.protocol !== "https:" ||
              u.username ||
              u.password ||
              u.href.length > 2048
            )
              throw new Error();
            this.target = u.href;
            this.servers.forEach((s) => (s.availability = "untested"));
            this.open("servers");
            this.notify("Адрес сайта сохранён.");
          } catch {
            this.notify("Нужен HTTPS-адрес без логина и пароля.", true);
          }
        }
      }
      return;
    }
    if (key.name === "q") {
      void this.close();
      return;
    }
    if (this.closing) return;
    if (key.name === "k") {
      this.cancel();
      return;
    }
    const screenKeys: Record<string, Screen> = {
      h: "home",
      v: "servers",
      c: "subscription",
      d: "diagnostics",
    };
    if (screenKeys[key.name]) {
      key.preventDefault();
      this.open(screenKeys[key.name]);
      return;
    }
    if (this.screen === "servers") {
      if (key.name === "a") {
        void this.runBatch("ping");
        return;
      }
      if (key.name === "tab" && this.servers.length) {
        this.serverMenu = !this.serverMenu;
        this.paint();
        return;
      }
      if (key.name === "o") {
        const sorts = ["name", "ping", "speed", "availability"] as const;
        this.sort = sorts[(sorts.indexOf(this.sort) + 1) % sorts.length];
        this.paint();
        return;
      }
      if (
        !this.serverMenu &&
        this.servers.length &&
        (key.name === "up" ||
          key.name === "down" ||
          key.name === "pageup" ||
          key.name === "pagedown")
      ) {
        const list = this.sortedServers();
        const at = Math.max(
          0,
          list.findIndex((s) => s.server_id === this.serverID),
        );
        const step = key.name.startsWith("page") ? this.rowCount() : 1;
        this.serverID =
          list[
            Math.max(
              0,
              Math.min(
                list.length - 1,
                at + (["up", "pageup"].includes(key.name) ? -step : step),
              ),
            )
          ]?.server_id;
        this.paint();
        return;
      }
      if (key.name === "return" && !this.serverMenu && this.servers.length) {
        if (this.catalogStale) {
          this.notify("Подписка изменена. Обновите список серверов.", true);
          return;
        }
        const s = this.servers.find((s) => s.server_id === this.serverID);
        if (s) void this.perform("servers/select", s.server_id);
        return;
      }
    }
    const items = this.items();
    if (key.name === "up" || (key.name === "tab" && key.shift))
      this.selected =
        (this.selected + items.length - 1) % Math.max(1, items.length);
    else if (key.name === "down" || key.name === "tab")
      this.selected = (this.selected + 1) % Math.max(1, items.length);
    else if (key.name === "return") items[this.selected]?.run();
    else if (key.name === "u")
      void this.perform(
        this.screen === "diagnostics" ? "diagnostics" : "status",
      );
    else {
      const item = items.find((i) => i.key === key.name);
      if (item) {
        key.preventDefault();
        item.run();
      }
    }
    this.paint();
  };
  private sortedServers() {
    const compare = (a: Server, b: Server) =>
      a.display_name.localeCompare(b.display_name, "ru", { numeric: true }) ||
      a.server_id.localeCompare(b.server_id);
    return [...this.servers].sort((a, b) => {
      if (this.sort === "ping")
        return (
          (a.latency_ms ?? Infinity) - (b.latency_ms ?? Infinity) ||
          compare(a, b)
        );
      if (this.sort === "speed")
        return (
          (b.download_mbps ?? -1) - (a.download_mbps ?? -1) || compare(a, b)
        );
      if (this.sort === "availability")
        return (
          Number(b.availability === "ready") -
            Number(a.availability === "ready") || compare(a, b)
        );
      return compare(a, b);
    });
  }
  private rowCount() {
    return Math.max(1, Math.min(100, this.renderer.terminalHeight - 15));
  }
  private visibleServers() {
    const list = this.sortedServers();
    const at = Math.max(
      0,
      list.findIndex((s) => s.server_id === this.serverID),
    );
    const start = Math.floor(at / this.rowCount()) * this.rowCount();
    return list.slice(start, start + this.rowCount());
  }
  private updateCatalog(reply: Reply, action: Action) {
    if (action === "servers/check-batch" && reply.catalog?.servers) {
      for (const row of reply.catalog.servers) {
        const at = this.servers.findIndex((s) => s.server_id === row.server_id);
        if (at >= 0)
          this.servers[at] = {
            ...this.servers[at],
            ping_status: row.ping_status,
            latency_ms: row.latency_ms,
            availability:
              this.batch?.target === this.target
                ? row.availability
                : this.servers[at].availability,
          };
      }
    } else if (reply.catalog?.servers) {
      this.servers = reply.catalog.servers.map((s) => ({
        ...s,
        // Persisted catalog failures are not measurements from this UI session.
        status: "untested",
        ping_status: "untested",
        latency_ms: undefined,
        download_mbps: undefined,
        download_seconds: undefined,
        downloaded_bytes: undefined,
        availability: "untested",
      }));
      this.catalogLoaded = true;
      this.catalogStale = false;
    }
    const row = reply.catalog?.server;
    if (row) {
      const at = this.servers.findIndex((s) => s.server_id === row.server_id);
      if (at >= 0) {
        const previous = this.servers[at];
        // A ping/site reply also contains cached speed fields. Update only
        // the measurement requested, so old failures cannot reappear.
        if (action === "servers/speed")
          this.servers[at] = {
            ...previous,
            status: row.status,
            download_mbps: row.download_mbps,
            download_seconds: row.download_seconds,
            downloaded_bytes: row.downloaded_bytes,
          };
        else if (action === "servers/ping")
          this.servers[at] = {
            ...previous,
            ping_status: row.ping_status,
            latency_ms: row.latency_ms,
          };
        else if (
          action === "servers/availability" &&
          this.batch?.target === this.target
        )
          this.servers[at] = { ...previous, availability: row.availability };
      }
    }
    if (!this.servers.some((s) => s.server_id === this.serverID))
      this.serverID = this.sortedServers()[0]?.server_id;
  }
  async perform(
    action: Action,
    value?: string,
    insideBatch = false,
  ): Promise<Reply | undefined> {
    if (this.closing || this.disposed) return;
    if (((this.batch || this.homeTesting) && !insideBatch) || this.busy) {
      if (
        this.busy === "status" &&
        action !== "status" &&
        !this.queued &&
        !this.batch
      ) {
        this.queued = { action, value };
        return;
      }
      if (action !== "status")
        this.notify(
          "Другая операция выполняется. Можно сменить экран или отменить её [k].",
        );
      return;
    }
    if (action === "subscription" && (!value || !/^https?:\/\//.test(value))) {
      this.notify("Введите ссылку подписки.", true);
      return;
    }
    this.busy = action;
    this.started = Date.now();
    this.phase = "";
    const revision = this.editorRevision,
      navigation = this.navigationRevision;
    this.paint();
    let reply: Reply | undefined;
    try {
      reply = await this.backend.request(action, value);
      if (!action.startsWith("servers/") && action !== "subscription/read") {
        this.status = reply.status;
        this.checked = Date.now();
      }
      const previousMeasurements = this.homeTesting && action === "servers/list"
        ? new Map(this.servers.map(row => [row.server_id, row])) : undefined;
      this.updateCatalog(reply, action);
      if (previousMeasurements) this.servers = this.servers.map(row => {
        const previous = previousMeasurements.get(row.server_id);
        return previous ? { ...row, status: previous.status, ping_status: previous.ping_status,
          latency_ms: previous.latency_ms, availability: previous.availability,
          download_mbps: previous.download_mbps, download_seconds: previous.download_seconds,
          downloaded_bytes: previous.downloaded_bytes } : row;
      });
      if (action === "subscription/read" && reply.ok) {
        this.loadedSubscription = true;
        if (revision === this.editorRevision) {
          this.draft = reply.value ?? "";
          if (this.screen === "subscription") this.input.value = this.draft;
        }
      }
      if (!reply.ok && !insideBatch) {
        this.notice = failure(reply.reason, reply.code);
        this.noticeAttempt = [
          "start",
          "backend/start",
          "stop",
          "disconnect",
          "toggle-mode",
          "diagnostics",
        ].includes(action)
          ? (reply.status.last_attempt ?? "")
          : "";
        this.error = true;
      } else if (
        reply.ok &&
        action !== "status" &&
        action !== "subscription/read" &&
        !insideBatch
      ) {
        const done: Partial<Record<Action, string>> = {
          "backend/start": "Docker готов. Можно проверять серверы.",
          start: "",
          disconnect: "VPN отключён. Docker работает.",
          stop: "Docker и VPN остановлены.",
          "servers/list": "Список загружен.",
          "servers/refresh": "Каталог обновлён. Результаты проверок сброшены.",
          "servers/select": "Сервер выбран.",
          subscription: "Подписка сохранена. Обновите список серверов.",
          diagnostics: "Состояние компонентов обновлено.",
          "toggle-mode": "Режим изменён.",
        };
        this.notice = done[action] ?? "Готово.";
        this.noticeAttempt = "";
        this.error = false;
        if (action === "subscription") {
          this.catalogStale = true;
          this.catalogLoaded = false;
          this.servers = [];
          this.serverID = undefined;
          this.loadedSubscription = true;
          if (revision === this.editorRevision) {
            this.draft = value ?? "";
            if (navigation === this.navigationRevision) this.open("home");
          }
        }
        if (action === "servers/select" && value)
          this.servers.forEach((s) => (s.selected = s.server_id === value));
      }
    } catch {
      if (!action.startsWith("servers/")) this.status = undefined;
      this.notify(failure("backend-unavailable", null), true);
    } finally {
      this.busy = undefined;
      this.paint();
      if (this.closing && !this.batch) await this.finishClose();
      else if (this.queued) {
        const queued = this.queued;
        this.queued = undefined;
        await this.perform(queued.action, queued.value);
      } else if (
        action === "backend/start" &&
        reply?.ok &&
        this.screen === "servers" &&
        this.status?.subscription === "configured" &&
        !this.catalogLoaded &&
        !this.batch
      ) {
        await this.perform(
          this.catalogStale ? "servers/refresh" : "servers/list",
        );
      }
    }
    await this.loadPendingSubscription();
    return reply;
  }
  private inlineSpeed() {
    return this.renderer.width < 70 || this.renderer.height < 25;
  }
  private async testHomeSpeed() {
    if (this.busy || this.batch || this.homeTesting) return;
    if (!this.ready()) { this.notify("Сначала запустите Docker.", true); return; }
    this.homeTesting = true;
    this.cancelled = false;
    this.homeSpeed = undefined;
    this.homePing = undefined;
    this.notice = "";
    this.noticeAttempt = "";
    this.error = false;
    this.homeSpeedLabel = "Определяем выбранный сервер…";
    try {
      const list = await this.perform("servers/list", undefined, true);
      if (this.cancelled || this.closing) return;
      if (!list?.ok) throw new Error("Не удалось загрузить серверы.");
      const server = list.catalog?.servers?.find(row => row.selected);
      if (!server) throw new Error("Сначала выберите сервер в списке.");
      this.homeSpeedLabel = `Ping · ${server.display_name}`;
      const ping = await this.perform("servers/ping", server.server_id, true);
      if (this.cancelled || this.closing) return;
      if (!ping?.ok || ping.catalog?.server?.ping_status !== "ready") throw new Error("Сервер недоступен: ping не прошёл.");
      this.homePing = ping.catalog?.server?.latency_ms;
      this.homeSpeedLabel = `Измеряем · ${server.display_name} · 3 с`;
      const result = await this.perform("servers/speed", server.server_id, true);
      if (this.cancelled || this.closing) return;
      const mbps = result?.catalog?.server?.download_mbps;
      if (!result?.ok || mbps === undefined || !Number.isFinite(mbps) || mbps < 0) throw new Error("Не удалось измерить скорость.");
      this.homeSpeed = mbps;
      this.homeSpeedAt = Date.now();
      this.homeSpeedLabel = `${server.display_name} · средняя скорость`;
    } catch (error) {
      this.homeSpeedLabel = error instanceof Error ? error.message : "Ошибка измерения.";
      this.notify(this.homeSpeedLabel, true);
    } finally {
      if (this.cancelled) this.homeSpeedLabel = "Тест отменён";
      this.homeTesting = false;
      this.paint();
    }
  }

  private async runBatch(kind: Batch) {
    if (this.busy || this.batch || this.homeTesting) {
      this.notify("Дождитесь завершения или отмените текущую операцию [k].");
      return;
    }
    if (!this.ready()) {
      this.notify("Сначала запустите Docker.", true);
      return;
    }
    this.cancelled = false;
    this.notice = "";
    this.noticeAttempt = "";
    this.error = false;
    // Batch owns the command lane, including discovery, so navigation cannot enqueue mutations.
    this.batch = {
      kind,
      done: 0,
      total: this.servers.length,
      failed: 0,
      target: this.target,
      running: new Set(),
      pinged: new Set(), completed: new Set(), failures: new Set(),
    };
    if (!this.catalogLoaded || this.catalogStale) {
      const result = await this.perform(
        this.catalogStale ? "servers/refresh" : "servers/list",
        undefined,
        true,
      );
      if (!result?.ok) {
        this.batch = undefined;
        this.notify(
          result?.reason === "backend-outdated"
            ? failure("backend-outdated", null)
            : "Не удалось загрузить серверы. Проверьте подписку и Docker.",
          true,
        );
        if (this.closing) await this.finishClose();
        return;
      }
    }
    const ids = this.sortedServers().map((s) => s.server_id);
    this.batch.total = ids.length;
    const target = this.batch.target!;
    let stoppedReason: string | undefined;
    const groupSize = kind === "speed" ? 1 : Math.max(1, ids.length);
    for (let offset = 0; offset < ids.length; offset += groupSize) {
      const group = ids.slice(offset, offset + groupSize);
      const id = group[0];
      if (this.cancelled || this.closing) break;
      this.batch.id = id;
      this.batch.ids = group;
      let result: Reply | undefined;
      if (kind === "speed") {
        // A fresh TCP ping gates each download; cached reachability is not enough.
        this.servers = this.servers.map((server) => server.server_id === id
          ? { ...server, status: "untested", download_mbps: undefined,
              download_seconds: undefined, downloaded_bytes: undefined }
          : server);
        result = await this.perform("servers/ping", id, true);
        if (this.cancelled || this.closing) break;
        if (result?.ok && result.catalog?.server?.ping_status === "ready") {
          this.batch.pinged.add(id!);
          result = await this.perform("servers/speed", id, true);
        } else if (result?.ok) {
          result = { ...result, ok: false, reason: "failed" };
        }
      } else {
        result = await this.perform(
          "servers/check-batch", JSON.stringify({ ids: group, url: target }), true,
        );
      }
      if (this.cancelled) break;
      if (kind === "speed") {
        this.batch.done++;
        if (!result?.ok) this.batch.failed++;
      } else {
        const measured = result?.catalog?.servers ?? [];
        for (const row of measured) {
          this.batch.completed.add(row.server_id);
          if (row.ping_status !== "ready" || row.availability !== "ready") this.batch.failures.add(row.server_id);
        }
        this.batch.done = this.batch.completed.size;
        this.batch.failed = this.batch.failures.size;
      }
      this.paint();
      if (
        !result ||
        (kind !== "speed" && !result.ok) ||
        [
          "unavailable",
          "stale",
          "recovery_required",
          "backend-outdated",
        ].includes(result.reason)
      ) {
        stoppedReason = result?.reason ?? "unavailable";
        this.cancelled = true;
        break;
      }
    }
    const { done, total, failed } = this.batch;
    this.batch = undefined;
    this.notify(
      stoppedReason
        ? `${failure(stoppedReason, null)} Проверено: ${done}/${total}.`
        : `${this.cancelled || this.closing ? "Остановлено" : "Завершено"}: ${done}/${total} · ошибок ${failed}. ${kind !== "speed" && (this.cancelled || this.closing) ? "Показаны полученные результаты; проход не сохранён." : "Результаты сохранены."}`,
      failed > 0 || !!stoppedReason,
    );
    if (this.closing) await this.finishClose();
    else await this.loadPendingSubscription();
  }
  private cancel() {
    if (!this.busy && !this.batch) return;
    if (
      this.busy === "subscription" ||
      this.busy === "subscription/read" ||
      this.busy === "status"
    ) {
      this.notify(
        "Короткая операция завершается; переход между экранами доступен.",
      );
      return;
    }
    this.cancelled = true;
    this.backend.cancel?.();
    this.notify(
      "Отмена запрошена. Ожидаем завершения и восстановления состояния.",
    );
  }
  private paint = () => {
    if (this.disposed) return;
    const width = Math.max(10, this.renderer.terminalWidth - 2),
      state = connection(this.status);
    this.headline.content = cell(
      `◈ VPNKIT${this.demo ? " · ДЕМО" : ""}    ${state.title}`,
      width,
    );
    this.headline.fg = state.color;
    const gateway = this.status?.gateway_state ?? this.status?.vpn_state;
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
      `Docker: ${gatewayNames[gateway ?? ""] ?? "неизвестно"} · Режим: ${this.status?.routing_mode === "smart" ? "умный" : "строгий"}`,
      width,
    );
    const editor = this.screen === "subscription" || this.screen === "target";
    this.input.visible = editor;
    this.editorHint.visible = editor;
    if (editor && !this.closing) this.input.focus();
    else this.input.blur();
    this.editorHint.content =
      "Enter сохранить · Esc назад · Ctrl+U очистить\nЗначение видно только здесь; в журнал не попадает.";
    const items = this.items();
    this.controls.visible = !editor;
    this.controlRows.forEach((row, i) => {
      row.visible = i < items.length;
      row.backgroundColor =
        i === this.selected &&
        (this.screen !== "servers" || this.serverMenu || !this.servers.length)
          ? p.edge
          : p.base;
      this.controlLabels[i].content = items[i]
        ? cell(`[${items[i].key}] ${items[i].label}`, width).trimEnd()
        : "";
    });
    this.summary.height = this.screen === "diagnostics" ? 6 : 2;
    if (this.screen === "confirm")
      this.summary.content =
        this.confirmation === "disconnect"
          ? "Точно хотите отключить VPN?"
          : "Точно хотите остановить Docker и отключить VPN?";
    if (this.screen === "home")
      this.summary.content = !this.status
        ? "Проверяем состояние…"
        : !this.ready()
          ? "Подписку можно настроить до запуска Docker."
          : this.status.subscription !== "configured"
            ? "Добавьте подписку, затем откройте серверы."
            : "";
    this.summary.visible = this.screen !== "home" || !!this.summary.content;
    if (!this.summary.visible) this.summary.height = 0;
    const compactGauge = this.renderer.height < 25 || width < 29;
    this.gauge.visible = this.screen === "home" && !this.inlineSpeed();
    this.gauge.height = this.gauge.visible ? (compactGauge ? 4 : 11) + (this.homeSpeedAt ? 1 : 0) : 0;
    this.gauge.marginTop = this.gauge.visible ? 1 : 0;
    this.gauge.fg = this.homeSpeed === undefined ? p.accent : p.green;
    const ease = Math.min(1, (Date.now() - this.homeSpeedAt) / 450);
    const dial = speedometer(this.homeSpeed, (this.homeSpeed ?? 0) * (1 - (1 - ease) ** 3), compactGauge);
    this.gauge.content = new StyledText([...dial.chunks,
      ...t`\n${cell(this.homeSpeedLabel, width).trimEnd()}${this.homeSpeedAt ? `\nПоследний замер: ${new Date(this.homeSpeedAt).toLocaleTimeString("ru-RU", { hour12: false })}` : ""}`.chunks,
    ]);
    if (this.screen === "subscription")
      this.summary.content =
        this.busy === "subscription/read"
          ? "Подписка · загрузка сохранённого адреса…"
          : "Подписка";
    if (this.screen === "target")
      this.summary.content =
        "Адрес для проверки доступности\nHTTPS-запрос через каждый сервер; это не тест скорости.";
    if (this.screen === "diagnostics")
      this.summary.content = `OpenVPN / KDE: ${this.status?.networkmanager_active === "yes" ? "активен" : this.status?.networkmanager_active === "no" ? "отключён" : "неизвестно"}\nDocker: ${gatewayNames[gateway ?? ""] ?? "неизвестно"}\nПрофиль KDE: ${this.status?.networkmanager_configured === "yes" ? "настроен" : "не подтверждён"}\nПроверка: ${this.status?.diagnostics === "available" ? "выполнена" : "не запускалась"}\nПоследняя попытка: ${this.status?.last_attempt ?? "—"}\nПроверка сайта и скорости — на экране серверов.`;
    const table = this.screen === "servers" && this.ready();
    this.tableHeader.visible = table;
    if (this.screen === "servers")
      this.summary.content = !this.ready()
        ? "Для проверок нужен Docker. VPN подключать не требуется."
        : this.status?.subscription !== "configured"
          ? "Добавьте подписку для загрузки серверов."
          : `${this.servers.length} серверов · Сортировка [o]: ${{ name: "имя ↑", ping: "ping ↑", speed: "скорость ↓", availability: "доступность ↓" }[this.sort]}${this.catalogStale ? " · каталог устарел" : ""}\nСайт: ${this.target}`;
    const nameWidth = Math.max(10, width - 39);
    this.tableHeader.content = `   ${cell("Сервер", nameWidth)} ${cell("Ping мс", 8)} ${cell("Мбит/с", 8)} ${cell("Сайт", 7)} Статус`;
    const visible = this.visibleServers();
    this.rows.forEach((row, i) => {
      const s = visible[i];
      row.visible = table && i < this.rowCount() && !!s;
      if (!s) return;
      const queued = this.batch?.kind === "ping" && this.batch.ids?.includes(s.server_id) && !this.batch.running.has(s.server_id) && !this.batch.completed.has(s.server_id);
      const active = this.batch?.kind === "ping"
        ? this.batch.running.has(s.server_id)
        : this.batch?.id === s.server_id;
      const checking = (kind: Batch) =>
        active &&
        (this.batch?.kind === kind ||
          (kind === "availability" && this.batch?.kind === "ping"));
      const ping = queued ? "ждёт" : (checking("ping") || checking("speed")) && !this.batch?.pinged.has(s.server_id)
        ? frames[this.frame % 10]
        : s.ping_status === "failed"
          ? "ошибка"
          : (s.latency_ms?.toString() ?? "—");
      const speed = checking("speed") && this.batch?.pinged.has(s.server_id)
        ? frames[this.frame % 10]
        : s.status === "failed"
          ? "ошибка"
          : (s.download_mbps?.toFixed(1) ?? "—");
      const site = queued ? "—" : checking("availability") && this.batch?.pinged.has(s.server_id) && s.ping_status === "ready" && !this.batch?.completed.has(s.server_id)
        ? frames[this.frame % 10]
        : s.availability === "ready"
          ? "да"
          : s.availability === "failed"
            ? "нет"
            : "—";
      row.content = `${s.server_id === this.serverID ? "›" : " "}${s.selected ? "●" : " "} ${cell(s.display_name, nameWidth)} ${cell(ping, 8)} ${cell(speed, 8)} ${cell(site, 7)} ${queued ? "в очереди" : active ? "проверка" : s.ping_status === "failed" || s.availability === "failed" ? "ошибка" : s.availability === "ready" ? "готов" : s.status === "failed" ? "ошибка" : s.status === "ready" ? "готов" : "не пров."}`;
      row.fg =
        s.server_id === this.serverID
          ? p.accent
          : s.selected
            ? p.green
            : p.text;
      row.attributes = s.server_id === this.serverID ? TextAttributes.BOLD : 0;
    });
    if (this.batch) {
      const detail = this.batch.kind === "speed"
        ? `Тест скорости · ${this.batch.done}/${this.batch.total} · ${cell(this.servers.find((s) => s.server_id === this.batch?.id)?.display_name ?? "", 16).trim()}`
        : `Ping → сайт · ${this.batch.done}/${this.batch.total} · ${Math.floor((Date.now() - this.started) / 1000)} с`;
      this.progress.content = `${frames[this.frame % 10]} ${detail}${this.cancelled ? " · отмена…" : " · [k] отменить"}`;
    }
    else if (this.busy && this.busy !== "status")
      this.progress.content = `${frames[this.frame % 10]} ${names[this.busy] ?? "Операция"} · ${this.phase || "выполняется"} · ${Math.floor((Date.now() - this.started) / 1000)} с`;
    else
      this.progress.content =
        this.checked && Date.now() - this.checked > 15000
          ? "Статус устарел · [u] проверить"
          : "";
    this.notification.content = this.closing
      ? "Завершаем текущую операцию перед выходом…"
      : this.notice +
        (this.error && this.noticeAttempt
          ? `\nПопытка: ${this.noticeAttempt}`
          : "");
    this.notification.fg = this.error ? p.red : p.muted;
    const inlineProgress = this.screen === "home" && this.inlineSpeed() && this.homeTesting;
    this.progress.visible = this.progress.chunks.some(chunk => chunk.text.length > 0) && !inlineProgress;
    this.progress.height = this.progress.visible ? 1 : 0;
    const hasNotice = this.closing || !!this.notice;
    this.notification.visible = hasNotice;
    this.notification.height = hasNotice ? 2 : 0;
    this.footer.content = editor
      ? "Esc назад · Ctrl+C выход"
      : this.screen === "servers"
        ? "Tab меню/список · ↑↓ · Enter · o сортировка · Esc назад · q выход"
        : "↑↓ / Tab выбор · Enter · Esc назад · k отменить · q выход";
  };
  async close() {
    if (this.closing || this.disposed) return;
    this.closing = true;
    this.queued = undefined;
    this.input.blur();
    this.draft = "";
    this.input.value = "";
    if (this.busy || this.batch) {
      this.cancelled = true;
      this.backend.cancel?.();
      this.paint();
      return;
    }
    await this.finishClose();
  }
  private async finishClose() {
    if (this.disposed) return;
    this.disposed = true;
    clearInterval(this.timer);
    clearInterval(this.poll);
    this.renderer.keyInput.off("keypress", this.onKey);
    this.renderer.off("resize", this.paint);
    this.backend.onProgress = undefined;
    this.backend.onCheckProgress = undefined;
    await this.backend.close();
    this.renderer.destroy();
  }
}
