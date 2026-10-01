"""Generate overlay/assets/tavern_bg.png, the backdrop for #tavern-area.

The strip is 1440x260 on the 1920x1080 canvas (left 25% -> right edge,
bottom 260px). Everything above the ground is transparent so the stream
shows through; the tavern is background fluff, drawn at a fixed size, then
shrunk so its top sits at TAVERN_TOP and its right edge at TAVERN_RIGHT.
It stands behind a grass/dirt ground the Dablings walk on (they stand
0-14px above the bottom edge).

Run from the repo root: python3 tools/gen_tavern_bg.py  (needs Pillow)
"""
import random

from PIL import Image, ImageDraw, ImageFilter

W, H = 1440, 260
S = 3  # supersample factor, downscaled at the end for smooth edges
OUT = "overlay/assets/tavern_bg.png"

rng = random.Random(7)
img = Image.new("RGBA", (W * S, H * S), (0, 0, 0, 0))
d = ImageDraw.Draw(img)


def P(*xy):
    return [v * S for v in xy]


def rect(x0, y0, x1, y1, fill, outline=None, width=0):
    d.rectangle(P(x0, y0, x1, y1), fill=fill, outline=outline, width=width * S)


def poly(points, fill, outline=None, width=0):
    pts = [(x * S, y * S) for x, y in points]
    d.polygon(pts, fill=fill)
    if outline:
        d.line(pts + [pts[0]], fill=outline, width=width * S, joint="curve")


def line(points, fill, width):
    d.line([(x * S, y * S) for x, y in points], fill=fill, width=width * S)


def ellipse(x0, y0, x1, y1, fill, outline=None, width=0):
    d.ellipse(P(x0, y0, x1, y1), fill=fill, outline=outline, width=width * S)


GROUND_Y = 206  # top of the grass line
TAVERN_TOP = 58  # chimney top; level with the top of the taskbar-free band
TAVERN_RIGHT = 1420  # right edge of the right-hand fence

# --- the tavern ------------------------------------------------------------
BX0, BX1 = 470, 970          # main hall
WALL_TOP, FLOOR_TOP = 64, 132
TIMBER = (64, 40, 24, 255)
PLASTER = (226, 208, 168, 255)
STONE = (128, 120, 110, 255)

# chimney (behind the roof)
rect(880, 2, 918, 70, (120, 70, 52, 255), TIMBER, 2)
rect(876, 0, 922, 8, (96, 56, 42, 255))
for i, (dx, dy, r) in enumerate([(-14, -2, 7), (-30, -4, 8)]):
    smoke = Image.new("RGBA", img.size, (0, 0, 0, 0))
    ImageDraw.Draw(smoke).ellipse(P(899 + dx - r, dy - r + 6, 899 + dx + r, dy + r + 6),
                                  fill=(220, 220, 220, 110 - i * 30))
    img.alpha_composite(smoke.filter(ImageFilter.GaussianBlur(3 * S)))
d = ImageDraw.Draw(img)

# stone ground floor
rect(BX0, FLOOR_TOP, BX1, GROUND_Y + 2, STONE, TIMBER, 3)
for row, y in enumerate(range(FLOOR_TOP + 4, GROUND_Y, 14)):
    off = 0 if row % 2 else 18
    for x in range(BX0 + 4 - off, BX1, 36):
        x0, x1 = max(x, BX0 + 3), min(x + 32, BX1 - 3)
        if x1 - x0 > 6:
            shade = rng.randint(-14, 14)
            rect(x0, y, x1, min(y + 11, GROUND_Y),
                 (130 + shade, 122 + shade, 112 + shade, 255))

# timber-framed upper floor (slight overhang)
rect(BX0 - 10, WALL_TOP, BX1 + 10, FLOOR_TOP, PLASTER, TIMBER, 4)
rect(BX0 - 12, FLOOR_TOP - 6, BX1 + 12, FLOOR_TOP + 2, TIMBER)
for x in range(BX0 + 30, BX1, 70):
    rect(x - 3, WALL_TOP, x + 3, FLOOR_TOP, TIMBER)
for x in range(BX0 + 30, BX1 - 70, 140):
    line([(x, WALL_TOP + 4), (x + 70, FLOOR_TOP - 6)], TIMBER, 5)
    line([(x + 140, WALL_TOP + 4), (x + 70, FLOOR_TOP - 6)], TIMBER, 5)

