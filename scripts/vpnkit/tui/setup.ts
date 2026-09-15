import { BoxRenderable, TextRenderable, TextAttributes, type CliRenderer, type KeyEvent } from "@opentui/core";
import { palette as p } from "./model";
import type { SetupBackend, SetupResult } from "./setup-backend";

const steps = ["Образ Docker", "Права для настройки сети", "Ключи и конфигурация", "Системные маршруты", "Подготовка шлюза", "Профиль KDE", "Проверка установки"];
const phaseIndex: Record<string, number> = { "setup-assets": 2, "setup-underlay": 3, "setup-gateway": 4, "setup-profile": 5, "setup-verify": 6 };
const frames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const reasons: Record<string, string> = {
  "foreign-profile": "Профиль vpnkit-local уже существует. Принадлежность старой установке не подтверждена.",
  "previous-profile-active": "Отключи локальный VPN перед обновлением профиля и повтори установку.",
  "underlay-install-failed": "Не удалось настроить системные маршруты. Повтори шаг; права будут запрошены заново.",
  "underlay-verify-failed": "Проверка системных маршрутов не прошла. Подробности в журнале установки.",
  "profile-import-failed": "Не удалось импортировать профиль KDE. Подробности в журнале установки.",
  "assets-failed": "Не удалось подготовить ключи и конфигурацию. Проверь приватный каталог secrets/.",
  "gateway-start-failed": "Шлюз не запустился. Подробности в журнале установки.",
  "docker-registry-failed": "Не удалось загрузить образ. Проверь доступ к реестру Docker.",
  "gateway-build-failed": "Сборка образа Docker завершилась ошибкой. Подробности в журнале.",
  "sudo-failed": "Права не получены. Повтори шаг, чтобы снова ввести пароль в терминале.",
  "backend-unavailable": "Не удалось запустить установщик. Повтори ./install.sh.",
  "diagnostics-unavailable": "Не удалось создать приватный журнал. Проверь права на secrets/.",
  cancelled: "Установка остановлена. Завершённые изменения сохранены; можно повторить установку.",
  timeout: "Шаг превысил время ожидания. Подробности в журнале установки.",
};

export class SetupApp {
  private root: BoxRenderable;
  private title: TextRenderable;
  private subtitle: TextRenderable;
  private rows: TextRenderable[] = [];
  private progress: TextRenderable;
  private detail: TextRenderable;
  private log: TextRenderable;
  private actions: TextRenderable[] = [];
  private footer: TextRenderable;
  private timer?: ReturnType<typeof setInterval>;
  private busy = false;
  private done = false;
  private disposed = false;
  private view: "intro" | "steps" | "confirm" = "intro";
  private selected = 0;
  private step = 0;
  private imageReady = false;
  private result?: SetupResult;
  private frame = 0;
  private started = 0;
  private pending?: Promise<void>;
  private cancelRequested = false;
  private suspended = false;
  private attempted = false;

  constructor(private renderer: CliRenderer, private backend: SetupBackend,
    private authorize: () => Promise<boolean>, private finish: (open: boolean) => void,
    private demo = false) {
    this.root = new BoxRenderable(renderer, { width: "100%", height: "100%", paddingX: 2, justifyContent: "center", alignItems: "center", backgroundColor: p.base });
    renderer.root.add(this.root);
    const card = new BoxRenderable(renderer, { width: 68, maxWidth: "100%", flexDirection: "column", padding: 1, border: true, borderStyle: "rounded", borderColor: p.edge, backgroundColor: p.panel });
    this.root.add(card);
    const text = (content = "", color = p.text, height = 1) => {
      const row = new TextRenderable(renderer, { content, fg: color, height, flexShrink: 0 });
      card.add(row); return row;
    };
    this.title = text("LOCAL VPN  /  Установка", p.accent);
    this.title.attributes = TextAttributes.BOLD;
    this.subtitle = text("", p.muted, 2);
    for (let i = 0; i < steps.length; i++) this.rows.push(text());
    this.progress = text("", p.accent, 2);
    this.detail = text("", p.text, 3);
    this.log = text("", p.muted, 2);
    for (let i = 0; i < 2; i++) {
      const row = text();
      row.onMouseDown = (event) => { if (event.button === 0) { this.selected = i; this.activate(); } };
      this.actions.push(row);
    }
    this.footer = text("", p.muted, 2);
    renderer.keyInput.on("keypress", this.onKey);
    renderer.on("resize", this.paint);
    this.paint();
  }

  start() { this.timer = setInterval(() => { this.frame++; this.paint(); }, 100); }
  async run(): Promise<void> {
    if (this.busy || this.disposed) return;
    this.attempted = true;
    this.busy = true; this.done = false; this.view = "steps"; this.result = undefined;
    this.cancelRequested = false; this.started = Date.now(); this.selected = 0;
    const work = async () => {
      if (!this.imageReady) {
        this.step = 0; this.paint();
        const result = await this.backend.run("image", () => {});
        if (!result.ok) { this.result = result; return; }
        this.imageReady = true;
      }
      if (this.cancelRequested) { this.result = { ok:false, reason:"cancelled" }; return; }
      this.step = 1; this.paint();
      if (!await this.authorize()) { this.result = { ok:false, reason:"sudo-failed" }; return; }
      if (this.cancelRequested) { this.result = { ok:false, reason:"cancelled" }; return; }
      this.step = 2; this.paint();
      this.result = await this.backend.run("install", phase => {
        if (phaseIndex[phase] !== undefined) { this.step = phaseIndex[phase]; this.paint(); }
      });
      this.done = this.result.ok;
    };
    this.pending = work().catch(() => { this.result = { ok:false, reason:"backend-unavailable" }; }).finally(() => {
      this.busy = false; this.pending = undefined;
      if (this.view === "confirm") this.view = "steps";
      this.selected = 0; this.paint();
    });
    await this.pending;
  }

