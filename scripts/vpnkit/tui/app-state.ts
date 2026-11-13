import type { Action, Backend, CheckProgress, Reply, Server, Status, AutostartOptions, AutostartServiceState } from "./bridge";
import { recoveryFor, type Recovery } from "./recovery";
import { compatible, requestClasses } from "./request-policy";
import { wrapText } from "./formatting";
import { frames } from "./animation-frames";
import { serverViewport, sortedServers, type ServerSort } from "./catalog-order";
import { safeText } from "./formatting";
import { names } from "./action-labels";
import { dispatchKey } from "./key-dispatch";
import { actionRegistry, groupedActions } from "./action-registry";
import { perform, updateCatalog } from "./request-execution";
import { testHomeSpeed, runBatch, cancelTask, cancel, retryCheck, reconnectBackend } from "./measurements";
import { defineMenuActions, menuItems, type ActionDefinition } from "./menu-actions";

export type Screen =
  | "details" | "tasks" | "cancel-confirm" | "recovery" | "retry-confirm"
 | "profiles" | "profile-name" | "profile-source" | "profile-remove"
  | "autostart"
  | "home" | "settings"
  | "servers"
  | "subscription"
  | "diagnostics"
  | "target"
  | "confirm";
export const overlayScreens: readonly Screen[] = ["autostart", "details", "confirm", "cancel-confirm", "retry-confirm", "profile-remove", "tasks", "recovery", "subscription", "target", "profile-name", "profile-source"];
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
  screen: Screen = "servers";
  tab: "servers" | "profiles" | "diagnostics" | "settings" = "servers";
  helpOpen = false;
  helpOffset = 0;
  detailsOffset = 0;
  measurementTimes = new Map<string, number>();
  speedSamples = new Map<string, number[]>();
  sessionMeasurements = new Map<string, { at: number; ping?: number; speed?: number; site: string }[]>();
  recordMeasurement(server: Server) {
    if (!this.sessionMeasurements.has(server.server_id) && this.sessionMeasurements.size >= 100)
      this.sessionMeasurements.delete(this.sessionMeasurements.keys().next().value!);
    const entries = this.sessionMeasurements.get(server.server_id) ?? [];
    entries.unshift({ at: Date.now(), ping: server.ping_status === "ready" ? server.latency_ms : undefined,
      speed: server.status === "ready" ? server.download_mbps : undefined, site: server.availability ?? "untested" });
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
  confirmationSnapshot?: string;
  confirmation: "disconnect" | "stop" | "subscription" | "fastest" = "disconnect";
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
  paletteOpen = false;
  paletteQuery = "";
  paletteIndex = 0;
  paletteActions() {
    const screen = this.screen, tab = this.tab;
    const open = this.paletteOpen; this.paletteOpen = false;
    const result: { action: ActionDefinition; screen: Screen }[] = [];
    const seen = new Set<string>();
    for (const target of [tab, "servers", "profiles", "diagnostics", "settings", "autostart", "tasks", "details", ...(this.recovery || this.backendOffline ? ["recovery" as const] : [])] as Screen[]) {
      this.screen = target;
      for (const action of actionRegistry(this)) {
        if (seen.has(action.id) || /^(help:|editor:|filter:)/.test(action.id) || action.id === "global:palette") continue;
        if (target === "tasks" && action.id.startsWith("task:cancel:") && Number(action.key) > [...this.tasks.values()].filter(task => task.action !== "status").length + this.queue.length) continue;
        const labels: Record<string, string> = { "global:back": "Вернуться или закрыть уведомление", "global:quit-control": "Выйти из приложения", "global:quit": "Выйти из приложения", "global:tab": "Следующая вкладка", "global:shift+tab": "Предыдущая вкладка", "menu:previous": "Предыдущий пункт", "menu:next": "Следующий пункт", "menu:select": "Выбрать пункт меню", "servers:up": "Предыдущий сервер", "servers:down": "Следующий сервер", "servers:pageup": "Предыдущая страница серверов", "servers:pagedown": "Следующая страница серверов", "servers:home": "Первый сервер", "servers:end": "Последний сервер" };
        seen.add(action.id); result.push({ action: { ...action, label: action.id.startsWith("task:cancel:") ? `Отменить: ${action.label}` : labels[action.id] ?? action.label }, screen: target });
      }
    }
    this.screen = screen; this.tab = tab; this.paletteOpen = open;
    return result;
  }
  filteredPaletteActions() {
    const query = this.paletteQuery.trim().toLocaleLowerCase("ru");
    return this.paletteActions().filter(({action}) => `${action.label} ${action.keys.join(" ")}`.toLocaleLowerCase("ru").includes(query));
  }
  runPaletteAction() {
    const entry = this.filteredPaletteActions()[this.paletteIndex];
    if (!entry?.action.available) return;
    this.paletteOpen = false;
    if (entry.action.scope !== "global" && this.screen !== entry.screen) this.open(entry.screen, false, false);
    entry.action.run(); this.paint();
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
    const next = safeText(value);
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
    void this.perform("status").then(() => { if (navigation === this.navigationRevision && this.screen === "servers" && !this.closing) this.open("servers"); });
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
  autostartValue(gateway: boolean, connect: boolean) {
    const value: {
      gateway: boolean;
      connect: boolean;
      runtime?: "docker" | "native";
    } = { gateway, connect };
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
        (action === "autostart/read" && this.screen === "autostart")) await this.perform(action);
  }
  notify(text: string, error = false) {
    this.noticeIdentity = undefined;
    this.notice = text;
    this.noticeAttempt = "";
    this.error = error;
    this.paint();
  }
  open(screen: Screen, load = true, loadScreen = true) {
    if (screen === "home") screen = "servers";
    if (this.closing) return;
    if (screen === "subscription" && !this.subscriptionAvailable()) {
      this.notify(this.nativeUnavailable("subscription"), true);
      return;
    }
    if (this.screen === "subscription") this.draft = this.editorValue;
    this.filterEditing = false;
    this.screen = screen;
    if (["servers", "profiles", "diagnostics", "settings"].includes(screen)) this.tab = screen as typeof this.tab;
    this.helpOpen = false;
    this.pendingScreenRead = undefined;
    this.selected = screen === "profile-remove" ? 1 : 0;
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
    if (loadScreen && screen === "autostart") { this.pendingScreenRead = "autostart/read"; void this.loadPendingScreenRead(); }
    if (screen === "target") this.editorValue = this.target;
    this.paint();
    if (
      load && screen === "servers" &&
      this.ready() &&
      this.status?.subscription === "configured" &&
      !this.catalogLoaded &&
      !this.catalogStale &&
      !this.busy &&
      !this.batch &&
      !this.nativePreviewOnly()
    )
      void this.perform("servers/list");
    // Diagnostics immediately displays the cached evidence. Refresh is explicit.
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
  confirm(action: "disconnect" | "stop") {
    this.confirmationSnapshot = this.confirmationContext();
    this.confirmation = action;
    this.open("confirm");
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
  actions(): ActionDefinition[] { return actionRegistry(this); }
  groupedActions() { return groupedActions(this); }
  onKey = (key: AppKeyEvent) => dispatchKey(this, key);
  sortedServers() {
    return sortedServers(this.filteredServers(), this.sort, this.frozenOrder, this.sortDescending);
  }
  filteredServers() {
    const query = this.filter.trim().toLocaleLowerCase("ru");
    return query ? this.servers.filter(server => server.display_name.toLocaleLowerCase("ru").includes(query)) : this.servers;
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
  rowCount() {
    return Math.max(1, Math.min(100, this.viewport().height - 6 - (this.filter || this.filterEditing ? 1 : 0)));
  }
  syncServerViewport() {
    this.viewStart = serverViewport(this.sortedServers(), this.serverID, this.viewStart, this.rowCount()).start;
  }
  visibleServers() {
    return serverViewport(this.sortedServers(), this.serverID, this.viewStart, this.rowCount()).servers;
  }
  updateCatalog(reply: Reply, action: Action) { return updateCatalog(this, reply, action); }
  perform(action: Action, value?: string, insideBatch = false): Promise<Reply | undefined> {
    if (requestClasses[action] !== "read") return perform(this, action, value, insideBatch);
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