# roof
poly([(BX0 - 34, WALL_TOP + 4), (BX0 + 70, 16), (BX1 - 70, 16), (BX1 + 34, WALL_TOP + 4)],
     (122, 46, 40, 255), (70, 24, 20, 255), 3)
for i, y in enumerate(range(24, WALL_TOP, 9)):
    t = (y - 16) / (WALL_TOP - 12)
    xl, xr = BX0 + 70 - t * 104, BX1 - 70 + t * 104
    line([(xl, y), (xr, y)], (96, 34, 30, 255), 2)
    for x in range(int(xl) + (i % 2) * 12, int(xr), 24):
        line([(x, y), (x, y + 9)], (96, 34, 30, 255), 1)

# dormer windows in the roof
for cx in (600, 720, 840):
    poly([(cx - 22, 52), (cx, 30), (cx + 22, 52)], (110, 40, 34, 255), (70, 24, 20, 255), 2)
    rect(cx - 14, 40, cx + 14, 62, TIMBER)
    rect(cx - 10, 44, cx + 10, 60, (248, 210, 120, 255))
    line([(cx, 44), (cx, 60)], TIMBER, 2)


def window(cx, cy, w, h):
    glow = Image.new("RGBA", img.size, (0, 0, 0, 0))
    ImageDraw.Draw(glow).rectangle(P(cx - w, cy - h, cx + w, cy + h), fill=(255, 200, 90, 120))
    img.alpha_composite(glow.filter(ImageFilter.GaussianBlur(6 * S)))
    dd = ImageDraw.Draw(img)
    dd.rectangle(P(cx - w / 2, cy - h / 2, cx + w / 2, cy + h / 2),
                 fill=(250, 206, 112, 255), outline=TIMBER, width=3 * S)
    dd.line(P(cx, cy - h / 2, cx, cy + h / 2), fill=TIMBER, width=2 * S)
    dd.line(P(cx - w / 2, cy, cx + w / 2, cy), fill=TIMBER, width=2 * S)
    dd.rectangle(P(cx - w / 2 - 4, cy + h / 2, cx + w / 2 + 4, cy + h / 2 + 4), fill=TIMBER)


for cx in (535, 905):
    window(cx, 98, 34, 30)
for cx in (535, 640, 800, 905):
    window(cx, 166, 40, 34)
d = ImageDraw.Draw(img)

# door with a little porch roof
DX = 720
rect(DX - 30, 140, DX + 30, GROUND_Y + 2, TIMBER)
d.pieslice(P(DX - 26, 146, DX + 26, 198), 180, 360, fill=(92, 60, 34, 255))
rect(DX - 26, 172, DX + 26, GROUND_Y + 2, (92, 60, 34, 255))
for x in (DX - 13, DX, DX + 13):
    line([(x, 152), (x, GROUND_Y)], (70, 44, 26, 255), 2)
ellipse(DX + 14, 178, DX + 20, 184, (210, 170, 70, 255))
poly([(DX - 46, 142), (DX, 124), (DX + 46, 142)], (122, 46, 40, 255), (70, 24, 20, 255), 2)
for x in (DX - 40, DX + 40):
    rect(x - 3, 142, x + 3, GROUND_Y, TIMBER)
# lanterns either side of the door
for x in (DX - 54, DX + 54):
    glow = Image.new("RGBA", img.size, (0, 0, 0, 0))
    ImageDraw.Draw(glow).ellipse(P(x - 16, 140, x + 16, 172), fill=(255, 190, 80, 140))
    img.alpha_composite(glow.filter(ImageFilter.GaussianBlur(5 * S)))
    d = ImageDraw.Draw(img)
    rect(x - 5, 150, x + 5, 164, (255, 214, 120, 255), TIMBER, 2)
    line([(x, 140), (x, 150)], TIMBER, 2)

# hanging sign
line([(BX1 + 10, 112), (BX1 + 60, 112)], TIMBER, 4)
line([(BX1 + 20, 112), (BX1 + 20, 120)], (40, 30, 20, 255), 1)
line([(BX1 + 52, 112), (BX1 + 52, 120)], (40, 30, 20, 255), 1)
d.rounded_rectangle(P(BX1 + 12, 120, BX1 + 60, 150), radius=4 * S,
                    fill=(140, 96, 54, 255), outline=TIMBER, width=2 * S)
