import type { Action, Backend, CheckProgress, Reply, Server, Status, AutostartOptions, AutostartServiceState } from "./bridge";
import { recoveryFor, type Recovery } from "./recovery";
import { compatible, requestClasses } from "./request-policy";
import { wrapText } from "./formatting";
import { frames } from "./animation-frames";
import { serverViewport, sortedServers, type ServerSort } from "./catalog-order";
import { safeText } from "./formatting";
import { names } from "./action-labels";
import { dispatchKey } from "./key-dispatch";
import { actionRegistry, groupedActions, serverMenuActions } from "./action-registry";
import { perform, updateCatalog } from "./request-execution";
import { testHomeSpeed, runBatch, cancelTask, cancel, retryCheck, reconnectBackend } from "./measurements";
import { defineMenuActions, menuItems, type ActionDefinition } from "./menu-actions";

export type Screen =
  | "server-rename" | "details" | "tasks" | "cancel-confirm" | "recovery" | "retry-confirm"
 | "profiles" | "profile-name" | "profile-source" | "profile-remove"
  | "autostart"
  | "home" | "settings"
  | "servers"
  | "subscription"
  | "diagnostics"
  | "target"
  | "confirm";
export const overlayScreens: readonly Screen[] = ["server-rename", "autostart", "details", "confirm", "cancel-confirm", "retry-confirm", "profile-remove", "tasks", "recovery", "subscription", "target", "profile-name", "profile-source"];
export const isOverlay = (screen: Screen) => overlayScreens.includes(screen);
export type Batch = "ping" | "speed" | "availability";
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
  "update-checking": "Проверка выпуска",
  "update-downloading": "Загрузка",
  "update-verifying": "Проверка пакета",
  "update-installing": "Установка",
  "update-done": "Установлено",
  "gateway-restart": "Переход на новую версию",
  compensating: "Восстановление состояния",
};
const nativeSupportedActions: readonly Action[] = [
  "status",
  "backend/start",
  "network/reapply",
  "start",
  "disconnect",
  "stop",
  "autostart/read",
  "autostart/set",
];

export interface AppKeyEvent {
  name: string;
  eventType?: string;
  ctrl?: boolean;
  shift?: boolean;
  preventDefault(): void;
}

export type StateRegion = "all" | "measurement" | "table" | "progress" | "editor" | "palette";

/** Renderer-independent application and request lifecycle. */
export class AppState {
  status?: Status;
  autostart?: AutostartOptions;
  serviceState?: AutostartServiceState;
  screen: Screen = "home";
  homeIndex = 0;
  overlayBase: "home" | AppState["tab"] = "home";
  tab: "servers" | "profiles" | "diagnostics" | "settings" = "servers";
  helpOpen = false;
  detailsOffset = 0;
  measurementTimes = new Map<string, number>();
  speedSamples = new Map<string, number[]>();
  sessionMeasurements = new Map<string, { at: number; ping?: number; speed?: number; site: string }[]>();
  /** Records only the values the finished action measured; the row also carries older results. */
  recordMeasurement(server: Server, action: Action = "servers/speed-batch") {
    const batch = action === "servers/speed-batch", check = action === "servers/check-batch";
    const ping = batch || check || action === "servers/ping", speed = batch || action === "servers/speed";
    const site = batch || check || action === "servers/availability";
    if (!this.sessionMeasurements.has(server.server_id) && this.sessionMeasurements.size >= 100)
      this.sessionMeasurements.delete(this.sessionMeasurements.keys().next().value!);
    const entries = this.sessionMeasurements.get(server.server_id) ?? [];
    entries.unshift({ at: Date.now(), ping: ping && server.ping_status === "ready" ? server.latency_ms : undefined,
      speed: speed && server.status === "ready" ? server.download_mbps : undefined, site: site ? server.availability ?? "untested" : "untested" });
    entries.length = Math.min(entries.length, 4);
    this.sessionMeasurements.set(server.server_id, entries);
  }
  profiles?: Reply["profiles"];
  profileName = "";
  profileRemoval?: { id: string; name: string; kind: "subscription" | "server" };
  profilePage = 0;

  forgetStatusHealth() {
    const known = this.status;
    this.status = known?.runtime === "native" || known?.runtime === "docker" ? {
      runtime: known.runtime, installed: known.installed,
      vpn_state: "unknown", gateway_state: "unknown", subscription: "unknown",
      routing_mode: "unknown", networkmanager_configured: "unknown",
      networkmanager_active: "unknown", diagnostics: "unknown",
    } : undefined;
  }

  resetProfileCatalog() {
    this.servers = [];
    this.catalogLoaded = false;
    this.serverID = undefined;
    this.viewStart = 0;
  }

  async browseProfile(id: string) {
    const navigation = this.navigationRevision;
    const reply = await this.perform("profiles/open", id);
    if (!reply?.ok) return;
    this.resetProfileCatalog();
    if (navigation === this.navigationRevision) this.open("servers");
    else this.loadCatalogIfShown();
  }

  async addProfile(source: string) {
    const navigation = this.navigationRevision, editor = this.editorRevision;
    const reply = await this.perform(
      "profiles/add",
      JSON.stringify({ name: this.profileName, source }),
    );
    if (!reply?.ok) return;
    if (navigation === this.navigationRevision && editor === this.editorRevision) this.editorValue = "";
    this.resetProfileCatalog();
    if (navigation === this.navigationRevision && editor === this.editorRevision) this.open("profiles");
    else this.loadCatalogIfShown();
  }

