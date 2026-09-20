// The backend returns an average after its existing download test. Animate only
// the transition to that measured result; never invent live throughput samples.
export function speedometer(value: number | undefined, needle: number, compact: boolean): string {
  const caption = value === undefined ? '— Мбит/с' : `${value.toFixed(1)} Мбит/с`;
  if (compact) return `СКОРОСТЬ СЕРВЕРА\n${caption}`;
  const scale = value === undefined ? 100 : Math.max(100, 10 ** Math.ceil(Math.log10(Math.max(1, value))));
  const w = 43, h = 11, cx = 21, cy = 9, rx = 17, ry = 7;
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
  grid[10] = [...`    0${' '.repeat(Math.max(1, 33 - String(scale).length))}${scale}`.padEnd(w)];
  return grid.map(row => row.join('')).join('\n') + '\n' + center(caption);
}
