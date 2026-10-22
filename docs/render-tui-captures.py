#!/usr/bin/env python3
"""Rasterize real OpenTUI captureSpans frames, preserving cell colors/positions.
Requires Pillow and a monospace font. Frames contain only demo fixture data.
Usage: python3 docs/render-tui-captures.py CAPTURE_DIRECTORY FONT_PATH
"""
import json
from pathlib import Path
import sys
from PIL import Image, ImageDraw, ImageFont

source = Path(sys.argv[1])
font = ImageFont.truetype(sys.argv[2], 20)
cell = round(font.getlength("M"))
line_height, padding = 28, 24
for name in ("home", "servers", "profiles"):
    frame = json.loads((source / f"{name}.json").read_text())
    image = Image.new("RGB", (frame["cols"] * cell + padding * 2, frame["rows"] * line_height + padding * 2), "#080e1a")
    draw = ImageDraw.Draw(image)
    for y, line in enumerate(frame["lines"]):
        x = padding
        for span in line["spans"]:
            width = span["width"] * cell
            top = padding + y * line_height
            draw.rectangle((x, top, x + width - 1, top + line_height - 1), fill=tuple(span["bg"][:3]))
            draw.text((x, top), span["text"], font=font, fill=tuple(span["fg"][:3]))
            x += width
    image.save(source / f"{name}.png", optimize=True)