  async removeProfile() {
    const target = this.profileRemoval;
    this.profileRemoval = undefined;
    const current = this.profiles?.items.find(item => item.id === target?.id);
    if (!target || this.profiles?.active !== target.id || !current || current.name !== target.name || current.kind !== target.kind) {
      this.open("profiles", false);
      this.notify("Профиль изменился. Выберите его заново.", true);
      return;
    }
    const navigation = this.navigationRevision;
    const reply = await this.perform("profiles/remove", target.id);
    if (!reply?.ok) return;
    this.resetProfileCatalog();
    if (navigation === this.navigationRevision) this.open("profiles");
    else this.loadCatalogIfShown();
  }
  tasks = new Map<string, { action: Action; measurement: boolean; started: number; phase: string; value?: string; cancelRequested?: boolean }>();
  nextTask = 0;
  statusRevision = 0;
  readFlights = new Map<Action, { first: Promise<Reply | undefined>; followup?: Promise<Reply | undefined> }>();
  cancelTaskID?: string;
  cancelTaskAction?: Action;
  recovery?: Recovery & { action: Action; value?: string; homeServerID?: string; batchRetry?: { kind: Batch; ids: string[]; target: string } };
  retryOperation?: NonNullable<AppState["recovery"]>;
  retryTarget?: { kind: "server" | "profile"; id: string; name?: string };
  backendOffline = false;
  reconnecting = false;
  liveSpeeds = new Map<string, number>();
  selectionRevision = 0;
  get busy(): Action | undefined {
    return [...this.tasks.values()].at(-1)?.action;
  }
  get measurementAction(): Action | undefined {
    return [...this.tasks.values()].find(task => task.measurement)?.action;
  }
  queue: { id: string; action: Action; value?: string; kind?: "batch" | "home"; batchKind?: Batch; signature: string; catalogRevision: number; wasReady: boolean; started: number }[] = [];
  history: { action: Action; started: number; ended: number; outcome: string; phase: string; count?: number }[] = [];
  catalogRevision = 0;
  catalogSignature() { return this.servers.map(server => server.server_id).sort().join("|") + `:${this.profiles?.active ?? ""}:${this.catalogStale}`; }
  enqueue(action: Action, value?: string, kind?: "batch" | "home", batchKind?: Batch) {
    if (this.queue.some(entry => entry.action === action && (kind === "batch" || entry.value === value) && entry.kind === kind)) { this.notify("Эта операция уже в очереди."); return; }
    this.queue.push({ id: `queue-${++this.nextTask}`, action, value, kind, batchKind, signature: this.catalogSignature(), catalogRevision: this.catalogRevision, wasReady: this.ready(), started: Date.now() });
    this.notify("Операция поставлена в очередь [j].");
  }
  abandonQueue(outcome: string) {
    for (const entry of this.queue) this.history.unshift({ action: entry.action, started: entry.started, ended: Date.now(), phase: "", outcome });
    this.history.length = Math.min(20, this.history.length);
    this.queue = [];
  }
  drainQueue() {
    if (this.closing || this.disposed || this.backendOffline || this.reconnecting) return;
    while (this.queue.length) {
      const entry = this.queue[0]!;
      if (((this.batch || this.homeTesting) && requestClasses[entry.action] !== "read") || [...this.tasks.values()].some(task => !compatible(entry.action, task.action))) return;
      this.queue.shift();
      const needsService = requestClasses[entry.action] === "measurement" || entry.action === "servers/list" || entry.action === "servers/refresh";
      if ((needsService && entry.wasReady && !this.ready()) || (entry.action.startsWith("servers/") && (entry.signature !== this.catalogSignature() || entry.catalogRevision !== this.catalogRevision))) {
        this.history.unshift({ action: entry.action, started: entry.started, ended: Date.now(), outcome: "Не запущено: условия изменились", phase: "" });
        this.history.length = Math.min(20, this.history.length);
        this.notify("Задача снята с очереди: состояние или каталог изменились.", true);
        continue;
      }
      if (entry.kind === "batch") void this.runBatch(entry.batchKind!, entry.value ? JSON.parse(entry.value) : undefined);
      else if (entry.kind === "home") void this.testHomeSpeed(entry.value);
      else void this.perform(entry.action, entry.value);
      return;
    }
  }
  batch?: {
    kind: Batch;
    done: number;
    total: number;
    id?: string;
    ids?: string[];
    failed: number;
    target?: string;
    running: Set<string>;
    stages: Map<string, CheckProgress["stage"]>;
    pinged: Set<string>;
    completed: Set<string>;
    failures: Set<string>;
  };
  cancelled = false;
  closing = false;
  disposed = false;
  frame = 0;
  started = 0;
  checked = 0;
  notice = "";
  noticeIdentity?: { action: Action; reason: string; ok: boolean };
  noticeAttempt = "";
  error = false;
  selected = 0;
  serverMenu = false;
  serverMenuIndex = 0;
  serverMenuTarget?: { id: string; name: string };
  renameTarget?: { id: string; original: string };
  selectTarget?: { id: string; name: string };
  confirmationSnapshot?: string;
  confirmation: "disconnect" | "stop" | "gateway/restart" | "subscription" | "fastest" | "select" | "update" = "disconnect";
  /** Latest update check or install result; shown in Settings. */
  updateInfo?: import("./bridge").UpdateInfo;
  /** Version installed by this session; offers relaunching the interface. */
  updatedTo?: string;
  gatewayRestartOffered = false;
  /** Set when the user asks to restart the interface after an update. */
  relaunchRequested = false;
  fastestCandidate?: { id: string; name: string; speed: number; measuredAt: number };
  pendingSubscription?: string;
  serverID?: string;
  servers: Server[] = [];
  viewStart = 0;
  frozenOrder?: string[];
  catalogLoaded = false;
  catalogStale = false;
  sort: ServerSort = "name";
  sortDescending = false;
  filter = "";
  filterEditing = false;
  filterAnchor?: string;
  searchFocused = false;
  paletteQuery = "";
  paletteIndex = 0;
  openKeys(search: boolean) {
    this.helpOpen = true; this.searchFocused = search;
    this.paletteQuery = ""; this.paletteIndex = 0; this.paint();
  }
  /** Rows the shortcuts list can show, including section headers. */
  keysPageSize() { return Math.max(1, Math.min(18, this.viewport().terminalHeight - 7)); }
  paletteActions(all = true) {
    const screen = this.screen, menu = this.serverMenu;
    const result: { action: ActionDefinition & { keyHints: string[] }; screen: Screen }[] = [];
    const seen = new Set<string>();
    const screens: Screen[] = all ? ["home", "servers", "profiles", "diagnostics", "settings", "autostart", "tasks", "details"] : [];
    if (all && (this.recovery || this.backendOffline)) screens.push("recovery");
    try {
      for (const [index, target] of [screen, ...screens.filter(target => menu || target !== screen)].entries()) {
        this.screen = target; this.serverMenu = !index && menu;
        for (const group of groupedActions(this, true)) for (const action of group.actions) {
          if (seen.has(action.id)) continue;
          if (target === "tasks" && action.id.startsWith("task:cancel:") && Number(action.key) > [...this.tasks.values()].filter(task => task.action !== "status").length + this.queue.length) continue;
          const outsideDialog = index > 0 && (menu || isOverlay(screen));
          seen.add(action.id); result.push({ action: outsideDialog ? { ...action, available: false, unavailableReason: "Сначала закройте текущее окно." } : action, screen: target });
        }
        // Row-menu actions (rename, single ping) have no key of their own but should be searchable.
        if (target === "servers" && !this.serverMenu) for (const action of serverMenuActions(this)) {
          if (seen.has(action.id)) continue;
          const outsideDialog = index > 0 && (menu || isOverlay(screen));
          seen.add(action.id); result.push({ action: { ...action, keyHints: [], ...(outsideDialog ? { available: false, unavailableReason: "Сначала закройте текущее окно." } : {}) }, screen: target });
        }
      }
    } finally { this.screen = screen; this.serverMenu = menu; }
    return result;
  }
  keysSectionName(screen: Screen = this.screen) {
    return ({ home: "Главная", servers: "Серверы", profiles: "Профили", diagnostics: "Диагностика", settings: "Настройки", autostart: "Автозапуск", tasks: "Задачи", details: "Сервер", recovery: "Восстановление" } as Partial<Record<Screen, string>>)[screen] ?? "Это окно";
  }
  /** Without a query: this screen, then global keys. With a query: every screen. */
  filteredPaletteActions(all?: boolean) {
    const query = this.paletteQuery.trim().toLocaleLowerCase("ru");
    const here = this.keysSectionName();
    const merged: (ReturnType<AppState["paletteActions"]>[number] & { section: string })[] = [];
    const navigation = new Set(["up", "down", "left", "right", "pageup", "pagedown", "home", "end"]);
    for (const entry of this.paletteActions(all ?? !!query)) {
      const action = entry.action;
      // Arrows and paging are in the footer; listing them only pushes real actions out of view.
      if (action.keys.length && action.keys.every(key => navigation.has(key))) continue;
      const section = action.scope === "global" ? "Общие" : entry.screen === this.screen ? here : this.keysSectionName(entry.screen);
      const alias = merged.find(row => row.section === section && row.screen === entry.screen && row.action.label === action.label && row.action.unavailableReason === action.unavailableReason);
      if (alias) {
        const exactKey = [...action.keys, ...action.keyHints].some(key => key.toLocaleLowerCase("ru") === query);
        alias.action = { ...(exactKey ? action : alias.action), keys: [...new Set([...alias.action.keys, ...action.keys])], keyHints: [...new Set([...alias.action.keyHints, ...action.keyHints])] };
      } else merged.push({ ...entry, section });
    }
    const rank = (section: string) => section === here ? 0 : section === "Общие" ? 1 : 2;
    return merged.filter(({action}) => `${action.label} ${action.keys.join(" ")} ${action.keyHints.join(" ")}`.toLocaleLowerCase("ru").includes(query))
      .map((entry, order) => ({ entry, order })).sort((a, b) => rank(a.entry.section) - rank(b.entry.section) || a.order - b.order).map(({ entry }) => entry);
  }
  runPaletteAction() {
    const entry = this.filteredPaletteActions()[this.paletteIndex];
    if (!entry) return;
    if (!entry.action.available) { this.notify(entry.action.unavailableReason ?? "Команда недоступна.", true); return; }
    this.helpOpen = false;
    if (this.screen !== entry.screen) this.open(entry.screen, false, false);
    const action = this.groupedActions().flatMap(group => group.actions).find(action => action.id === entry.action.id)
      ?? (this.screen === "servers" ? serverMenuActions(this).find(action => action.id === entry.action.id) : undefined);
    if (action?.available) action.run();
    else this.notify(action?.unavailableReason ?? "Команда недоступна.", true);
    this.paint();
  }
  target = "https://www.youtube.com/";
  editorRevision = 0;
  navigationRevision = 0;
  loadedSubscription = false;
  pendingSubscriptionRevision?: number;
  pendingScreenRead?: "profiles/list" | "autostart/read";
  draft = "";
  timer?: ReturnType<typeof setInterval>;
  staleTimer?: ReturnType<typeof setTimeout>;
  animationEnabled = false;
  poll?: ReturnType<typeof setInterval>;
  homeTesting = false;
  homeSpeed?: number;
  homePing?: number;
  homeSpeedAt = 0;
  homeSpeedServer?: { id: string; name: string };
  homeSpeedLabel = "[t] Проверить скорость выбранного сервера";

