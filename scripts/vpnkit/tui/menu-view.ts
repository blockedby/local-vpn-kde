import { BoxRenderable, StyledText, TextRenderable, fg, type CliRenderer } from "@opentui/core";
import { cell } from "./formatting";
import type { MenuItem } from "./menu-actions";
import { palette as p } from "./model";

export function menuHeight(items: MenuItem[]) {
  return items.length + items.filter(item => item.dividerBefore).length;
}

/** Owns menu rows and sends row activations back to the application. */
export class MenuView {
  private controls: BoxRenderable;
  private rows: BoxRenderable[] = [];
  private labels: TextRenderable[] = [];
  private indices: (number | undefined)[] = [];

  constructor(private renderer: CliRenderer, parent: BoxRenderable, activate: (index: number) => void, scope = "workspace") {
    this.controls = new BoxRenderable(renderer, { flexDirection: "column", flexShrink: 0 });
    parent.add(this.controls);
    for (let i = 0; i < 16; i++) {
      const row = new BoxRenderable(renderer, {
        id: `menu:${scope}:row:${i}`, height: 1, flexShrink: 0,
        onMouseDown: (event) => { const index = this.indices[i]; if (event.button === 0 && index !== undefined) activate(index); },
      });
      const label = new TextRenderable(renderer, { content: "", fg: p.text, height: 1, flexShrink: 0, wrapMode: "none" });
      row.add(label);
      this.controls.add(row);
      this.rows.push(row);
      this.labels.push(label);
    }
  }

  update(items: MenuItem[], editor: boolean, selected: number, width = this.renderer.terminalWidth - 2, available = this.rows.length) {
    this.controls.visible = !editor;
    // Keep values and shortcuts near their titles on wide terminals.
    width = Math.min(width, 76);
    const lines: (number | string)[] = [];
    items.forEach((item, index) => {
      if (item.dividerBefore) lines.push(item.dividerBefore === true ? "─".repeat(width) : item.dividerBefore);
      lines.push(index);
    });
    const count = Math.min(this.rows.length, Math.max(1, available));
    const offset = Math.max(0, Math.min(lines.length - count, lines.indexOf(selected) - count + 1));
    const keyWidth = Math.max(1, ...items.map(item => Bun.stringWidth(item.key)));
    const bodyWidth = width - keyWidth - 4;
    const widestTitle = Math.max(0, ...items.map(item => item.value === undefined ? 0 : Bun.stringWidth(item.title ?? item.label)));
    const titleWidth = Math.min(bodyWidth - 11, Math.floor(bodyWidth * 0.7), widestTitle + 4);
    this.rows.forEach((row, i) => {
      const line = i < count ? lines[offset + i] : undefined;
      const index = typeof line === "number" ? line : undefined;
      this.indices[i] = index;
      row.visible = line !== undefined;
      row.backgroundColor = index === selected ? p.edge : p.base;
      row.width = width;
      if (line === undefined) return;
      const label = this.labels[i];
      label.fg = typeof line === "string" ? p.muted : p.text;
      if (typeof line === "string") { label.content = cell(line, width); return; }
      const item = items[line];
      const title = item.title ?? item.label;
      let body: string;
      if (item.value !== undefined) {
        const head = cell(title, titleWidth - 2).trimEnd();
        const leader = ".".repeat(titleWidth - Bun.stringWidth(head) - 2);
        const value = cell(item.value, bodyWidth - titleWidth - (item.kind === "toggle" ? 4 : 5));
        body = `${head} ${leader} ${item.kind === "toggle" ? `‹ ${value.trimEnd()} ›` : `  ${value}  →`}`;
      } else if (item.kind === "open") body = `${cell(title, bodyWidth - 3)}  →`;
      else body = title;
      label.content = new StyledText([fg(p.text)(`${line === selected ? "›" : " "} ${cell(body, bodyWidth)}`), fg(p.muted)(`  ${item.key.padStart(keyWidth)}`)]);
    });
  }
}
