import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import type { Backend } from "./bridge";
import { palette as p } from "./model";
import { observeRendererClose } from "./renderer-lifecycle";
import { ServerTableView } from "./server-table-view";
import { DetailsView, DETAILS_THRESHOLD, DETAILS_WIDTH } from "./details-view";
import { MenuView } from "./menu-view";
import { HeaderView } from "./header-view";
import { TabView } from "./tab-view";
import { OverlayView } from "./overlay-view";
import { SummaryView } from "./summary-view";
import { NoticeView } from "./notice-view";
import { FooterView } from "./footer-view";
import { EditorView } from "./editor-view";
import { AppState, isOverlay } from "./app-state";
export { cell } from "./formatting";

export class App extends AppState {
  private root: BoxRenderable;
  private headerView: HeaderView;
  private tabView: TabView;
  private overlayView: OverlayView;
  private content: BoxRenderable;
  private tooSmall: TextRenderable;
  private summaryView: SummaryView;
  private menuView: MenuView;
  private tableView: ServerTableView;
  private detailsView: DetailsView;
  private editorView: EditorView;
  private noticeView: NoticeView;
  private footerView: FooterView;
  private readonly rendererClosed: Promise<void>;
  constructor(private renderer: CliRenderer, backend: Backend, private demo = false) {
    super(backend);
    this.paint = this.paint.bind(this);
    this.rendererClosed = observeRendererClose(renderer);
    this.root = new BoxRenderable(renderer, { width: "100%", height: "100%", paddingX: 1, flexDirection: "column", backgroundColor: p.base });
    renderer.root.add(this.root);
    this.headerView = new HeaderView(renderer, this.root);
    this.tabView = new TabView(renderer, this.root, tab => { if (!isOverlay(this.screen) && !this.helpOpen) this.open(tab); });
    this.content = new BoxRenderable(renderer, { flexDirection: "column", flexGrow: 1, minHeight: 0, overflow: "hidden" }); this.root.add(this.content);
    this.tooSmall = new TextRenderable(renderer, { content: "Увеличьте терминал минимум до 60 × 16.", height: 1, fg: p.amber, visible: false }); this.content.add(this.tooSmall);
    this.summaryView = new SummaryView(renderer, this.content);
    this.menuView = new MenuView(renderer, this.content, index => {
      if (isOverlay(this.screen) || this.helpOpen) return;
      this.selected = index;
      const item = this.items()[index];
      const action = item && this.actions().find(candidate => candidate.keys.includes(item.key));
      if (action?.available) action.run();
      else if (action?.unavailableBehavior === "notice" && action.unavailableReason) this.notify(action.unavailableReason, true);
      this.paint();
    });
    const serverArea = new BoxRenderable(renderer, { flexDirection: "row", flexGrow: 1, minHeight: 0, overflow: "hidden" });
    this.content.add(serverArea);
    const tableArea = new BoxRenderable(renderer, { flexDirection: "column", flexGrow: 1, minWidth: 0, overflow: "hidden" });
    serverArea.add(tableArea);
    this.detailsView = new DetailsView(renderer, serverArea);
    this.tableView = new ServerTableView(renderer, tableArea, index => { if (isOverlay(this.screen) || this.helpOpen) return; const server = this.visibleServers()[index]; if (server) { this.serverID = server.server_id; this.syncServerViewport(); this.paint(); } });
    this.editorView = new EditorView(renderer, this.content, value => this.edit(value));
    this.noticeView = new NoticeView(renderer, this.root);
    this.footerView = new FooterView(renderer, this.root);
    this.overlayView = new OverlayView(renderer, this.root);
    renderer.keyInput.on("keypress", this.onKey); renderer.on("resize", this.onResize);
    this.paint();
  }
  private onResize = () => { this.syncServerViewport(); this.paint(); };
  override viewport() { return { width: this.renderer.width, height: this.renderer.height, terminalHeight: this.renderer.terminalHeight }; }
  override paintMeasurement() { super.paintMeasurement(); this.headerView.update(this, this.demo); this.detailsView.update(this, this.renderer.width >= DETAILS_THRESHOLD && (this.screen === "servers" || isOverlay(this.screen) && this.tab === "servers") && this.ready() && !this.nativePreviewOnly() && this.servers.length > 0); if (this.screen === "details") this.overlayView.update(this); }
  override paintTable() {
    super.paintTable();
    const tableVisible = (this.screen === "servers" || isOverlay(this.screen) && this.tab === "servers") && this.ready() && !this.nativePreviewOnly() && this.servers.length > 0;
    const wide = this.renderer.width >= DETAILS_THRESHOLD && tableVisible;
    this.detailsView.update(this, wide);
    this.tableView.update({ width: this.renderer.terminalWidth - 2 - (wide ? DETAILS_WIDTH : 0), visible: this.renderer.width >= 60 && this.renderer.height >= 16 && (this.screen === "servers" || isOverlay(this.screen) && this.tab === "servers") && this.ready() && !this.nativePreviewOnly() && this.servers.length > 0, servers: this.visibleServers(), rowCount: this.rowCount(), serverID: this.serverID, batch: this.batch, frame: this.frame, liveSpeeds: this.liveSpeeds, sort: this.sort });
    this.headerView.update(this, this.demo);
    if (this.screen === "details") this.overlayView.update(this);
  }
  override paintProgress() { super.paintProgress(); this.tabView.update(this); this.headerView.update(this, this.demo); }
  override paint() {
    if (this.disposed) return;
    super.paint();
    this.headerView.update(this, this.demo); this.tabView.update(this);
    const small = this.renderer.width < 60 || this.renderer.height < 16;
    this.tooSmall.visible = small;
    const overlayScreen = this.screen;
    const modal = isOverlay(overlayScreen);
    if (modal) this.screen = this.tab;
    const editor = !small && ["subscription", "target", "profile-name", "profile-source"].includes(this.screen);
    this.editorView.update(this.editorValue, editor && !this.helpOpen, this.closing);
    const items = small || this.screen === "servers" && this.ready() && !this.nativePreviewOnly() && this.status?.subscription === "configured" ? [] : this.items();
    this.menuView.update(items, editor || small, this.selected, true);
    this.summaryView.update(this);
    if (small) this.summaryView.hide();
    this.paintTable();
    this.screen = overlayScreen;
    this.noticeView.update(this); this.footerView.update(this, modal && ["subscription", "target", "profile-name", "profile-source"].includes(overlayScreen)); this.overlayView.update(this);
  }
  override onClosing() { this.editorView.blur(); }
  override onBeforeDispose() { this.renderer.keyInput.off("keypress", this.onKey); this.renderer.off("resize", this.onResize); }
  override async onDispose() { this.renderer.destroy(); await this.rendererClosed; }
}