  editorValue = "";
  constructor(readonly backend: Backend) {
    this.backend.onProgress = (phase, id) => {
      const task = id ? this.tasks.get(id) : [...this.tasks.values()].at(-1);
      if (!task) return;
      task.phase = this.phaseLabel(phase);
      this.paintProgress();
    };
    this.backend.onCheckProgress = (row, id) => {
      if (id && !["servers/check-batch", "servers/speed-batch"].includes(this.tasks.get(id)?.action ?? "")) return;
      const batch = this.batch;
      if (!batch || this.cancelled || this.closing || !batch.ids?.includes(row.server_id) || batch.completed.has(row.server_id)) return;
      const server = this.servers.find(s => s.server_id === row.server_id);
      if (!server) return;
      batch.running.add(row.server_id);
      batch.stages.set(row.server_id, row.stage);
      if (row.stage === "start") {
        server.ping_status = "untested";
        server.latency_ms = undefined;
        server.availability = "untested";
        if (batch.kind === "speed") { server.status = "untested"; server.download_mbps = undefined; server.download_seconds = undefined; server.downloaded_bytes = undefined; }
        this.paintTable();
        this.paintProgress();
        return;
      }
      server.ping_status = row.ping_status;
      server.latency_ms = row.ping_status === "ready" ? row.latency_ms : undefined;
      batch.pinged.add(row.server_id);
      if (row.stage === "speed" && batch.target === this.target) server.availability = row.availability;
      if (row.stage === "complete") {
        if (batch.kind === "speed" && row.status === "ready" && row.download_mbps !== undefined && Number.isFinite(row.download_mbps) && row.download_mbps >= 0) this.measurementTimes.set(row.server_id, Date.now());
        if (batch.target === this.target) server.availability = row.availability;
        if (batch.kind === "speed") { server.status = row.status ?? "failed"; server.download_mbps = row.download_mbps; server.download_seconds = row.download_seconds; server.downloaded_bytes = row.downloaded_bytes; }
        batch.running.delete(row.server_id);
        batch.completed.add(row.server_id);
        if (row.ping_status !== "ready" || row.availability !== "ready" || (batch.kind === "speed" && row.status !== "ready")) batch.failures.add(row.server_id);
        batch.done = batch.completed.size;
        batch.failed = batch.failures.size;
      }
      this.syncServerViewport();
      this.paintTable();
      this.paintProgress();
    };
    this.backend.onDisconnect = () => {
      if (this.closing || this.disposed) return;
      this.backendOffline = true;
      this.forgetStatusHealth();
      this.abandonQueue("Не запущено: связь потеряна");
      this.cancelled = true;
      this.recovery = { ...recoveryFor("status", "backend-unavailable", this.status?.runtime), action: "status" };
      this.notify("Связь потеряна. Действие могло продолжиться. [f] Восстановить связь", true);
    };
    this.backend.onSpeedProgress = (row, id) => {
      const task = id ? this.tasks.get(id) : undefined;
      if (!task || task.action !== "servers/speed" || task.value !== row.server_id || this.cancelled || this.closing) return;
      if (!Number.isFinite(row.download_mbps) || row.download_mbps < 0) return;
      this.liveSpeeds.set(row.server_id, row.download_mbps);
      const samples = this.speedSamples.get(row.server_id) ?? [];
      samples.push(row.download_mbps);
      if (samples.length > 12) samples.shift();
      this.speedSamples.set(row.server_id, samples);
      if (this.homeTesting && this.homeSpeedServer?.id === row.server_id) this.homeSpeed = row.download_mbps;
      this.paintMeasurement();
      if (this.screen === "servers") this.paintTable();
    };
  }

