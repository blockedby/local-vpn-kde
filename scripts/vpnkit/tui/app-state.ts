import type { Action, Backend, CheckProgress, Reply, Server, Status, AutostartOptions, AutostartServiceState } from "./bridge";
import { failure } from "./model";
import { recoveryFor, type Recovery } from "./recovery";
import { compatible } from "./request-policy";
import { frames } from "./animation-frames";
import { serverViewport, sortedServers, type ServerSort } from "./catalog-order";
import { safeText } from "./formatting";

type Screen =
  | "tasks" | "cancel-confirm" | "recovery" | "retry-confirm"
 | "profiles" | "profile-name" | "profile-source" | "profile-remove"
  | "autostart"
  | "home"
  | "servers"
  | "subscription"
  | "diagnostics"
  | "target"
  | "confirm";
type Batch = "ping" | "speed" | "availability";
type Item = { key: string; label: string; run: () => void };
export const names: Partial<Record<Action, string>> = {
  "autostart/read": "Читаем настройки автозапуска",
  "autostart/set": "Сохраняем автозапуск",
  "backend/start": "Запускаем Docker",
  "network/reapply": "Применяем сетевые настройки VPN",
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

export type StateRegion = "all" | "measurement" | "table" | "progress" | "editor";

/** Renderer-independent application and request lifecycle. */
export class AppState {
  status?: Status;
  autostart?: AutostartOptions;
  serviceState?: AutostartServiceState;
  screen: Screen = "home";
  profiles?: Reply["profiles"];
  profileName = "";
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
  }

  async browseProfile(id: string) {
    const reply = await this.perform("profiles/open", id);
    if (!reply?.ok) return;
    this.resetProfileCatalog();
    this.open("servers");
  }

  async addProfile(source: string) {
    const reply = await this.perform(
      "profiles/add",
      JSON.stringify({ name: this.profileName, source }),
    );
    if (!reply?.ok) return;
    this.editorValue = "";
    this.resetProfileCatalog();
    this.open("profiles");
  }

  async removeProfile() {
    const reply = await this.perform("profiles/remove", this.profiles?.active);
    if (!reply?.ok) return;
    this.resetProfileCatalog();
    this.open("profiles");
  }
  tasks = new Map<string, { action: Action; measurement: boolean; started: number; phase: string; value?: string }>();
  nextTask = 0;
  cancelTaskID?: string;
  recovery?: Recovery & { action: Action; value?: string; homeServerID?: string; batchRetry?: { kind: Batch; ids: string[]; target: string } };
  retryOperation?: NonNullable<AppState["recovery"]>;
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
  queued?: { action: Action; value?: string };
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
  noticeAttempt = "";
  error = false;
  selected = 0;
  serverMenu = false;
  confirmation: "disconnect" | "stop" | "subscription" = "disconnect";
  pendingSubscription?: string;
  serverID?: string;
  servers: Server[] = [];
  viewStart = 0;
  frozenOrder?: string[];
  catalogLoaded = false;
  catalogStale = false;
  sort: ServerSort = "name";
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
  constructor(protected backend: Backend) {
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
        if (batch.target === this.target) server.availability = row.availability;
        if (batch.kind === "speed") { server.status = row.status ?? "failed"; server.download_mbps = row.download_mbps; server.download_seconds = row.download_seconds; server.downloaded_bytes = row.downloaded_bytes; }
        batch.running.delete(row.server_id);
        batch.completed.add(row.server_id);
        if (row.ping_status !== "ready" || row.availability !== "ready" || (batch.kind === "speed" && row.status !== "ready")) batch.failures.add(row.server_id);
        batch.done = batch.completed.size;
        batch.failed = batch.failures.size;
      }
      this.paintTable();
      this.paintProgress();
    };
    this.backend.onDisconnect = () => {
      if (this.closing || this.disposed) return;
      this.backendOffline = true;
      this.forgetStatusHealth();
      this.queued = undefined;
      this.cancelled = true;
      this.recovery = { ...recoveryFor("status", "backend-unavailable", this.status?.runtime), action: "status" };
      this.notify("Связь потеряна. Действие могло продолжиться. [f] Восстановить связь", true);
    };
    this.backend.onSpeedProgress = (row, id) => {
      const task = id ? this.tasks.get(id) : undefined;
      if (!task || task.action !== "servers/speed" || task.value !== row.server_id || this.cancelled || this.closing) return;
      this.liveSpeeds.set(row.server_id, row.download_mbps);
      if (this.homeTesting && this.homeSpeedServer?.id === row.server_id) this.homeSpeed = row.download_mbps;
      if (this.screen === "home") this.paintMeasurement();
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
  paintProgress() { this.changed("progress"); }
  onClosing() {}
  onBeforeDispose() {}
  async onDispose() {}
  edit(value: string) {
    this.editorValue = safeText(value);
    this.editorRevision++;
    if (this.screen === "subscription") this.draft = this.editorValue;
    this.changed("editor");
  }

  start() {
    this.animationEnabled = true;
    this.syncAnimation();
    this.poll = setInterval(() => {
      if (!this.busy && !this.batch && !this.closing && !this.backendOffline && !this.reconnecting)
        void this.perform("status");
    }, 5000);
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
    if (this.closing || this.disposed || this.nativePreviewOnly() || [...this.tasks.values()].some(task => !compatible("subscription/read", task.action))) return;
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
    this.notice = text;
    this.noticeAttempt = "";
    this.error = error;
    this.paint();
  }
  open(screen: Screen) {
    if(screen==="servers" && this.nativeInstalled() && !this.profiles && this.status?.subscription==="configured") screen="profiles";
    if (this.closing) return;
    if (screen === "subscription" && !this.subscriptionAvailable()) {
      this.notify(this.nativeUnavailable("subscription"), true);
      return;
    }
    if (this.screen === "subscription") this.draft = this.editorValue;
    this.screen = screen;
    this.pendingScreenRead = undefined;
    this.selected = 0;
    this.serverMenu = false;
    this.navigationRevision++;
    if (screen === "subscription") {
      this.editorValue = this.draft;
      if (!this.loadedSubscription && !this.nativePreviewOnly()) {
        this.pendingSubscriptionRevision = this.editorRevision;
        void this.loadPendingSubscription();
      }
    }
    if (screen === "profiles") { this.pendingScreenRead = "profiles/list"; void this.loadPendingScreenRead(); }
    if(screen==="profile-name" || screen==="profile-source") this.editorValue="";
    if (screen === "autostart") { this.pendingScreenRead = "autostart/read"; void this.loadPendingScreenRead(); }
    if (screen === "target") this.editorValue = this.target;
    this.paint();
    if (
      screen === "servers" &&
      this.ready() &&
      this.status?.subscription === "configured" &&
      !this.catalogLoaded &&
      !this.busy &&
      !this.batch &&
      !this.nativePreviewOnly()
    )
      void this.perform(this.catalogStale ? "servers/refresh" : "servers/list");
    // Diagnostics immediately displays the cached evidence. Refresh is explicit.
  }
  confirm(action: "disconnect" | "stop") {
    this.confirmation = action;
    this.open("confirm");
  }
  confirmSubscription(value: string) {
    this.pendingSubscription = value;
    this.confirmation = "subscription";
    this.open("confirm");
  }
  items(): Item[] {
    if (this.screen === "cancel-confirm" || this.screen === "retry-confirm") {
      const retry = this.screen === "retry-confirm";
      return [{ key: "n", label: "Нет", run: () => this.open(retry ? "recovery" : "tasks") },
        { key: "y", label: "Да", run: () => {
          if (retry) { const r = this.retryOperation; this.retryOperation = undefined; this.open("home"); if (r) void this.perform(r.action, r.value); }
          else { const id = this.cancelTaskID; this.open("tasks"); if (id) this.cancelTask(id); }
        } }];
    }
    if (this.screen === "tasks") return [...this.tasks].filter(([, task]) => task.action !== "status").map(([id, task], i) => ({
      key: String(i + 1), label: `Отменить: ${names[task.action] ?? (task.action === "servers/check-batch" ? "Ping → сайт" : "Операция")}`,
      run: () => {
        if (task.measurement || task.action.startsWith("servers/") && task.action !== "servers/select") this.cancelTask(id);
        else { this.cancelTaskID = id; this.open("cancel-confirm"); }
      },
    }));
    if (this.screen === "recovery") {
      if (this.backendOffline) return [{ key: "u", label: this.reconnecting ? "Восстанавливаем связь…" : "Восстановить связь", run: () => void this.reconnectBackend() }];
      return (this.recovery?.actions ?? ["status", "diagnostics"]).map(choice => ({
        key: { status: "u", diagnostics: "d", servers: "v", retry: "r", reapply: "f" }[choice],
        label: { status: "Проверить состояние", diagnostics: "Открыть диагностику", servers: "Выбрать другой сервер", retry: "Повторить действие", reapply: "Применить сетевые настройки VPN" }[choice],
        run: () => {
          if (choice === "reapply") void this.perform("network/reapply");
          else if (choice === "servers") this.open("servers");
          else if (choice === "diagnostics") this.open("diagnostics");
          else if (choice === "status") void this.perform("status");
          else if (this.recovery) {
            if (["servers/ping", "servers/speed", "servers/availability", "servers/check-batch", "servers/speed-batch", "servers/list", "status", "diagnostics"].includes(this.recovery.action))
              void this.retryCheck(this.recovery);
            else { this.retryOperation = { ...this.recovery }; this.open("retry-confirm"); }
          }
        },
      }));
    }
    if(this.screen==="profile-remove") return [
      {key:"y",label:"Удалить профиль из приложения",run:()=>void this.removeProfile()},
      {key:"n",label:"Отмена",run:()=>this.open("profiles")},
    ];
    if(this.screen==="profiles") {
      const profiles=this.profiles?.items ?? [];
      const pages=Math.max(1,Math.ceil(profiles.length/4));this.profilePage=Math.min(this.profilePage,pages-1);
      return [
        ...profiles.slice(this.profilePage*4,this.profilePage*4+4).map((profile,i)=>({key:String(i+1),label:`${profile.id===this.profiles?.active?"● ":""}${profile.name} · ${profile.kind==="server"?"сервер":"подписка"} · ${profile.count}${profile.stale?" · сохранённый список":""}`,run:()=>void this.browseProfile(profile.id)})),
        {key:"n",label:"Добавить профиль",run:()=>this.open("profile-name")},
        ...(this.profiles?.active?[{key:"x",label:"Удалить выбранный профиль",run:()=>this.open("profile-remove")}]:[]),
        ...(pages>1?[{key:"[",label:"Предыдущие профили",run:()=>{this.profilePage=(this.profilePage+pages-1)%pages;this.selected=0;this.paint()}},{key:"]",label:"Следующие профили",run:()=>{this.profilePage=(this.profilePage+1)%pages;this.selected=0;this.paint()}}]:[]),
      ];
    }
    if (this.screen === "confirm")
      return [
        {
          key: "n",
          label: "Нет",
          run: () => {
            if (this.confirmation === "subscription") {
              this.pendingSubscription = undefined;
              this.open("subscription");
            } else this.open("home");
          },
        },
        {
          key: "y",
          label: "Да",
          run: () => {
            if (this.confirmation === "subscription") {
              const value = this.pendingSubscription;
              this.pendingSubscription = undefined;
              this.open("home");
              if (value) void this.perform("subscription", value);
              return;
            }
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
          ? [{ key: "s", label: `Запустить ${this.serviceObject()}`, run: run("backend/start") }]
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
        { key: "t", label: this.nativePreviewOnly() ? "Тест скорости · недоступно в native preview" : this.inlineSpeed() ? `Тест скорости · ${this.homeTesting ? `${frames[this.frame % 10]} ${this.measurementAction === "servers/ping" ? "ping" : this.measurementAction === "servers/speed" ? "замер" : "подготовка"} · ` : ""}${this.homePing === undefined ? "—" : Math.round(this.homePing)}ms, ${this.homeSpeed === undefined ? "—" : this.homeSpeed.toFixed(1)} Мбит/с` : "Тест скорости", run: () => void this.testHomeSpeed() },
        { key: "v", label: this.nativePreviewOnly() ? "Серверы · native preview" : "Серверы", run: () => this.open("servers") },
        { key: "c", label: this.nativeInstalled() && this.status?.subscription==="configured" ? "Профили · подписки и серверы" : this.nativeMode() ? (this.nativeInstalled() ? "Подписка · настройка native-пакета" : "Подписка · недоступна в native preview") : "Подписка", run: () => this.open(this.nativeInstalled() && this.status?.subscription==="configured"?"profiles":"subscription") },
        { key: "d", label: this.nativePreviewOnly() ? "Диагностика · недоступна в native" : "Диагностика", run: () => this.open("diagnostics") },
        { key: "a", label: "Автозапуск", run: () => this.open("autostart") },
        {
          key: "m",
          label: `Режим: ${this.status?.routing_mode === "smart" ? "умный" : this.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}${this.nativePreviewOnly() ? " · недоступен в native" : ""}`,
          run: () => {
            if (this.nativeMode()) void this.perform("toggle-mode");
            else if (this.ready()) void this.perform("toggle-mode");
            else this.notify(`Смена режима доступна после запуска ${this.nativeMode() ? "сервиса" : "Docker"}.`);
          },
        },
        ...(["healthy", "unhealthy", "running", "stopped", "starting"].includes(
          this.status?.gateway_state ?? this.status?.vpn_state ?? "",
        )
          ? [
              {
                key: "z",
                label: this.nativeMode() ? "Остановить сервис" : "Остановить Docker и VPN",
                run: () => this.confirm("stop"),
              },
            ]
          : []),
      ];
    if (this.screen === "autostart") {
      if (!this.autostart) return [{ key: "u", label: "Загрузить настройки", run: run("autostart/read") }];
      return [
        { key: "g", label: `${this.nativeMode() || this.autostart.runtime === "native" ? "Запускать native-службу" : "Запускать шлюз"}: ${this.autostart.gateway ? "да" : "нет"}`, run: () => void this.perform("autostart/set", this.autostartValue(!this.autostart!.gateway, false)) },
        { key: "a", label: `Подключать VPN: ${this.autostart.connect ? "да" : "нет"}`, run: () => void this.perform("autostart/set", this.autostartValue(this.autostart!.gateway || !this.autostart!.connect, !this.autostart!.connect)) },
      ];
    }
    if (this.screen === "diagnostics")
      return [
        { key: "u", label: this.nativePreviewOnly() ? "Обновить диагностику · недоступно в native" : "Обновить диагностику", run: run("diagnostics") },
      ];
    if (this.screen === "servers") {
      if (!this.ready())
        return [
          { key: "s", label: `Запустить ${this.serviceObject()}`, run: run("backend/start") },
        ];
      if (this.nativePreviewOnly())
        return [
          { key: "i", label: "Серверы и проверки · недоступны в native preview", run: () => this.notify(this.nativeUnavailable("servers/list"), true) },
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
          label: "Скорость + сайт · замер до 3 с",
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
  onKey = (key: AppKeyEvent) => {
    if (key.eventType === "release") return;
    const editor = this.screen === "subscription" || this.screen === "target" || this.screen === "profile-name" || this.screen === "profile-source";
    if (key.name === "escape") {
      key.preventDefault();
      this.open(
        this.screen.startsWith("profile-") || this.screen==="servers" && !!this.profiles
          ? "profiles"
          : editor && this.screen === "target"
          ? "servers"
          : this.screen === "confirm" && this.confirmation === "subscription"
            ? "subscription"
            : "home",
      );
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
        if(this.screen==="profile-name") {
          const value=this.editorValue.trim();if(!value || [...value].length>60){this.notify("Введите имя профиля: до 60 символов.",true);return}
          this.profileName=value;this.open("profile-source");return;
        }
        if(this.screen==="profile-source") {void this.addProfile(this.editorValue.trim());return}
        if (this.screen === "subscription") {
          const value = this.editorValue.trim();
          if (this.nativeInstalled()) {
            if (!value || !/^(https?:|vless:|hysteria2:|hy2:)\/\//.test(value)) {
              this.notify("Введите ссылку подписки.", true);
              return;
            }
            this.confirmSubscription(value);
          } else void this.perform("subscription", value);
        } else {
          try {
            const u = new URL(this.editorValue.trim());
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
    if (key.name === "j") { this.open("tasks"); return; }
    if (key.name === "f" && this.recovery) {
      if (this.recovery.actions.includes("reapply")) void this.perform("network/reapply");
      else this.open("recovery");
      return;
    }
    if (key.name === "k") {
      this.cancel();
      return;
    }
    const screenKeys: Record<string, Screen> = {
      h: "home",
      v: "servers",
      c: this.nativeInstalled() && this.status?.subscription==="configured"?"profiles":"subscription",
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
        this.frozenOrder = undefined;
        this.sort = sorts[(sorts.indexOf(this.sort) + 1) % sorts.length];
        this.viewStart = 0;
        if (this.batch) this.frozenOrder = this.sortedServers().map(s => s.server_id);
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
    else if (key.name === "u" && !items.some(item => item.key === "u"))
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
  sortedServers() {
    return sortedServers(this.servers, this.sort, this.frozenOrder);
  }
  rowCount() {
    return Math.max(1, Math.min(100, this.viewport().terminalHeight - (this.inlineSpeed() ? (this.catalogStale || this.status?.subscription !== "configured" ? 14 : 12) : 15)));
  }
  visibleServers() {
    const viewport = serverViewport(this.sortedServers(), this.serverID, this.viewStart, this.rowCount());
    this.viewStart = viewport.start;
    return viewport.servers;
  }
  updateCatalog(reply: Reply, action: Action) {
    if ((action === "servers/check-batch" || action === "servers/speed-batch") && reply.catalog?.servers) {
      for (const row of reply.catalog.servers) {
        const at = this.servers.findIndex((s) => s.server_id === row.server_id);
        if (at >= 0)
          this.servers[at] = {
            ...this.servers[at],
            ...(action === "servers/speed-batch" ? { status: row.status, download_mbps: row.download_mbps, download_seconds: row.download_seconds, downloaded_bytes: row.downloaded_bytes } : {}),
            ping_status: row.ping_status,
            latency_ms: row.latency_ms,
            availability:
              this.batch?.target === this.target
                ? row.availability
                : this.servers[at].availability,
          };
      }
    } else if (reply.catalog?.servers) {
      const previous = new Map(this.servers.map(s => [s.server_id, s]));
      const oldOrder = this.sortedServers();
      const anchor = oldOrder[this.viewStart]?.server_id;
      const cursorIndex = oldOrder.findIndex(s => s.server_id === this.serverID);
      this.servers = reply.catalog.servers.filter(s => !this.profiles || (s.profile_id ?? "profile_legacy1") === this.profiles.active).map(s => {
        const measured = previous.get(s.server_id);
        return { ...s, status: measured?.status ?? "untested",
          ping_status: measured?.ping_status ?? "untested", latency_ms: measured?.latency_ms,
          download_mbps: measured?.download_mbps, download_seconds: measured?.download_seconds,
          downloaded_bytes: measured?.downloaded_bytes, availability: measured?.availability ?? "untested" };
      });
      const order = this.sortedServers();
      if (!this.servers.some(s => s.server_id === this.serverID))
        this.serverID = order[Math.max(0, Math.min(cursorIndex, order.length - 1))]?.server_id;
      const anchorIndex = order.findIndex(s => s.server_id === anchor);
      if (anchorIndex >= 0) this.viewStart = anchorIndex;
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
    if (this.backendOffline || this.reconnecting && !["status", "servers/list"].includes(action)) {
      this.notify("Служба управления недоступна. [f] Восстановить связь", true); return;
    }
    if (!this.canPerform(action)) return;
    const measurementActive = !!this.batch || this.homeTesting;
    const allowedDuringMeasurement = ["servers/select", "status", "subscription/read"].includes(action);
    if ((measurementActive && !insideBatch && !allowedDuringMeasurement) ||
        [...this.tasks.values()].some(task => !compatible(action, task.action))) {
      if (this.busy === "status" && action !== "status" && !this.queued && !measurementActive) {
        this.queued = { action, value };
        return;
      }
      if (action !== "status") this.notify("Эта операция конфликтует с текущей. Дождитесь завершения или отмените её [k].");
      return;
    }
    if (action === "subscription" && (!value || !/^(https?:|vless:|hysteria2:|hy2:)\/\//.test(value))) {
      this.notify("Введите ссылку подписки.", true);
      return;
    }
    const requestID = `ui-${++this.nextTask}`;
    const requestRuntime = this.status?.runtime;
    const nativeSubscriptionSetup = action === "subscription" && this.nativeInstalled();
    this.tasks.set(requestID, { action, measurement: insideBatch, started: Date.now(), phase: "", value });
    const selectionRevision = this.selectionRevision;
    const revision = this.editorRevision,
      navigation = this.navigationRevision;
    this.paint();
    let reply: Reply | undefined;
    try {
      reply = await this.backend.request(action, value, requestID);
      if (!action.startsWith("servers/") && !action.startsWith("autostart/") && action !== "subscription/read" && !action.startsWith("profiles/")) {
        this.status = reply.status;
        this.checked = Date.now();
        clearTimeout(this.staleTimer);
        this.staleTimer = setTimeout(() => this.paintProgress(), 15001);
      }
      if (reply.ok && reply.autostart) this.autostart = reply.autostart;
      if (action === "autostart/read" || action === "autostart/set") {
        this.serviceState = reply.ok ? reply.autostart_service : undefined;
        if (!reply.ok && action === "autostart/set") this.autostart = undefined;
      }
      const previousMeasurements = this.homeTesting && action === "servers/list"
        ? new Map(this.servers.map(row => [row.server_id, row])) : undefined;
      const currentSelection = this.servers.find(row => row.selected)?.server_id;
      this.updateCatalog(reply, action);
      if (selectionRevision !== this.selectionRevision && reply.catalog?.servers)
        this.servers.forEach(row => row.selected = row.server_id === currentSelection);
      if (previousMeasurements) this.servers = this.servers.map(row => {
        const previous = previousMeasurements.get(row.server_id);
        return previous ? { ...row, status: previous.status, ping_status: previous.ping_status,
          latency_ms: previous.latency_ms, availability: previous.availability,
          download_mbps: previous.download_mbps, download_seconds: previous.download_seconds,
          downloaded_bytes: previous.downloaded_bytes } : row;
      });
      if(reply.ok && reply.profiles) this.profiles=reply.profiles;
      if (action === "subscription/read" && reply.ok) {
        this.loadedSubscription = true;
        if (revision === this.editorRevision) {
          this.draft = reply.value ?? "";
          if (this.screen === "subscription") this.editorValue = this.draft;
        }
      }
      if (!reply.ok && reply.reason === "cancelled") this.recovery = undefined;
      else if (!reply.ok) this.recovery = { ...recoveryFor(action, reply.reason, this.status?.runtime), action, value,
        homeServerID: this.homeTesting ? this.homeSpeedServer?.id : undefined,
        batchRetry: this.batch ? { kind: this.batch.kind, ids: [...(this.batch.ids ?? [])], target: this.batch.target ?? this.target } : undefined,
      };
      if (!reply.ok && !insideBatch) {
        this.notice = nativeSubscriptionSetup
          ? "Не удалось настроить native-пакет. Проверьте подтверждение администратора и повторите."
          : reply.reason === "autostart-unavailable" ? "Не удалось настроить автозапуск. Проверьте пользовательскую службу systemd." : this.recovery?.message ?? failure(reply.reason, reply.code, this.status?.runtime);
        this.noticeAttempt = reply.reason === "native-network-settings-changed" ? "" : [
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
        if (this.recovery?.actions.includes("reapply") && ["network/reapply", "backend/start", "start", "disconnect", "stop"].includes(action)) this.recovery = undefined;
        const done: Partial<Record<Action, string>> = {
          "network/reapply": "Сетевые настройки VPN применены.",
          "profiles/list":"", "profiles/open":"Профиль открыт. Активный сервер не изменён.", "profiles/add":"Профиль добавлен.", "profiles/remove":"Профиль удалён. Активное подключение не изменено.",
          "autostart/read": "",
          "autostart/set": this.nativeMode() || this.autostart?.runtime === "native"
            ? "Сохранено. Настройка применяется при входе в графическую сессию."
            : "Сохранено. Настройка применяется при следующем входе в KDE.",
          "backend/start": this.nativeMode()
            ? "Native-служба готова. Доступны VPN и автозапуск; серверы пока не подключены."
            : "Docker готов. Можно проверять серверы.",
          start: "",
          disconnect: this.nativeMode() ? "VPN отключён. Native-служба работает." : "VPN отключён. Docker работает.",
          stop: this.nativeMode() ? "Native-служба и VPN остановлены." : "Docker и VPN остановлены.",
          "servers/list": "Список загружен.",
          "servers/refresh": "Каталог обновлён.",
          "servers/select": "Сервер выбран.",
          subscription: nativeSubscriptionSetup
            ? "Native-пакет настроен; первый сервер выбран, это могло занять несколько минут.\nVPN не подключён — нажмите «Подключить VPN» отдельно. Автозапуск не изменён — настройте его отдельно при желании."
            : "Подписка сохранена. Обновите список серверов.",
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
        if (action === "servers/select" && value) {
          this.selectionRevision++;
          this.servers.forEach((s) => (s.selected = s.server_id === value));
        }
      }
    } catch {
      this.backendOffline = true;
      this.cancelled = true;
      this.queued = undefined;
      this.recovery = { ...recoveryFor(action, "backend-unavailable", this.status?.runtime), action, value };
      this.forgetStatusHealth();
      if (action === "autostart/read" || action === "autostart/set") {
        this.serviceState = undefined;
        if (action === "autostart/set") this.autostart = undefined;
      }
      this.notify(
        nativeSubscriptionSetup
          ? "Не удалось настроить native-пакет. Проверьте подтверждение администратора и повторите."
          : failure("backend-unavailable", null, requestRuntime),
        true,
      );
    } finally {
      this.tasks.delete(requestID);
      if (action === "servers/speed" && value) this.liveSpeeds.delete(value);
      this.paint();
      if (this.closing && !this.batch && !this.homeTesting && !this.tasks.size) await this.finishClose();
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
        !this.batch &&
        !this.nativePreviewOnly()
      ) {
        await this.perform(
          this.catalogStale ? "servers/refresh" : "servers/list",
        );
      }
    }
    if(action==="servers/refresh" && reply?.ok && this.profiles){
      await this.perform("profiles/list");
      if(this.profiles.items.find(p=>p.id===this.profiles?.active)?.stale) this.notify("Источник недоступен. Показаны ранее сохранённые серверы.");
    }
    await this.loadPendingSubscription();
    await this.loadPendingScreenRead();
    return reply;
  }
  inlineSpeed() {
    return this.viewport().width < 70 || this.viewport().height < 25;
  }
  async testHomeSpeed(sourceID?: string) {
    if ([...this.tasks.values()].some(task => task.action !== "status") || this.batch || this.homeTesting) {
      this.notify("Дождитесь завершения или отмените текущую операцию [k].");
      return;
    }
    if (this.nativePreviewOnly()) { this.notify(this.nativeUnavailable("servers/speed"), true); return; }
    if (!this.ready()) { this.notify(`Сначала запустите ${this.serviceObject()}.`, true); return; }
    this.homeTesting = true;
    this.cancelled = false;
    this.homeSpeed = undefined;
    this.homePing = undefined;
    this.homeSpeedAt = 0;
    this.homeSpeedServer = undefined;
    this.notice = "";
    this.noticeAttempt = "";
    this.error = false;
    this.homeSpeedLabel = "Определяем выбранный сервер…";
    try {
      const list = await this.perform("servers/list", undefined, true);
      if (this.cancelled || this.closing) return;
      if (!list?.ok) throw new Error("Не удалось загрузить серверы.");
      const server = list.catalog?.servers?.find(row => sourceID ? row.server_id === sourceID : row.selected);
      if (!server) throw new Error(sourceID ? "Сервер удалён из подписки. Выберите другой сервер." : "Сначала выберите сервер в списке.");
      this.homeSpeedServer = { id: server.server_id, name: server.display_name };
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
      this.homeSpeed = undefined;
      this.homeSpeedLabel = error instanceof Error ? error.message : "Ошибка измерения.";
      this.notify(this.homeSpeedLabel, true);
    } finally {
      if (this.cancelled) { this.homeSpeedLabel = "Тест отменён"; this.homeSpeed = undefined; }
      this.homeTesting = false;
      this.paint();
      if (this.closing && !this.tasks.size) await this.finishClose();
      else { await this.loadPendingSubscription(); await this.loadPendingScreenRead(); }
    }
  }

  async runBatch(kind: Batch, retry?: { ids: string[]; target: string }) {
    if (this.busy || this.batch || this.homeTesting) {
      this.notify("Дождитесь завершения или отмените текущую операцию [k].");
      return;
    }
    if (this.nativePreviewOnly()) {
      this.notify(this.nativeUnavailable("servers/check-batch"), true);
      return;
    }
    if (!this.ready()) {
      this.notify(`Сначала запустите ${this.serviceObject()}.`, true);
      return;
    }
    this.cancelled = false;
    this.notice = "";
    this.noticeAttempt = "";
    this.error = false;
    // One measurement pass at a time; server selection has an independent task.
    this.started = Date.now();
    this.batch = {
      kind,
      done: 0,
      total: this.servers.length,
      failed: 0,
      target: retry?.target ?? this.target,
      running: new Set(), stages: new Map(),
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
            ? failure("backend-outdated", null, this.status?.runtime)
            : `Не удалось загрузить серверы. Проверьте подписку и ${this.serviceLabel()}.`,
          true,
        );
        if (this.closing && !this.tasks.size && !this.homeTesting && !this.batch) await this.finishClose();
        else await this.loadPendingScreenRead();
        return;
      }
    }
    const ids = retry?.ids ?? this.sortedServers().map((s) => s.server_id);
    if (ids.some(id => !this.servers.some(s => s.server_id === id))) {
      this.batch = undefined;
      this.notify("Сервер удалён из подписки. Обновите список перед проверкой.", true);
      await this.loadPendingScreenRead();
      return;
    }
    this.frozenOrder = ids;
    this.batch.total = ids.length;
    const target = this.batch.target!;
    let stoppedReason: string | undefined;
    if (!this.cancelled && !this.closing && ids.length) {
      this.batch.ids = ids;
      const result = await this.perform(kind === "speed" ? "servers/speed-batch" : "servers/check-batch", JSON.stringify({ ids, url: target }), true);
      if (!this.cancelled && !this.closing) {
        for (const row of result?.catalog?.servers ?? []) {
          this.batch.completed.add(row.server_id);
          if (row.ping_status !== "ready" || row.availability !== "ready" || (kind === "speed" && row.status !== "ready")) this.batch.failures.add(row.server_id);
        }
        this.batch.done = this.batch.completed.size;
        this.batch.failed = this.batch.failures.size;
        if (!result?.ok) { stoppedReason = result?.reason ?? "unavailable"; this.cancelled = true; }
      }
    }
    const { done, total, failed } = this.batch;
    this.frozenOrder = undefined;
    this.batch = undefined;
    this.notify(
      stoppedReason
        ? `${failure(stoppedReason, null, this.status?.runtime)} Проверено: ${done}/${total}.`
        : `${this.cancelled || this.closing ? "Остановлено" : "Завершено"}: ${done}/${total} · ошибок ${failed}. ${(this.cancelled || this.closing) ? (kind === "speed" ? "Завершённые результаты сохранены." : "Показаны полученные результаты; проход не сохранён.") : "Результаты сохранены."}`,
      failed > 0 || !!stoppedReason,
    );
    if (this.closing && !this.tasks.size && !this.homeTesting && !this.batch) await this.finishClose();
    else { await this.loadPendingSubscription(); await this.loadPendingScreenRead(); }
  }
  cancelTask(id: string) {
    const task = this.tasks.get(id);
    if (!task) return;
    if (task.measurement) this.cancelled = true;
    this.backend.cancel?.(id);
    this.notify("Отмена запрошена. Ожидаем завершения и восстановления состояния.");
  }
  cancel() {
    const entries = [...this.tasks.entries()].filter(([, task]) => !["status", "subscription", "subscription/read"].includes(task.action));
    if (entries.length !== 1) { if (entries.length) this.open("tasks"); return; }
    const [id, task] = entries[0]!;
    if (task.measurement || task.action.startsWith("servers/") && task.action !== "servers/select") this.cancelTask(id);
    else { this.cancelTaskID = id; this.open("cancel-confirm"); }
  }
  async retryCheck(recovery: NonNullable<AppState["recovery"]>) {
    if (recovery.homeServerID) {
      await this.testHomeSpeed(recovery.homeServerID);
      return;
    }
    if (recovery.batchRetry) {
      await this.runBatch(recovery.batchRetry.kind, recovery.batchRetry);
      return;
    }
    const { action, value } = recovery;
    if (action === "servers/speed") {
      const ping = await this.perform("servers/ping", value);
      if (!ping?.ok || ping.catalog?.server?.ping_status !== "ready") return;
    }
    await this.perform(action, value);
  }
  async reconnectBackend() {
    if (this.reconnecting || this.tasks.size || this.batch || this.homeTesting || this.closing) return;
    if (!this.backend.reconnect) { this.notify("Перезапустите интерфейс: служба не поддерживает восстановление.", true); return; }
    this.reconnecting = true;
    this.paint();
    try {
      await this.backend.reconnect();
      if (this.closing || this.disposed) return;
      this.backendOffline = false;
      const state = await this.perform("status");
      if (!state?.ok) throw new Error();
      if (this.ready() && this.status?.subscription === "configured") {
        const list = await this.perform("servers/list");
        if (!list?.ok) throw new Error();
      }
      this.notify("Связь восстановлена. Состояние обновлено; действия не повторялись.");
    } catch { this.backendOffline = true; this.notify("Не удалось восстановить связь. Можно повторить [u].", true); }
    finally {
      this.reconnecting = false;
      this.paint();
      if (!this.backendOffline) await this.loadPendingScreenRead();
    }
  }
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
    this.queued = undefined;
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
