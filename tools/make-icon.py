"""Render the program's existing tray symbol at Windows icon sizes."""
from pathlib import Path
from PIL import Image, ImageDraw

root = Path(__file__).resolve().parent.parent
image = Image.new("RGBA", (256, 256))
draw = ImageDraw.Draw(image)
draw.rounded_rectangle((4, 4, 252, 252), radius=44, fill=(55, 99, 48))
draw.line([(67, 188), (185, 70)], fill=(215, 232, 164), width=23)
draw.line([(119, 70), (185, 70), (185, 136)], fill=(215, 232, 164), width=23)
image.save(root / "installer" / "LinkWeaver.ico", sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
