import { BoxRenderable, type CliRenderer } from "@opentui/core";
import type { Backend } from "./bridge";
import { palette as p } from "./model";
import { observeRendererClose } from "./renderer-lifecycle";
import { ServerTableView } from "./server-table-view";
import { MenuView } from "./menu-view";
import { MeasurementView } from "./measurement-view";
import { HeaderView } from "./header-view";
import { SummaryView } from "./summary-view";
import { ProgressView } from "./progress-view";
import { NoticeFooterView } from "./notice-footer-view";
import { EditorView } from "./editor-view";
import { AppState } from "./app-state";
export { cell } from "./formatting";

export class App extends AppState {
  private root: BoxRenderable;
  private headerView: HeaderView;
  private content: BoxRenderable;
  private summaryView: SummaryView;
  private menuView: MenuView;
  private tableView: ServerTableView;
  private editorView: EditorView;
  private progressView: ProgressView;
  private noticeFooterView: NoticeFooterView;

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
    this.editorView = new EditorView(renderer, this.content, value => this.edit(value));
    this.progressView = new ProgressView(renderer, this.root);
    this.noticeFooterView = new NoticeFooterView(renderer, this.root);
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
    this.progressView.update(this);
  }
  override paint() {
    if (this.disposed) return;
    super.paint();
    this.headerView.update(this, this.demo);
    const editor = this.screen === "subscription" || this.screen === "target" || this.screen === "profile-name" || this.screen === "profile-source";
    this.editorView.update(this.editorValue, editor, this.closing);
    const items = this.items();
    this.menuView.update(items, editor, this.selected,
      this.screen !== "servers" || this.serverMenu || !this.servers.length);
    this.summaryView.update(this);
    this.paintMeasurement();
    this.paintTable();
    this.paintProgress();
    this.noticeFooterView.update(this, editor);
  }
  override onClosing() { this.editorView.blur(); }
  override onBeforeDispose() {
    this.renderer.keyInput.off("keypress", this.onKey);
    this.renderer.off("resize", this.paint);
  }
  override async onDispose() {
    this.renderer.destroy();
    await this.rendererClosed;
  }
}
