import { BoxRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { cell } from "./formatting";
import { palette as p } from "./model";

export interface MenuEntry { key: string; label: string }

/** Owns menu rows and sends row activations back to the application. */
export class MenuView {
  private controls: BoxRenderable;
  private rows: BoxRenderable[] = [];
  private labels: TextRenderable[] = [];

  constructor(private renderer: CliRenderer, parent: BoxRenderable, activate: (index: number) => void) {
    this.controls = new BoxRenderable(renderer, { flexDirection: "column", flexShrink: 0 });
    parent.add(this.controls);
    for (let i = 0; i < 8; i++) {
      const row = new BoxRenderable(renderer, {
        height: 1, flexShrink: 0,
        onMouseDown: (event) => { if (event.button === 0) activate(i); },
      });
      const label = new TextRenderable(renderer, { content: "", fg: p.text, height: 1, flexShrink: 0 });
      row.add(label);
      this.controls.add(row);
      this.rows.push(row);
      this.labels.push(label);
    }
  }

  update(items: MenuEntry[], editor: boolean, selected: number, highlight: boolean) {
    const width = Math.max(10, this.renderer.terminalWidth - 2);
    this.controls.visible = !editor;
    this.rows.forEach((row, i) => {
      row.visible = i < items.length;
      row.backgroundColor = i === selected && highlight ? p.edge : p.base;
      this.labels[i].content = items[i] ? cell(`[${items[i].key}] ${items[i].label}`, width).trimEnd() : "";
    });
  }
}
