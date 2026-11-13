import { BoxRenderable, TextRenderable, InputRenderable, TextAttributes, type CliRenderer } from "@opentui/core";
import type { Backend } from "./bridge";
import { connection, palette as p } from "./model";
import { observeRendererClose } from "./renderer-lifecycle";
import { cell } from "./formatting";
import { ServerTableView } from "./server-table-view";
import { MenuView } from "./menu-view";
import { MeasurementView } from "./measurement-view";
import { frames } from "./animation-frames";
import { AppState, names } from "./app-state";
export { cell } from "./formatting";

export class App extends AppState {
  private root: BoxRenderable;
  private headline: TextRenderable;
  private facts: TextRenderable;
  private content: BoxRenderable;
  private summary: TextRenderable;
  private menuView: MenuView;
  private tableView: ServerTableView;
  private input: InputRenderable;
  private editorHint: TextRenderable;
  private progress: TextRenderable;
  private notification: TextRenderable;
  private footer: TextRenderable;

  private measurementView: MeasurementView;
  private readonly rendererClosed: Promise<void>;
  constructor(
    private renderer: CliRenderer,
    backend: Backend,
    private demo = false,
  ) {
    super(backend);
    this.paint = this.paint.bind(this);
    this.rendererClosed = observeRendererClose(renderer);
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
    this.menuView = new MenuView(renderer, this.content, (index) => {
      this.serverMenu = true;
      this.selected = index;
      this.items()[index]?.run();
      this.paint();
    });
    this.measurementView = new MeasurementView(renderer, this.content);
    this.tableView = new ServerTableView(renderer, this.content, (index) => {
      const server = this.visibleServers()[index];
      if (server) {
        this.serverMenu = false;
        this.serverID = server.server_id;
        this.paint();
      }
    });
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
      this.edit(this.input.value);
      if (this.input.value !== this.editorValue) this.input.value = this.editorValue;
    });
    this.editorHint = text(this.content, "", p.muted, 2);
    this.progress = text(this.root, "", p.accent);
    this.notification = text(this.root, "", p.muted, 2);
    this.footer = text(this.root, "", p.muted);
    renderer.keyInput.on("keypress", this.onKey);
    renderer.on("resize", this.paint);
    this.paint();
  }

  override viewport() { return { width: this.renderer.width, height: this.renderer.height, terminalHeight: this.renderer.terminalHeight }; }
  override paintMeasurement() {
    this.measurementView.update({
      home: this.screen === "home", inline: this.inlineSpeed(), servers: this.servers,
      testing: this.homeTesting, speed: this.homeSpeed, speedAt: this.homeSpeedAt,
      speedServer: this.homeSpeedServer, speedLabel: this.homeSpeedLabel,
    });
  }
  override paintTable() {
    this.tableView.update({
      visible: this.screen === "servers" && this.ready() && !this.nativePreviewOnly(),
      servers: this.visibleServers(), rowCount: this.rowCount(), serverID: this.serverID,
      batch: this.batch, frame: this.frame, liveSpeeds: this.liveSpeeds,
    });
  }
  override paintProgress() {
    const width = Math.max(10, this.renderer.terminalWidth - 2);
    const activeTask = [...this.tasks.values()].find(task => task.measurement) ??
      [...this.tasks.values()].find(task => task.action !== "status");
    if (this.batch) {
      const detail = `${this.batch.kind === "speed" ? "Ping → сайт + скорость" : "Ping → сайт"} · ${this.batch.done}/${this.batch.total} · ${Math.floor((Date.now() - this.started) / 1000)} с`;
      this.progress.content = `${frames[this.frame % 10]} ${detail}${this.cancelled ? " · отмена…" : " · [k] отменить"}`;
    }
    else if (activeTask) {
      this.progress.content = `${frames[this.frame % 10]} ${this.actionLabel(activeTask.action)} · ${activeTask.phase || "выполняется"} · ${Math.floor((Date.now() - activeTask.started) / 1000)} с`;
    }
    else
      this.progress.content =
        this.checked && Date.now() - this.checked > 15000
          ? "Статус устарел · [u] проверить"
          : "";
    const switching = [...this.tasks.values()].find(task => task.action === "servers/select");
    if (switching && (this.batch || this.homeTesting))
      this.progress.content = `${this.progress.chunks.map(chunk => chunk.text).join("")} · Применяем сервер`;
    const activeRows = [...this.tasks.values()].filter(task => task.action !== "status");
    if (activeRows.length > 1) this.progress.content = activeRows.map(task =>
      cell(`${frames[this.frame % 10]} ${names[task.action] ?? "Ping → сайт"} · ${task.phase || "выполняется"} · [j] задачи`, width).trimEnd()).join("\n");
    const inlineProgress = this.screen === "home" && this.inlineSpeed() && this.homeTesting && !switching;
    this.progress.visible = this.progress.chunks.some(chunk => chunk.text.length > 0) && !inlineProgress;
    this.progress.height = this.progress.visible ? Math.max(1, activeRows.length) : 0;
  }
  override paint() {
    if (this.disposed) return;
    if (this.input.value !== this.editorValue) this.input.value = this.editorValue;
    this.syncAnimation();
    const width = Math.max(10, this.renderer.terminalWidth - 2),
      state = connection(this.status);
    this.headline.content = cell(
      `◈ VPNKIT${this.demo ? " · ДЕМО" : ""}${this.nativeMode() ? (this.nativeInstalled() ? " · NATIVE" : " · NATIVE PREVIEW") : ""}    ${state.title}`,
      width,
    );
    this.headline.fg = state.color;
    const compactServers = this.screen === "servers" && this.inlineSpeed();
    this.facts.visible = !compactServers;
    this.facts.height = compactServers ? 0 : 1;
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
      this.nativeMode()
        ? `Native-служба: ${gatewayNames[gateway ?? ""] ?? "неизвестно"} · runtime: native · Режим: ${this.status?.routing_mode === "smart" ? "умный" : this.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}`
        : `Docker: ${gatewayNames[gateway ?? ""] ?? "неизвестно"} · Режим: ${this.status?.routing_mode === "smart" ? "умный" : this.status?.routing_mode === "strict" ? "строгий" : "неизвестно"}`,
      width,
    );
    const editor = this.screen === "subscription" || this.screen === "target" || this.screen === "profile-name" || this.screen === "profile-source";
    this.input.visible = editor;
    this.editorHint.visible = editor;
    if (editor && !this.closing) this.input.focus();
    else this.input.blur();
    this.editorHint.content =
      "Enter сохранить · Esc назад · Ctrl+U очистить\nЗначение видно только здесь; в журнал не попадает.";
    const items = this.items();
    this.menuView.update(items, editor, this.selected,
      this.screen !== "servers" || this.serverMenu || !this.servers.length);
    this.summary.height = this.screen === "diagnostics" ? 6 : 2;
    if (this.screen === "tasks") this.summary.content = this.tasks.size ? "Текущие задачи" : "Нет выполняющихся задач";
    if (this.screen === "cancel-confirm") this.summary.content = "Точно отменить эту операцию?";
    if (this.screen === "retry-confirm") this.summary.content = "Точно повторить изменение VPN?";
    if (this.screen === "recovery") {
      this.summary.height = 3;
      const stages: Record<string, string> = { docker: "Docker", routes: "Маршруты", profile: "Профиль KDE", dns: "DNS", server: "Сервер", management: "Служба управления", input: "Ввод данных", unknown: "Операция" };
      this.summary.content = `${stages[this.recovery?.stage ?? "unknown"]}\n${this.recovery?.message ?? "Проверьте состояние."}`;
    }
    if (this.screen === "confirm") {
      if (this.confirmation === "subscription") {
        this.summary.height = 4;
        this.summary.content =
          "Настроить native-пакет?\nСистемное окно Polkit попросит подтверждение администратора.\nПервичный выбор сервера может занять несколько минут.\nVPN не запускается; автозапуск не изменяется.";
      } else {
        this.summary.content =
          this.confirmation === "disconnect"
            ? "Точно хотите отключить VPN?"
            : `Точно хотите остановить ${this.serviceObject()} и отключить VPN?`;
      }
    }
    if (this.screen === "home")
      this.summary.content = !this.status
        ? "Проверяем состояние…"
        : this.nativePreviewOnly()
          ? this.nativePreview()
          : !this.ready()
            ? `Подписку можно настроить до запуска ${this.nativeMode() ? "сервиса" : "Docker"}.`
            : this.status.subscription !== "configured"
              ? "Добавьте подписку, затем откройте серверы."
              : "";
    this.summary.visible = this.screen !== "home" || this.summary.chunks.some(chunk => chunk.text.length > 0);
    if (!this.summary.visible) this.summary.height = 0;
    this.paintMeasurement();
    if (this.screen === "autostart") {
      const nativeAutostart = this.nativeMode() || this.autostart?.runtime === "native";
      const results: Record<string, string> = { ready: "выполнен", failed: "ошибка, откройте диагностику", "prerequisites-unavailable": `${nativeAutostart ? "Native-служба" : "Docker"} или сеть не готовы`, cancelled: "отменён", disabled: "отключён", running: "выполняется" };
      const login = nativeAutostart
        ? "Автозапуск native-службы при входе в графическую сессию"
        : "При входе в KDE";
      this.summary.height = nativeAutostart ? 5 : 4;
      this.summary.content = `${login}\nОтключение автозапуска не отключает работающий VPN.\nПоследний запуск: ${results[this.autostart?.last_result ?? ""] ?? "не выполнялся"}${nativeAutostart ? "\nРежим: native-служба" : ""}\n${this.serviceStateLabel()}`;
    }
    if (this.screen === "subscription") {
      this.summary.height = this.nativeInstalled() ? 4 : this.nativeMode() ? 3 : 2;
      this.summary.content = this.nativeInstalled()
        ? "Первичная настройка native-пакета\nСистемное окно Polkit попросит подтверждение администратора.\nВыбор первого сервера может занять несколько минут.\nПосле успеха нажмите «Подключить VPN» отдельно; автозапуск включается отдельно и не включается сам."
        : this.nativeMode()
          ? "Подписка\nВ native preview чтение и применение подписки пока не подключены."
          : this.busy === "subscription/read"
            ? "Подписка · загрузка сохранённого адреса…"
            : "Подписка";
    }
    if(this.screen==="profiles") this.summary.content="Профили · у каждого свой список серверов\nВыбор профиля не меняет активное VPN-подключение.";
    if(this.screen==="profile-name") this.summary.content="Новый профиль · имя\nНапример: Моя VPS или Основная подписка";
    if(this.screen==="profile-source") this.summary.content=`${this.profileName} · ссылка\nHTTPS-подписка, vless://, hysteria2:// или hy2://`;
    if(this.screen==="profile-remove") this.summary.content="Удалить выбранный профиль?\nЭто не останавливает уже работающий сервер.";
    if (this.screen === "target")
      this.summary.content =
        "Адрес для проверки доступности\nHTTPS-запрос через каждый сервер; это не тест скорости.";
    if (this.screen === "diagnostics")
      this.summary.content = this.nativeMode()
        ? `OpenVPN / KDE: ${this.status?.networkmanager_active === "yes" ? "активен" : this.status?.networkmanager_active === "no" ? "отключён" : "неизвестно"}\nNative-служба: ${gatewayNames[gateway ?? ""] ?? "неизвестно"}\nПрофиль KDE: ${this.status?.networkmanager_configured === "yes" ? "настроен" : "не подтверждён"}\nПроверка: ${this.status?.diagnostics === "available" ? "выполнена" : "не запускалась"}\nПоследняя попытка: ${this.status?.last_attempt ?? "—"}\nПроверки сайта и скорости — на экране серверов.`
        : `OpenVPN / KDE: ${this.status?.networkmanager_active === "yes" ? "активен" : this.status?.networkmanager_active === "no" ? "отключён" : "неизвестно"}\nDocker: ${gatewayNames[gateway ?? ""] ?? "неизвестно"}\nПрофиль KDE: ${this.status?.networkmanager_configured === "yes" ? "настроен" : "не подтверждён"}\nПроверка: ${this.status?.diagnostics === "available" ? "выполнена" : "не запускалась"}\nПоследняя попытка: ${this.status?.last_attempt ?? "—"}\nПроверка сайта и скорости — на экране серверов.`;
    if (this.screen === "servers")
      this.summary.content = this.nativePreviewOnly()
        ? !this.ready()
          ? `Native-служба не готова. Запустите ${this.serviceObject()} для жизненного цикла VPN.\nВыбор серверов и проверки пока не подключены.`
          : "Native preview: выбор серверов и проверки пока не подключены.\nЗапуск/остановка VPN и автозапуск доступны на главной."
        : !this.ready()
          ? `Для проверок запустите ${this.serviceObject()}. VPN подключать не требуется.`
          : this.status?.subscription !== "configured"
            ? "Добавьте подписку для загрузки серверов."
            : `${this.profiles?.items.find(p=>p.id===this.profiles?.active)?.name ?? "Серверы"} · ${this.servers.length} серверов · Сортировка [o]: ${{ name: "имя ↑", ping: "ping ↑", speed: "скорость ↓", availability: "доступность ↓" }[this.sort]}${this.catalogStale ? " · каталог устарел" : ""}\nСайт: ${this.target}`;
    if (compactServers && !this.profiles && !this.nativePreviewOnly() && this.ready() && this.status?.subscription === "configured" && !this.catalogStale) {
      this.summary.visible = false;
      this.summary.height = 0;
    }
    this.paintTable();
    this.paintProgress();
    this.notification.content = this.closing
      ? "Завершаем текущую операцию перед выходом…"
      : this.notice + (this.error && this.recovery && !this.recovery.actions.includes("reapply") ? "\n[f] Действия" : "") +
        (this.error && this.noticeAttempt
          ? `\nПопытка: ${this.noticeAttempt}`
          : "");
    this.notification.fg = this.error ? p.red : p.muted;
    const hasNotice = this.closing || !!this.notice;
    this.notification.visible = hasNotice;
    this.notification.height = hasNotice ? (this.error && this.recovery && this.noticeAttempt ? 3 : 2) : 0;
    this.footer.content = editor
      ? "Esc назад · Ctrl+C выход"
      : this.screen === "servers"
        ? "Tab меню/список · ↑↓ · Enter · o сортировка · j задачи · Esc назад · q выход"
        : "↑↓ / Tab выбор · Enter · Esc назад · k отменить · q выход";
  }
  override onClosing() { this.input.blur(); }
  override onBeforeDispose() {
    this.renderer.keyInput.off("keypress", this.onKey);
    this.renderer.off("resize", this.paint);
  }
  override async onDispose() {
    this.renderer.destroy();
    await this.rendererClosed;
  }
}
