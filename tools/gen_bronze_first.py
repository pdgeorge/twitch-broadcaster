"""Generate overlay/assets/cosmetics/bronze_first.png, the bronze "1" shirt.

Auto-granted for a chatter's first "First Login Bonus" redeem. It reuses
shirtA's cut (same silhouette, sleeve cut-outs, collar and hem lines) so it
sits on the Dabling exactly like the other shirts, recolors the cloth bronze
with a soft sheen, and swaps the "A" for a bronze medallion with a "1".

Run from the repo root: python3 tools/gen_bronze_first.py  (needs Pillow)
"""
from PIL import Image, ImageDraw, ImageFont

SRC = "overlay/assets/cosmetics/shirtA.png"
OUT = "overlay/assets/cosmetics/bronze_first.png"

BRONZE = (184, 115, 51)
BRONZE_LIGHT = (222, 160, 92)
BRONZE_DARK = (110, 62, 24)
SHIRTA_RED = 200  # red channel of shirtA's flat cloth color

# shirtA's "A" emblem (with its outline) lives in this box.
LETTER_BOX = (220, 413, 270, 463)
MEDAL_CENTER = (245, 438)
MEDAL_R = 26
S = 4  # supersample factor for the medallion


def lerp(a, b, t):
    return tuple(round(x + (y - x) * t) for x, y in zip(a, b))


def cloth(x, y):
    """Bronze with a diagonal sheen across the chest, darkening to the hem."""
    shade = lerp(BRONZE, BRONZE_DARK, max(0.0, (y - 400) / 100) * 0.45)
    d = abs((x - 190) + (y - 380) * 0.6)
    if d < 26:
        shade = lerp(shade, BRONZE_LIGHT, (1 - d / 26) * 0.55)
    return shade


img = Image.open(SRC).convert("RGBA")
px = img.load()
W, H = img.size
for y in range(H):
    for x in range(W):
        r, g, b, a = px[x, y]
        if not a:
            continue
        in_letter = LETTER_BOX[0] <= x <= LETTER_BOX[2] and LETTER_BOX[1] <= y <= LETTER_BOX[3]
        if in_letter:
            px[x, y] = (*cloth(x, y), a)
        elif r > g + 40:  # red cloth, including where it blends into a line
            k = min(1.0, r / SHIRTA_RED)
            px[x, y] = (*(round(c * k) for c in cloth(x, y)), a)

# Medallion: drawn large and shrunk for smooth edges.
cx, cy, R = (v * S for v in (*MEDAL_CENTER, MEDAL_R))
medal = Image.new("RGBA", (W * S, H * S), (0, 0, 0, 0))
d = ImageDraw.Draw(medal)
d.ellipse((cx - R, cy - R, cx + R, cy + R), fill=BRONZE_DARK)
inner = R - 3 * S
d.ellipse((cx - inner, cy - inner, cx + inner, cy + inner), fill=BRONZE_LIGHT)
try:
    font = ImageFont.truetype("DejaVuSans-Bold.ttf", 36 * S)
except OSError:
    font = ImageFont.load_default(36 * S)
d.text((cx, cy + S), "1", font=font, fill=BRONZE_DARK, anchor="mm", stroke_width=S, stroke_fill=BRONZE_DARK)
medal = medal.resize((W, H), Image.LANCZOS)
img.alpha_composite(medal)

img.save(OUT, optimize=True)
print(f"wrote {OUT}")
