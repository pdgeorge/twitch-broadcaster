"""Generate overlay/assets/DablingTemplate.ora, a layered file to draw cosmetics on.

OpenRaster opens with its layers in Krita, GIMP and MyPaint. Top to bottom:
  Draw your item here           empty, where the new cosmetic goes
  Example: the mane (head)      hidden, how a hat-slot item sits
  Example: shirtA (torso)       hidden, how a shirt sits
  Dabling                       the bald base, a guide: hide it before exporting
  Background                    white: hide it before exporting

It's on the base's own 500x600 canvas, so whatever is drawn on the top layer
lines up with every Dabling on stream. In other apps, put
overlay/assets/Dabling.png under the drawing instead.

Run from the repo root: python3 tools/gen_dabling_template.py  (needs Pillow)
"""
import io
import zipfile
from xml.sax.saxutils import quoteattr

from PIL import Image

BASE = "overlay/assets/Dabling.png"
OUT = "overlay/assets/DablingTemplate.ora"

base = Image.open(BASE).convert("RGBA")
size = base.size

# (name, image, visible), top first, which is how OpenRaster stacks them.
layers = [
    ("Draw your item here", Image.new("RGBA", size, (0, 0, 0, 0)), True),
    ("Example: the mane (head slot)", Image.open("overlay/assets/cosmetics/mane.png").convert("RGBA"), False),
    ("Example: shirtA (torso slot)", Image.open("overlay/assets/cosmetics/shirtA.png").convert("RGBA"), False),
    ("Dabling (guide: hide before exporting)", base, True),
    ("Background (hide before exporting)", Image.new("RGBA", size, (255, 255, 255, 255)), True),
]
for name, image, _ in layers:
    if image.size != size:
        raise SystemExit(f"{name} is {image.size[0]}x{image.size[1]}, not the base's {size[0]}x{size[1]}")


def png(image):
    buf = io.BytesIO()
    image.save(buf, "PNG", optimize=True)
    return buf.getvalue()


merged = Image.new("RGBA", size, (0, 0, 0, 0))
for _, image, visible in reversed(layers):
    if visible:
        merged.alpha_composite(image)
thumb = merged.copy()
thumb.thumbnail((256, 256))

stack = [f'<image version="0.0.3" w="{size[0]}" h="{size[1]}">', "<stack>"]
with zipfile.ZipFile(OUT, "w", zipfile.ZIP_DEFLATED) as ora:
    # The spec wants the mimetype first and stored, so the file can be sniffed.
    ora.writestr(zipfile.ZipInfo("mimetype"), "image/openraster", compress_type=zipfile.ZIP_STORED)
    for i, (name, image, visible) in enumerate(layers):
        src = f"data/layer{i}.png"
        ora.writestr(src, png(image))
        attrs = f'name={quoteattr(name)} src="{src}" x="0" y="0" opacity="1.0" visibility="{"visible" if visible else "hidden"}"'
        if i == 0:
            attrs += ' selected="true"'
        stack.append(f"<layer {attrs}/>")
    stack += ["</stack>", "</image>"]
    ora.writestr("stack.xml", '<?xml version="1.0" encoding="UTF-8"?>\n' + "\n".join(stack) + "\n")
    ora.writestr("mergedimage.png", png(merged))
    ora.writestr("Thumbnails/thumbnail.png", png(thumb))
print(f"wrote {OUT}")
