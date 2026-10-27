"""Generate overlay/assets/cosmetics/mane.png, Dabling's old mane as a head cosmetic.

The base Dabling (overlay/assets/Dabling.png) is bald, drawn from
resources/DablingHairless.png. The mane is cut out of the original drawing,
resources/Dabling.png: every pixel where the original differs from the bald one.
Worn over the bald Dabling it gives back the original look exactly, apart from
the hue tint, which only the body gets (cosmetics are drawn untinted).

Run from the repo root: python3 tools/gen_mane.py  (needs Pillow)
"""
from PIL import Image, ImageChops

ORIGINAL = "resources/Dabling.png"
BALD = "resources/DablingHairless.png"
OUT = "overlay/assets/cosmetics/mane.png"

original = Image.open(ORIGINAL).convert("RGBA")
bald = Image.open(BALD).convert("RGBA")
if original.size != bald.size:
    raise SystemExit(f"{BALD} is {bald.size[0]}x{bald.size[1]}, but {ORIGINAL} is {original.size[0]}x{original.size[1]}")

# Any channel differing by more than a rounding error counts as mane, alpha
# included: black mane over the bald one's empty (transparent black) pixels
# only differs in alpha.
bands = ImageChops.difference(original, bald).split()
diff = bands[0]
for band in bands[1:]:
    diff = ImageChops.lighter(diff, band)
diff = diff.point(lambda v: 255 if v > 2 else 0)
mane = Image.new("RGBA", original.size, (0, 0, 0, 0))
mane.paste(original, mask=diff)

# Worn, it should give back the original. A few soft outline edges come out a
# touch darker, where a semi-transparent mane edge lands on the bald head's own
# outline edge; anything more than that means the cut went wrong.
worn = bald.copy()
worn.alpha_composite(mane)
bands = ImageChops.difference(worn, original).split()
off = bands[0]
for band in bands[1:]:
    off = ImageChops.lighter(off, band)
wrong = sum(off.histogram()[3:])
inked = sum(original.getchannel("A").histogram()[1:])
worst = off.getextrema()[1]
if wrong > inked * 0.002 or worst > 64:
    raise SystemExit(f"the mane over the bald Dabling doesn't match the original: {wrong} pixels off, by up to {worst}")
mane.save(OUT, optimize=True)
print(f"wrote {OUT}: {mane.getchannel('A').getbbox()}")
