# TUI captures

The PNGs are captured from the real `App` using OpenTUI's test renderer and a
fake backend with public demonstration data. Colors, text and cell positions
come from `captureSpans`; measurements are illustrative, not a performance claim.
No real backend, subscription, server address or key is read.

To regenerate from the repository root (Bun, Python/Pillow and Noto Sans Mono):

```sh
(cd scripts/vpnkit/tui && bun install --frozen-lockfile)
bun scripts/vpnkit/tui/capture-readme.ts .build/readme-captures
python3 docs/render-tui-captures.py .build/readme-captures /usr/share/fonts/noto/NotoSansMono-Regular.ttf
cp .build/readme-captures/*.png docs/images/
```

The English README uses the same captures because the current TUI labels are
Russian. The source fixture can be replayed without network/system services.