  private listeners = new Set<(region: StateRegion) => void>();
  subscribe(listener: (region: StateRegion) => void) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }
  private changed(region: StateRegion) {
    for (const listener of this.listeners) listener(region);
  }
  viewport() { return { width: 100, height: 28, terminalHeight: 28 }; }
  paint() { this.syncAnimation(); this.changed("all"); }
  paintMeasurement() { this.changed("measurement"); }
  paintTable() { this.changed("table"); }
  paintPalette() { this.changed("palette"); }
  paintProgress() { this.changed("progress"); }
  onClosing() {}
  onBeforeDispose() {}
  async onDispose() {}
  edit(value: string) {
    const clean = safeText(value);
    const next = this.screen === "server-rename" ? [...clean].slice(0, 60).join("") : clean;
    if (next === this.editorValue) return;
    this.editorValue = next;
    this.editorRevision++;
    if (this.screen === "subscription") this.draft = this.editorValue;
    this.changed("editor");
  }

  start() {
    this.animationEnabled = true;
    this.syncAnimation();
    this.poll = setInterval(() => this.pollStatus(), 5000);
    const navigation = this.navigationRevision;
    void this.perform("status").then(() => {
      if (navigation === this.navigationRevision && this.screen === "servers" && !this.closing) this.open("servers");
      else this.loadCatalogIfShown();
    });
  }
  pollStatus() {
    if (!this.closing && !this.backendOffline && !this.reconnecting && ![...this.tasks.values()].some(task => task.action !== "status" && !compatible("status", task.action)))
      void this.perform("status");
  }
  ready() {
    return (this.status?.gateway_state ?? this.status?.vpn_state) === "healthy";
  }
  runtime(): "docker" | "native" {
    return this.status?.runtime === "native" ? "native" : "docker";
  }
  nativeMode() {
    return this.runtime() === "native";
  }
  nativeInstalled() {
    return this.nativeMode() && this.status?.installed === true;
  }
  nativePreviewOnly() { return this.nativeMode() && !this.nativeInstalled(); }
  subscriptionAvailable() {
    return !this.nativeMode() || this.nativeInstalled();
  }
  serviceLabel(runtime: Status["runtime"] = this.status?.runtime) {
    return runtime === "native" ? "native-служба" : "Docker";
  }
  serviceObject(runtime: Status["runtime"] = this.status?.runtime) {
    return runtime === "native" ? "сервис" : "Docker";
  }
  nativePreview() {
    return this.nativeInstalled()
      ? "Native-пакет: доступна первичная настройка подписки. Системное окно Polkit попросит подтверждение администратора.\nВыбор первого сервера может занять несколько минут; VPN запускается отдельно, автозапуск включается отдельно и только по желанию."
      : "Native preview: доступны статус, запуск/остановка native-службы, VPN и автозапуск.\nСерверы, подписка, режим и диагностика пока не подключены.";
  }
  nativeUnavailable(action: Action) {
    if (action.startsWith("servers/"))
      return "В native preview каталог серверов, выбор и проверки пока не подключены.";
    if (action === "subscription/read")
      return "В native-пакете сохранённый адрес не показывается; задайте его через редактор настройки.";
    if (action === "subscription")
      return "В native preview настройка подписки доступна только в установленном native-пакете.";
    if (action === "toggle-mode")
      return "В native preview смена режима маршрутизации пока не подключена.";
    if (action === "diagnostics")
      return "В native preview адаптер диагностики пока не подключён.";
    return "В native preview эта операция пока не подключена.";
  }
  canPerform(action: Action) {
    if (!this.nativeMode() || this.nativeInstalled() || nativeSupportedActions.includes(action) ||
        (this.nativeInstalled() && action === "subscription")) return true;
    this.notify(this.nativeUnavailable(action), true);
    return false;
  }
  phaseLabel(phase: string) {
    if (this.nativeMode()) {
      const nativePhases: Record<string, string> = {
        "compose-up": "Запуск native-службы",
        "compose-up-done": "Native-служба запущена",
        "compose-down": "Остановка native-службы",
      };
      return nativePhases[phase] ?? phases[phase] ?? "";
    }
    return phases[phase] ?? "";
  }
  actionLabel(action: Action) {
    if (this.nativeMode()) {
      if (action === "backend/start") return "Запускаем сервис";
      if (action === "network/reapply") return "Применяем сетевые настройки VPN";
      if (action === "stop") return "Останавливаем сервис";
      if (this.nativeInstalled() && action === "subscription") return "Настраиваем native-пакет";
    }
    return names[action] ?? "Операция";
  }
  autostartValue(gateway: boolean, connect: boolean, resume = this.autostart?.resume) {
    const value: {
      gateway: boolean;
      connect: boolean;
      resume?: boolean;
      runtime?: "docker" | "native";
    } = { gateway, connect };
    if (resume !== undefined) value.resume = resume;
    if (this.autostart?.runtime !== undefined)
      value.runtime = this.autostart.runtime;
    return JSON.stringify(value);
  }
  enabledServiceLabel(value: string) {
    switch (value.trim().toLowerCase()) {
      case "enabled": return "автозапуск включён";
      case "disabled": return "автозапуск отключён";
      case "masked": return "автозапуск замаскирован";
      case "not-installed":
      case "not-found": return "служба не установлена";
      case "unknown": return "автозапуск: неизвестно";
      default: return "автозапуск: неизвестно";
    }
  }
  activeServiceLabel(value: string) {
    switch (value.trim().toLowerCase()) {
      case "active": return "активна";
      case "inactive":
      case "dead": return "неактивна";
      case "failed": return "ошибка";
      case "activating": return "запускается";
      case "deactivating": return "останавливается";
      case "reloading": return "перечитывает конфигурацию";
      case "unknown": return "неизвестно";
      case "not-installed":
      case "not-found": return "недоступно";
      default: return "неизвестно";
    }
  }
  serviceStateLabel() {
    if (!this.serviceState)
      return "Фактическое состояние службы: не проверено · текущее состояние: недоступно";
    return `Фактическое состояние службы: ${this.enabledServiceLabel(this.serviceState.enabled)}; текущее состояние: ${this.activeServiceLabel(this.serviceState.active)}`;
  }
  async loadPendingSubscription() {
    if (this.closing || this.disposed || this.nativePreviewOnly() || (!this.status && [...this.tasks.values()].some(task => task.action === "status")) || [...this.tasks.values()].some(task => !compatible("subscription/read", task.action))) return;
    const revision = this.pendingSubscriptionRevision;
    this.pendingSubscriptionRevision = undefined;
    if (revision !== undefined && revision === this.editorRevision &&
        this.screen === "subscription" && !this.loadedSubscription)
      await this.perform("subscription/read");
  }
  async loadPendingScreenRead() {
    const action = this.pendingScreenRead;
    if (!action || this.closing || this.disposed || this.backendOffline || this.reconnecting ||
        this.batch || this.homeTesting || [...this.tasks.values()].some(task => !compatible(action, task.action))) return;
    this.pendingScreenRead = undefined;
    if ((action === "profiles/list" && this.screen === "profiles") ||
        (action === "autostart/read" && (this.screen === "autostart" || this.screen === "settings"))) await this.perform(action);
  }
  notify(text: string, error = false) {
    this.noticeIdentity = undefined;
    this.notice = text;
    this.noticeAttempt = "";
    this.error = error;
    this.paint();
  }
  open(screen: Screen, load = true, loadScreen = true) {
    if (this.closing) return;
    if (screen === "subscription" && !this.subscriptionAvailable()) {
      this.notify(this.nativeUnavailable("subscription"), true);
      return;
    }
    if (isOverlay(screen) && !isOverlay(this.screen)) this.overlayBase = this.screen === "home" ? "home" : this.tab;
    const tile = ["", "servers", "tasks", "profiles", "diagnostics", "settings"].indexOf(screen);
    if (tile > 0) this.homeIndex = tile;
    if (this.screen === "subscription") this.draft = this.editorValue;
    this.filterEditing = false;
    this.screen = screen;
    if (["servers", "profiles", "diagnostics", "settings"].includes(screen)) this.tab = screen as typeof this.tab;
    this.helpOpen = false;
    this.pendingScreenRead = undefined;
    this.selected = 0;
    if (screen === "details") this.detailsOffset = 0;
    if (screen === "retry-confirm") {
      this.confirmationSnapshot = this.confirmationContext();
      const retry = this.retryOperation;
      this.retryTarget = retry?.value && retry.action === "servers/select"
        ? { kind: "server", id: retry.value, name: this.servers.find(server => server.server_id === retry.value)?.display_name }
        : retry?.value && ["profiles/open", "profiles/remove"].includes(retry.action)
          ? { kind: "profile", id: retry.value, name: this.profiles?.items.find(profile => profile.id === retry.value)?.name }
          : retry?.action === "start" && this.servers.some(server => server.selected)
            ? { kind: "server", id: this.servers.find(server => server.selected)!.server_id, name: this.servers.find(server => server.selected)!.display_name }
            : undefined;
    }
    if (screen === "cancel-confirm") this.cancelTaskAction = this.cancelTaskID ? this.tasks.get(this.cancelTaskID)?.action : undefined;
    if (screen === "profile-remove") {
      const profile = this.profiles?.items.find(item => item.id === this.profiles?.active);
      this.profileRemoval = profile && { id: profile.id, name: profile.name, kind: profile.kind };
    }
    this.serverMenu = false;
    this.serverMenuTarget = undefined;
    this.navigationRevision++;
    if (screen === "subscription") {
      this.editorValue = this.draft;
      if (!this.loadedSubscription && !this.nativePreviewOnly()) {
        this.pendingSubscriptionRevision = this.editorRevision;
        void this.loadPendingSubscription();
      }
    }
    if (loadScreen && screen === "profiles") { this.pendingScreenRead = "profiles/list"; void this.loadPendingScreenRead(); }
    if(screen==="profile-name" || screen==="profile-source") this.editorValue="";
    if (loadScreen && (screen === "autostart" || load && screen === "settings" && this.nativeInstalled())) { this.pendingScreenRead = "autostart/read"; void this.loadPendingScreenRead(); }
    if (screen === "target") this.editorValue = this.target;
    this.paint();
    if (load) this.loadCatalogIfShown();
    // Diagnostics immediately displays the cached evidence. Refresh is explicit.
  }

  /** Home tile columns: three when wide, one stacked column when narrow; the grid scrolls when short. */
  homeColumns() {
    const { width } = this.viewport();
    return width >= 90 ? 3 : width < 56 ? 1 : 2;
  }

  moveHome(key: string) {
    const columns = this.homeColumns();
    const row = Math.floor(this.homeIndex / columns), col = this.homeIndex % columns;
    if (key === "home") this.homeIndex = 0;
    else if (key === "end") this.homeIndex = 5;
    else if (key === "left" && col > 0) this.homeIndex--;
    else if (key === "right" && col < columns - 1 && this.homeIndex < 5) this.homeIndex++;
    else if (key === "up" && row > 0) this.homeIndex -= columns;
    else if (key === "down" && this.homeIndex + columns < 6) this.homeIndex += columns;
    this.paint();
  }
  activateHome() {
    const target = [undefined, "servers", "tasks", "profiles", "diagnostics", "settings"] as const;
    if (this.homeIndex) { this.open(target[this.homeIndex]!); return; }
    const action = this.actions().find(item => ["home:connect", "home:start-service", "home:x"].includes(item.id));
    if (action?.available) action.run();
    else if (action?.unavailableBehavior === "notice" && action.unavailableReason) this.notify(action.unavailableReason, true);
  }
  /** Reads the cached catalog when the server table is showing an unloaded one. A cached read only. */
  loadCatalogIfShown() {
    if (
      !this.closing && this.screen === "servers" &&
      this.ready() &&
      this.status?.subscription === "configured" &&
      !this.catalogLoaded &&
      !this.catalogStale &&
      ![...this.tasks.values()].some(task => !compatible("servers/list", task.action)) &&
      !this.batch &&
      !this.nativePreviewOnly()
    )
      void this.perform("servers/list");
  }
  confirmFastest() {
    if (!this.ready() || this.catalogStale) { this.notify("Сначала запустите сервис и обновите список серверов.", true); return; }
    const candidates = this.filteredServers().filter(server => this.measurementTimes.has(server.server_id) && server.status === "ready" && server.availability !== "failed" && server.download_mbps !== undefined && Number.isFinite(server.download_mbps) && server.download_mbps >= 0)
      .sort((a, b) => b.download_mbps! - a.download_mbps! || a.server_id.localeCompare(b.server_id));
    const server = candidates[0];
    if (!server) { this.notify("Нет подходящих замеров этой сессии. Запустите тест скорости [t]."); return; }
    if (server.selected) { this.notify("Самый быстрый сервер уже выбран."); return; }
    this.fastestCandidate = { id: server.server_id, name: server.display_name, speed: server.download_mbps!, measuredAt: this.measurementTimes.get(server.server_id)! };
    this.confirmation = "fastest";
    this.open("confirm");
  }
  openServerMenu() {
    const server = this.servers.find(s => s.server_id === this.serverID);
    if (!server) return;
    this.serverMenuTarget = { id: server.server_id, name: server.display_name };
    this.serverMenuIndex = 0; this.serverMenu = true; this.paint();
  }
  closeServerMenu() {
    const target = this.serverMenuTarget;
    this.serverMenu = false; this.serverMenuTarget = undefined;
    if (target && this.servers.some(s => s.server_id === target.id && s.display_name === target.name)) this.serverID = target.id;
    this.syncServerViewport(); this.paint();
  }
  openServerRename() {
    const server = this.servers.find(s => s.server_id === this.serverID);
    if (!server) return;
    this.renameTarget = { id: server.server_id, original: server.original_name ?? server.display_name };
    this.open("server-rename", false);
    this.editorValue = [...server.display_name].slice(0, 60).join("");
    this.paint();
  }
  async saveServerRename() {
    const target = this.renameTarget;
    if (!target || !this.servers.some(s => s.server_id === target.id)) {
      this.open(this.overlayBase, false);
      this.notify("Сервер больше не найден. Переименование отменено.", true);
      return;
    }
    if ([...this.tasks.values()].some(task => task.action === "servers/rename")) {
      this.notify("Переименование уже выполняется. Дождитесь завершения.", true); return;
    }
    const revision = this.navigationRevision;
    const typed = this.editorValue.replace(/\s+/g, " ").trim();
    const reply = await this.perform("servers/rename", JSON.stringify({ id: target.id, name: this.editorValue }));
    // The name is saved even when the backend could not attach a fresh catalog;
    // show it on the row now and let the next catalog read confirm it.
    if (reply?.ok && !reply.catalog?.servers) {
      const at = this.servers.findIndex(s => s.server_id === target.id);
      if (at >= 0) {
        const { original_name, ...row } = this.servers[at]!;
        this.servers[at] = typed ? { ...row, display_name: typed, original_name: target.original } : { ...row, display_name: original_name ?? row.display_name };
      }
    }
    if (reply?.ok && this.screen === "server-rename" && this.navigationRevision === revision) {
      this.serverID = target.id; this.open(this.overlayBase, false); this.syncServerViewport(); this.paint();
    }
  }
  confirmServerSelection() {
    const server = this.servers.find(s => s.server_id === this.serverID);
    if (!server) { this.notify("Сервер изменился. Выберите сервер заново.", true); return; }
    this.selectTarget = { id: server.server_id, name: server.display_name };
    this.confirmationSnapshot = this.confirmationContext();
    this.confirmation = "select"; this.open("confirm");
  }
  retryTargetValid() {
    const target = this.retryTarget;
    if (!target) return true;
    if (!target.name) return false;
    if (target.kind === "server") return this.servers.some(server => server.server_id === target.id && server.display_name === target.name);
    return this.profiles?.items.some(profile => profile.id === target.id && profile.name === target.name) ?? false;
  }
  confirmationContext() {
    return JSON.stringify({ runtime: this.status?.runtime, installed: this.status?.installed,
      gateway: this.status?.gateway_state ?? this.status?.vpn_state,
      active: this.status?.networkmanager_active, profile: this.profiles?.active,
      server: this.servers.find(server => server.selected)?.server_id });
  }
  confirm(action: "disconnect" | "stop" | "gateway/restart") {
    this.confirmationSnapshot = this.confirmationContext();
    this.confirmation = action;
    this.open("confirm");
  }
  confirmUpdate() {
    if (!this.updateInfo?.available) return;
    this.confirmationSnapshot = this.confirmationContext();
    this.confirmation = "update";
    this.open("confirm");
  }
  relaunch() {
    this.relaunchRequested = true;
    void this.close();
  }
  confirmSubscription(value: string) {
    this.pendingSubscription = value;
    this.confirmationSnapshot = this.confirmationContext();
    this.confirmation = "subscription";
    this.open("confirm");
  }
  items(): ActionDefinition[] {
    return defineMenuActions(this, menuItems(this));
  }
  activateMenuRow(index: number) {
    if (this.helpOpen || this.filterEditing) return;
    const item = this.items()[index];
    if (!item) return;
    this.selected = index;
    this.onKey({ name: item.kind ? "return" : item.key, preventDefault() {} });
    this.paint();
  }
  actions(): ActionDefinition[] { return actionRegistry(this); }
  groupedActions() { return groupedActions(this); }
  onKey = (key: AppKeyEvent) => dispatchKey(this, key);
  sortedServers() {
    return sortedServers(this.filteredServers(), this.sort, this.frozenOrder, this.sortDescending);
  }
  filteredServers() {
    const query = this.filter.trim().toLocaleLowerCase("ru");
    return query ? this.servers.filter(server => [server.display_name, server.original_name ?? "", server.country ?? ""].some(value => value.toLocaleLowerCase("ru").includes(query))) : this.servers;
  }
  editFilter(value: string) {
    this.filter = safeText(value);
    const list = this.sortedServers();
    if (!list.some(server => server.server_id === this.serverID)) this.serverID = list[0]?.server_id;
    this.syncServerViewport(); this.paintTable();
  }
  setSort(sort: ServerSort) {
    if (this.sort === sort) this.sortDescending = !this.sortDescending;
    else { this.sort = sort; this.sortDescending = sort === "speed" || sort === "availability"; }
    this.syncServerViewport(); this.paint();
  }
  profilePageSize() { return Math.max(1, Math.min(9, this.viewport().height - 8 - wrapText(this.notice, Math.max(1, this.viewport().width - 2)).length)); }
  /** Short terminals give the two notice rows back to content while there is nothing to say. */
  noticeReserved() {
    return this.viewport().height >= 16 || !!this.notice || this.closing;
  }
  rowCount() {
    return Math.max(1, Math.min(100, this.viewport().height - (this.noticeReserved() ? 6 : 4) - (this.filter || this.filterEditing ? 1 : 0)));
  }
  syncServerViewport() {
    this.viewStart = serverViewport(this.sortedServers(), this.serverID, this.viewStart, this.rowCount()).start;
  }
  visibleServers() {
    return serverViewport(this.sortedServers(), this.serverID, this.viewStart, this.rowCount()).servers;
  }
  updateCatalog(reply: Reply, action: Action) { return updateCatalog(this, reply, action); }
  perform(action: Action, value?: string, insideBatch = false): Promise<Reply | undefined> {
    if (action === "servers/rename" || requestClasses[action] !== "read") return perform(this, action, value, insideBatch);
    const flight = this.readFlights.get(action);
    if (flight) {
      if (!flight.followup) flight.followup = flight.first.then(() => {
        if (this.readFlights.get(action) === flight) this.readFlights.delete(action);
        return this.perform(action, value, insideBatch);
      });
      return flight.followup;
    }
    const first = perform(this, action, value, insideBatch);
    const entry = { first, followup: undefined as Promise<Reply | undefined> | undefined };
    this.readFlights.set(action, entry);
    void first.finally(() => { if (!entry.followup && this.readFlights.get(action) === entry) this.readFlights.delete(action); });
    return first;
  }
  inlineSpeed() {
    return this.viewport().width < 70 || this.viewport().height < 25;
  }
  testHomeSpeed(sourceID?: string) { return testHomeSpeed(this, sourceID); }
  runBatch(kind: Batch, retry?: { ids: string[]; target: string }) { return runBatch(this, kind, retry); }
  cancelTask(id: string) { return cancelTask(this, id); }
  cancel() { return cancel(this); }
  retryCheck(recovery: NonNullable<AppState["recovery"]>) { return retryCheck(this, recovery); }
  reconnectBackend() { return reconnectBackend(this); }
  syncAnimation() {
    const animated = this.animationEnabled && !this.disposed &&
      (!!this.batch || [...this.tasks.values()].some(task => task.action !== "status"));
    if (animated && !this.timer) this.timer = setInterval(() => {
      this.frame++;
      this.paintProgress();
      if (this.batch && this.screen === "servers") this.paintTable();
    }, 100);
    else if (!animated && this.timer) { clearInterval(this.timer); this.timer = undefined; }
  }
  async close() {
    if (this.closing || this.disposed) return;
    this.closing = true;
    this.queue = [];
    this.onClosing();
    this.pendingSubscription = undefined;
    this.draft = "";
    this.editorValue = "";
    if (this.tasks.size || this.batch || this.homeTesting) {
      this.cancelled = true;
      for (const id of this.tasks.keys()) this.backend.cancel?.(id);
      this.paint();
      return;
    }
    await this.finishClose();
  }
  async finishClose() {
    if (this.disposed || this.tasks.size || this.batch || this.homeTesting) return;
    this.disposed = true;
    clearInterval(this.timer);
    clearTimeout(this.staleTimer);
    clearInterval(this.poll);
    this.onBeforeDispose();
    this.backend.onProgress = undefined;
    this.backend.onCheckProgress = undefined;
    this.backend.onSpeedProgress = undefined;
    this.backend.onDisconnect = undefined;
    try { await this.backend.close(); } finally { await this.onDispose(); }
  }
}
