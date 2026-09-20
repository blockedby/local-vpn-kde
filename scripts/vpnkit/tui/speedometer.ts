import { StyledText, fg } from "@opentui/core";

// The backend returns an average after its existing download test. Animate only
// the transition to that measured result; never invent live throughput samples.
export function speedometer(value: number | undefined, needle: number, compact: boolean): StyledText {
  const caption = value === undefined ? '— Мбит/с' : `${value.toFixed(1)} Мбит/с`;
  if (compact) return new StyledText([fg("#edf6ff")(`СКОРОСТЬ СЕРВЕРА\n${caption}`)]);
  const scale = value === undefined ? 100 : Math.max(100, 10 ** Math.ceil(Math.log10(Math.max(1, value))));
  const w = 25, h = 8, cx = 12, cy = 6, rx = 10, ry = 4;
  const grid = Array.from({ length: h }, () => Array(w).fill(' '));
  for (let i = 0; i <= 60; i++) {
    const a = Math.PI * (1 - i / 60);
    grid[Math.round(cy - ry * Math.sin(a))]![Math.round(cx + rx * Math.cos(a))] = i % 10 === 0 ? '┃' : '·';
  }
  for (let i = 0; i <= 6; i++) {
    const a = Math.PI * (1 - i / 6);
    grid[Math.round(cy - ry * Math.sin(a))]![Math.round(cx + rx * Math.cos(a))] = "┃";
  }
  if (value !== undefined) {
    const a = Math.PI * (1 - Math.min(1, Math.max(0, needle) / scale));
    for (let r = .12; r < .83; r += .045) {
      grid[Math.round(cy - ry * r * Math.sin(a))]![Math.round(cx + rx * r * Math.cos(a))] = Math.abs(Math.cos(a)) < .25 ? '│' : Math.abs(Math.sin(a)) < .25 ? '─' : Math.cos(a) > 0 ? '╱' : '╲';
    }
    grid[Math.round(cy - ry * .85 * Math.sin(a))]![Math.round(cx + rx * .85 * Math.cos(a))] = '◆';
    grid[cy]![cx] = '●';
  }
  const center = (s: string) => s.padStart(Math.floor((w + s.length) / 2)).padEnd(w);
  grid[0] = [...center('СКОРОСТЬ СЕРВЕРА')];
  grid[1] = [...center(String(scale / 2))];
  grid[7]![cx - rx] = '0';
  [...String(scale)].forEach((ch, i) => { grid[7]![cx + rx - 1 + i] = ch; });
  const color = (x: number) => {
    const t = Math.max(0, Math.min(1, (x - (cx - rx)) / (2 * rx)));
    const left = t < .5 ? [255, 105, 120] : [245, 248, 255];
    const right = t < .5 ? [245, 248, 255] : [104, 230, 155];
    const f = t < .5 ? t * 2 : (t - .5) * 2;
    return '#' + left.map((v, i) => Math.round(v + (right[i]! - v) * f).toString(16).padStart(2, '0')).join('');
  };
  return new StyledText([
    ...grid.flatMap((row, y) => [
      ...row.map((ch, x) => fg(y < 2 ? '#edf6ff' : color(x))(ch)),
      fg('#edf6ff')('\n'),
    ]),
    fg('#edf6ff')(center(caption)),
  ]);
}
