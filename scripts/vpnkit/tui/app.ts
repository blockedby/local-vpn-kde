import { BoxRenderable, TextRenderable, InputRenderable, type CliRenderer } from "@opentui/core";
import type { Backend } from "./bridge";
import { palette as p } from "./model";
import { observeRendererClose } from "./renderer-lifecycle";
import { cell } from "./formatting";
import { ServerTableView } from "./server-table-view";
import { MenuView } from "./menu-view";
import { MeasurementView } from "./measurement-view";
import { HeaderView } from "./header-view";
import { SummaryView } from "./summary-view";
import { frames } from "./animation-frames";
import { AppState } from "./app-state";
import { names } from "./action-labels";
export { cell } from "./formatting";

export class App extends AppState {
  private root: BoxRenderable;
  private headerView: HeaderView;
  private content: BoxRenderable;
  private summaryView: SummaryView;
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
    this.headerView = new HeaderView(renderer, this.root);
    this.content = new BoxRenderable(renderer, {
      flexDirection: "column",
      flexGrow: 1,
      minHeight: 0,
      overflow: "hidden",
      marginTop: 1,
    });
    this.root.add(this.content);
    this.summaryView = new SummaryView(renderer, this.content);
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
    super.paintMeasurement();
    this.measurementView.update({
      home: this.screen === "home", inline: this.inlineSpeed(), servers: this.servers,
      testing: this.homeTesting, speed: this.homeSpeed, speedAt: this.homeSpeedAt,
      speedServer: this.homeSpeedServer, speedLabel: this.homeSpeedLabel,
    });
  }
  override paintTable() {
    super.paintTable();
    this.tableView.update({
      visible: this.screen === "servers" && this.ready() && !this.nativePreviewOnly(),
      servers: this.visibleServers(), rowCount: this.rowCount(), serverID: this.serverID,
      batch: this.batch, frame: this.frame, liveSpeeds: this.liveSpeeds,
    });
  }
  override paintProgress() {
    super.paintProgress();
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
    super.paint();
    this.headerView.update(this, this.demo);
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
    this.summaryView.update(this);
    this.paintMeasurement();
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