# tankard emblem
rect(BX1 + 26, 128, BX1 + 42, 145, (210, 180, 90, 255), (90, 60, 30, 255), 1)
d.arc(P(BX1 + 38, 131, BX1 + 49, 142), 270, 90, fill=(90, 60, 30, 255), width=2 * S)
ellipse(BX1 + 24, 124, BX1 + 44, 132, (250, 245, 230, 255))

# barrels and a bench out front
for bx in (BX0 + 16, BX0 + 46):
    ellipse(bx - 13, 176, bx + 13, GROUND_Y + 2, (122, 80, 44, 255), TIMBER, 2)
    line([(bx - 12, 184), (bx + 12, 184)], (60, 60, 64, 255), 2)
    line([(bx - 12, 198), (bx + 12, 198)], (60, 60, 64, 255), 2)
rect(BX1 - 120, 186, BX1 - 40, 191, (110, 72, 40, 255), TIMBER, 1)
for x in (BX1 - 112, BX1 - 48):
    rect(x - 2, 191, x + 2, GROUND_Y, TIMBER)

# fence
for x0, x1 in [(250, 440), (1000, 1190)]:
    for y in (178, 192):
        rect(x0, y, x1, y + 4, (150, 112, 70, 255), (90, 64, 40, 255), 1)
    for x in range(x0 + 4, x1, 26):
        poly([(x - 4, 172), (x, 166), (x + 4, 172), (x + 4, GROUND_Y + 2), (x - 4, GROUND_Y + 2)],
             (158, 120, 76, 255), (90, 64, 40, 255), 1)

# --- shrink the tavern and fences into place --------------------------------
tavern = img.crop((0, 0, W * S, (GROUND_Y + 2) * S))
bx0, by0, bx1, by1 = tavern.getbbox()
k = (GROUND_Y + 2 - TAVERN_TOP) / ((by1 - by0) / S)
tavern = tavern.crop((bx0, by0, bx1, by1))
tavern = tavern.resize((round(tavern.width * k), round(tavern.height * k)), Image.LANCZOS)
left = TAVERN_RIGHT * S - tavern.width
img = Image.new("RGBA", (W * S, H * S), (0, 0, 0, 0))
img.alpha_composite(tavern, (left, TAVERN_TOP * S))
d = ImageDraw.Draw(img)
DX = (left + (DX * S - bx0) * k) / S  # door position after the move

# --- ground: grass edge over a dirt floor ----------------------------------
DIRT = (122, 88, 56, 255)
rect(0, GROUND_Y + 8, W, H, DIRT)
for _ in range(900):  # pebbles and speckle
    x, y = rng.uniform(0, W), rng.uniform(GROUND_Y + 14, H)
    r = rng.uniform(0.8, 2.6)
    shade = rng.choice([(100, 70, 44, 255), (140, 104, 70, 255), (150, 140, 128, 255)])
    ellipse(x - r, y - r * 0.7, x + r, y + r * 0.7, shade)
# worn path in front of the door
d.ellipse(P(DX - 120, GROUND_Y + 2, DX + 120, GROUND_Y + 40), fill=(150, 112, 74, 255))
# grass band with a wavy lower edge
pts = [(0, GROUND_Y)]
for x in range(0, W + 1, 12):
    pts.append((x, GROUND_Y + 10 + 4 * rng.random()))
pts.append((W, GROUND_Y))
poly(pts, (86, 140, 66, 255))
d.ellipse(P(DX - 60, GROUND_Y - 2, DX + 60, GROUND_Y + 18), fill=(150, 112, 74, 255))
# grass tufts along the edge
for _ in range(700):
    x = rng.uniform(0, W)
    if abs(x - DX) < 55:
        continue
    y = GROUND_Y + rng.uniform(-1, 10)
    h = rng.uniform(4, 11)
    lean = rng.uniform(-3, 3)
    c = rng.choice([(70, 124, 54, 255), (98, 156, 72, 255), (60, 110, 48, 255)])
    line([(x, y), (x + lean, y - h)], c, 1)
# a few flowers
for _ in range(40):
    x = rng.uniform(0, W)
    if abs(x - DX) < 70:
        continue
    y = GROUND_Y + rng.uniform(-2, 6)
    c = rng.choice([(240, 220, 90, 255), (240, 240, 240, 255), (220, 120, 160, 255)])
    ellipse(x - 1.6, y - 1.6, x + 1.6, y + 1.6, c)

img = img.resize((W, H), Image.LANCZOS)
img.save(OUT, optimize=True)
print(f"wrote {OUT} ({W}x{H})")
