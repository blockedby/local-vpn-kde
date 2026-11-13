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