  private labels(): string[] {
    if (this.view === "confirm") return ["Нет, продолжить установку", "Да, остановить"];
    if (this.busy) return this.view === "intro" ? ["Ход установки", "Отменить установку"] : ["Назад", "Отменить установку"];
    if (this.done) return ["Открыть программу", "Закрыть"];
    if (this.result) return ["Повторить установку", "Назад"];
    return ["Начать установку", "Выйти"];
  }
  private activate() {
    if (this.view === "confirm") {
      if (this.selected === 1) { this.cancelRequested = true; this.backend.cancel(); }
      this.view = "steps"; this.selected = 0; this.paint(); return;
    }
    if (this.busy) {
      this.view = this.selected === 1 ? "confirm" : this.view === "intro" ? "steps" : "intro";
      this.selected = 0; this.paint(); return;
    }
    if (this.selected === 0) {
      if (this.done) this.finish(true); else void this.run();
    } else if (this.result && !this.done) {
      this.view = "intro"; this.result = undefined; this.selected = 0; this.paint();
    } else this.finish(false);
  }
  private onKey = (key: KeyEvent) => {
    if (this.disposed) return;
    if (key.name === "up" || key.name === "down" || key.name === "tab") { this.selected = 1-this.selected; this.paint(); return; }
    if (key.name === "return" || key.name === "enter") { this.activate(); return; }
    if (key.name === "escape") {
      if (this.view === "confirm") this.view = "steps";
      else if (this.view === "steps") this.view = "intro";
      else if (!this.busy) this.finish(false);
      this.selected = 0; this.paint(); return;
    }
    if (key.name === "k" && this.busy || key.name === "q" || key.ctrl && key.name === "c") {
      if (this.busy) { this.view = "confirm"; this.selected = 0; this.paint(); }
      else this.finish(false);
    }
  };

  private paint = () => {
    if (this.disposed || this.suspended) return;
    const compact = this.renderer.height < 25;
    this.title.content = `LOCAL VPN  /  ${this.done ? "Готово" : "Установка"}${this.demo ? " · демо" : ""}`;
    this.subtitle.content = this.view === "confirm" ? "Остановить установку?" : this.done ? "Всё подготовлено. Подписка настраивается в программе." : "Docker-шлюз и профиль KDE · VPN подключается вручную";
    this.subtitle.height = compact ? 1 : 2;
    const showSteps = this.view === "steps" || this.done;
    this.rows.forEach((row, i) => {
      row.visible = showSteps && (this.renderer.height >= 23 || i === this.step);
      const complete = this.done || i < this.step;
      const current = i === this.step && !this.done;
      row.fg = complete ? p.green : current ? this.result && !this.result.ok ? p.red : p.accent : p.muted;
      const symbol = complete ? "✓" : current && this.busy ? frames[this.frame % frames.length] : current && this.result ? "!" : "·";
      row.content = `${symbol}  ${steps[i]}`;
    });
    this.progress.height = compact ? 1 : 2;
    this.progress.content = this.busy ? `${this.cancelRequested ? "Останавливаем операцию…" : steps[this.step]}  ·  ${Math.floor((Date.now()-this.started)/1000)} с` : this.done ? "✓ Установка завершена" : this.result ? "Установка не завершена" : "Одна установка — затем только ./run.sh";
    this.detail.fg = this.result && !this.result.ok ? p.red : p.text;
    this.detail.content = this.view === "confirm" ? "Дождёмся безопасного завершения текущей операции. Затем установку можно повторить." : this.result && !this.result.ok ? reasons[this.result.reason] ?? `Ошибка на этапе «${steps[this.step]}». Подробности в приватном журнале.` : this.done ? "Открой «Подписка», укажи URL и запусти шлюз. Подключение VPN — отдельное действие." : this.busy ? "Можно вернуться назад: операция продолжится. Пароль запрашивается только в системном терминале." : "Подготовим всё для подключения. Подписку можно добавить после установки.";
    this.log.visible = !!this.result?.attempt;
    this.log.content = this.result?.attempt ? `Журнал: secrets/vpnkit-local/diagnostics/\n${this.result.attempt}.log` : "";
    this.labels().forEach((label, i) => {
      this.actions[i].content = `${this.selected === i ? "›" : " "} ${label}`;
      this.actions[i].fg = this.selected === i ? p.accent : p.muted;
      this.actions[i].bg = this.selected === i ? p.edge : p.panel;
    });
    this.footer.height = compact ? 1 : 2;
    this.footer.content = "↑↓ / Tab — выбор · Enter · Esc — назад";
    this.renderer.requestRender();
  };
  setSuspended(value: boolean) { this.suspended = value; if (!value) this.paint(); }
  get exitCode() { return this.attempted && !this.done ? 1 : 0; }
  async close() {
    this.cancelRequested = true;
    if (this.busy) this.backend.cancel();
    await this.pending;
    this.disposed = true;
    clearInterval(this.timer);
    this.renderer.keyInput.off("keypress", this.onKey);
    this.renderer.off("resize", this.paint);
    this.root.destroyRecursively();
  }
}
