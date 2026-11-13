export const safeText = (s: string) =>
  s.replace(/[\x00-\x1f\x7f-\x9f\u202a-\u202e\u2066-\u2069]/g, "");
export function cell(text: string, width: number) {
  let out = "";
  for (const part of new Intl.Segmenter().segment(safeText(text))) {
    if (Bun.stringWidth(out + part.segment) > width) break;
    out += part.segment;
  }
  if (out !== safeText(text) && width > 1) {
    while (Bun.stringWidth(out) > width - 1)
      out = Array.from(out).slice(0, -1).join("");
    out += "…";
  }
  return out + " ".repeat(Math.max(0, width - Bun.stringWidth(out)));
}

export function wrapText(text: string, width: number): string[] {
  const lines: string[] = [];
  let line = "";
  for (const word of safeText(text.replace(/\s+/g, " ")).trim().split(" ")) {
    if (!word) continue;
    const next = line ? `${line} ${word}` : word;
    if (line && Bun.stringWidth(next) > width) { lines.push(line); line = word; }
    else line = next;
  }
  if (line) lines.push(line);
  return lines.map(value => cell(value, width).trimEnd());
}
