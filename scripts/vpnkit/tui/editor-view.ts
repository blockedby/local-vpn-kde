import { BoxRenderable, InputRenderable, TextRenderable, type CliRenderer } from "@opentui/core";
import { safeText } from "./formatting";
import { palette as p } from "./model";

export class EditorView {
  private input: InputRenderable;
  private hint: TextRenderable;
  constructor(renderer: CliRenderer, parent: BoxRenderable, edit: (value: string) => void) {
    this.input = new InputRenderable(renderer, {
      width: "100%", maxLength: 16384, placeholder: "https://", backgroundColor: p.panel,
      textColor: p.text, focusedBackgroundColor: p.edge,
    });
    parent.add(this.input);
    this.input.on("input", () => {
      const clean = safeText(this.input.value);
      edit(clean);
      if (this.input.value !== clean) this.input.value = clean;
    });
    this.hint = new TextRenderable(renderer, { content: "", fg: p.muted, height: 2, flexShrink: 0 });
    parent.add(this.hint);
  }
  update(value: string, visible: boolean, closing: boolean) {
    if (this.input.value !== value) this.input.value = value;
    this.input.visible = visible;
    this.hint.visible = visible;
    if (visible && !closing) this.input.focus();
    else this.input.blur();
    this.hint.content = "Enter сохранить · Esc назад · Ctrl+U очистить\nЗначение видно только здесь; в журнал не попадает.";
  }
  blur() { this.input.blur(); }
}
