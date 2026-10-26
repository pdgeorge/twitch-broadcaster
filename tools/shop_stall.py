# Procedural build of Dabi's shop stall, rendered as the 1920x1080 cartoon background of /shop.
# Same method as Dabi's own build (vtuber-george/blender/dabi_build.py): every piece is made in
# code, so changing a number and running it again gives a new render. Headless, from the repo root:
#   blender -b --factory-startup --python tools/shop_stall.py -- overlay/shop
# Add --transparent after the folder for the stall alone, with no sky or grass behind it.
# Writes stall.png and layout.json there. layout.json says where the item boxes, the price tags
# and the stool's seat land on screen, so /shop lines up with the art without measuring by hand.
# Blender 5.2 (vtuber-george's VTUBING.md says where it's installed).
import bpy, bmesh, math, json, os, sys, random
from mathutils import Vector, Matrix, noise
from bpy_extras.object_utils import world_to_camera_view

argv = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else []
OUT_DIR = argv[0] if argv else os.getcwd()
TRANSPARENT = "--transparent" in argv   # stall only, no sky/ground behind it
W, H = 1920, 1080
EYE_Z = 1.95   # camera height, so also where the horizon is
random.seed(7)

# ---------------------------------------------------------------- scene

for o in list(bpy.data.objects):
    bpy.data.objects.remove(o)
sc = bpy.context.scene
sc.render.engine = 'BLENDER_EEVEE'
sc.render.resolution_x, sc.render.resolution_y = W, H
sc.render.film_transparent = True
sc.view_settings.view_transform = 'Standard'
sc.view_settings.look = 'None'
sc.eevee.taa_render_samples = 32
sc.world = sc.world or bpy.data.worlds.new("World")
sc.world.color = (0, 0, 0)


def lin(hex_):
    """sRGB hex -> linear rgb, so colours come out as picked."""
    h = hex_.lstrip('#')
    out = []
    for i in (0, 2, 4):
        c = int(h[i:i + 2], 16) / 255
        out.append(c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4)
    return tuple(out)


SHADOW_TINT = (0.9, 1.25, 0.75)   # how hard each channel darkens in shadow: shadows go a little purple


def toon(name, hex_, shade=0.76, stripes=None):
    """Flat cartoon fill: lit or shadow, nothing in between. stripes=(hex2, width, x0) paints
    vertical stripes in object space starting at x0 (the valance)."""
    m = bpy.data.materials.new(name)
    if m.node_tree is None:
        m.use_nodes = True
    nt = m.node_tree
    nt.nodes.clear()
    out = nt.nodes.new('ShaderNodeOutputMaterial')
    dif = nt.nodes.new('ShaderNodeBsdfDiffuse')
    s2r = nt.nodes.new('ShaderNodeShaderToRGB')
    ramp = nt.nodes.new('ShaderNodeValToRGB')
    ramp.color_ramp.interpolation = 'CONSTANT'
    ramp.color_ramp.elements[0].color = (*(1 - (1 - shade) * t for t in SHADOW_TINT), 1)
    ramp.color_ramp.elements[1].position = 0.22
    ramp.color_ramp.elements[1].color = (1, 1, 1, 1)
    mul = nt.nodes.new('ShaderNodeMix')
    mul.data_type = 'RGBA'
    mul.blend_type = 'MULTIPLY'
    mul.inputs['Factor'].default_value = 1.0
    base = (*lin(hex_), 1)
    if stripes:
        tc = nt.nodes.new('ShaderNodeTexCoord')
        sep = nt.nodes.new('ShaderNodeSeparateXYZ')
        sub = nt.nodes.new('ShaderNodeMath'); sub.operation = 'SUBTRACT'; sub.inputs[1].default_value = stripes[2]
        div = nt.nodes.new('ShaderNodeMath'); div.operation = 'DIVIDE'; div.inputs[1].default_value = stripes[1]
        flo = nt.nodes.new('ShaderNodeMath'); flo.operation = 'FLOOR'
        mod = nt.nodes.new('ShaderNodeMath'); mod.operation = 'FLOORED_MODULO'; mod.inputs[1].default_value = 2
        pick = nt.nodes.new('ShaderNodeMix'); pick.data_type = 'RGBA'
        pick.inputs[6].default_value = base
        pick.inputs[7].default_value = (*lin(stripes[0]), 1)
        nt.links.new(tc.outputs['Object'], sep.inputs[0])
        nt.links.new(sep.outputs['X'], sub.inputs[0])
        nt.links.new(sub.outputs[0], div.inputs[0])
        nt.links.new(div.outputs[0], flo.inputs[0])
        nt.links.new(flo.outputs[0], mod.inputs[0])
        nt.links.new(mod.outputs[0], pick.inputs['Factor'])
        nt.links.new(pick.outputs[2], mul.inputs[6])
    else:
        mul.inputs[6].default_value = base
    em = nt.nodes.new('ShaderNodeEmission')
    nt.links.new(dif.outputs[0], s2r.inputs[0])
    nt.links.new(s2r.outputs['Color'], ramp.inputs['Fac'])
    nt.links.new(ramp.outputs['Color'], mul.inputs[7])
    nt.links.new(mul.outputs[2], em.inputs['Color'])
    nt.links.new(em.outputs[0], out.inputs['Surface'])
    m.diffuse_color = base
    return m


INK = '#1c1414'
# The boxes' backboard and its grain: a wood between the light frames and the dark back wall,
# so the white Dabling on it stands out and the box doesn't sink into the wall.
BACKBOARD = ('#c4874f', '#a66d3c')
CURRENT = ['Stall']   # the collection new objects and ink strokes go into
M = dict(
    plank=toon('Plank', '#b4713b'),
    post=toon('Post', '#99592c'),
    back=toon('BackWall', '#7a4626'),
    light=toon('LightWood', '#dda463'),
    roof=toon('Roof', '#8c4a26'),
    soffit=toon('Soffit', '#5a321c', shade=0.85),
    backboard=toon('Backboard', BACKBOARD[0], shade=1.0),   # never shaded: the Dabling and item sit on it
    paper=toon('Paper', '#fff8ea', shade=0.88),
    seat=toon('StoolSeat', '#c8864c'),
)


def link(o, coll_name=None):
    coll_name = coll_name or CURRENT[0]
    c = bpy.data.collections.get(coll_name)
    if not c:
        c = bpy.data.collections.new(coll_name)
        sc.collection.children.link(c)
    c.objects.link(o)
    return o


def mesh_obj(name, bm, mat, smooth=False):
    me = bpy.data.meshes.new(name)
    bm.to_mesh(me)
    bm.free()
    me.materials.append(mat)
    for p in me.polygons:
        p.use_smooth = smooth
    return link(bpy.data.objects.new(name, me))


def wobble(bm, amp=0.012, freq=1.3, flat=False):
    """Bend straight edges a little, the way a cartoon background is never quite square.
    flat keeps the movement in x/z, so a flat face stays flat."""
    off = Vector((random.uniform(0, 100), random.uniform(0, 100), random.uniform(0, 100)))
    for v in bm.verts:
        d = noise.noise_vector(v.co * freq + off) * amp
        if flat:
            d.y = 0
        v.co += d


def block(name, size, mat, loc=(0, 0, 0), rot=(0, 0, 0), cuts=6, amp=0.012, taper=0.0):
    """A box (planks, posts, boards) with subdivided edges so wobble() can bend them.
    taper narrows the top (x) by that fraction."""
    bm = bmesh.new()
    bmesh.ops.create_cube(bm, size=1.0)
    bmesh.ops.subdivide_edges(bm, edges=bm.edges[:], cuts=cuts, use_grid_fill=True)
    for v in bm.verts:
        k = 1.0 - taper * (v.co.z + 0.5)
        v.co = Vector((v.co.x * size[0] * k, v.co.y * size[1], v.co.z * size[2]))
    wobble(bm, amp)
    o = mesh_obj(name, bm, mat)
    o.location = loc
    o.rotation_euler = [math.radians(a) for a in rot]
    return o


def cylinder(name, r, h, mat, loc, rot=(0, 0, 0), r_top=None, segs=40, amp=0.008):
    bm = bmesh.new()
    bmesh.ops.create_cone(bm, cap_ends=True, segments=segs, radius1=r, radius2=r if r_top is None else r_top, depth=h)
    bmesh.ops.subdivide_edges(bm, edges=[e for e in bm.edges if abs(e.verts[0].co.z - e.verts[1].co.z) > 1e-4], cuts=5)
    wobble(bm, amp)
    o = mesh_obj(name, bm, mat, smooth=True)
    o.location = loc
    o.rotation_euler = [math.radians(a) for a in rot]
    return o


def prism(name, outline, depth, mat, y, amp=0.01):
    """Extrude a 2D outline [(x, z)] along y (front face at y, back at y+depth)."""
    bm = bmesh.new()
    front = [bm.verts.new((x, y, z)) for x, z in outline]
    back = [bm.verts.new((x, y + depth, z)) for x, z in outline]
    n = len(outline)
    bm.faces.new(front[::-1])
    bm.faces.new(back)
    for i in range(n):
        j = (i + 1) % n
        bm.faces.new((front[i], front[j], back[j], back[i]))
    bmesh.ops.recalc_face_normals(bm, faces=bm.faces[:])
    bmesh.ops.subdivide_edges(bm, edges=bm.edges[:], cuts=4)
    # a big flat outline triangulates into long thin slivers, which fold into creases the ink
    # picks up if they bend in depth, so only the outline wobbles
    wobble(bm, amp, flat=True)
    return mesh_obj(name, bm, mat)


# ---------------------------------------------------------------- brush strokes
# Hand-drawn detail (grain, nails, lettering, strings) is flat ribbon meshes with round ends,
# facing the camera. Not grease pencil: in Blender 5.2's EEVEE a grease pencil stroke draws
# over meshes that are in front of it. The ribbons live in their own "...Ink" collections,
# which no Freestyle line set picks, so they don't get outlined themselves.

FLAT_CACHE = {}
STROKES = {}  # collection -> [(points, radius, hex)], built in one go at the end


def flat(hex_):
    """Unlit colour, exactly as picked."""
    if hex_ not in FLAT_CACHE:
        m = bpy.data.materials.new("ink_" + hex_.lstrip('#'))
        if m.node_tree is None:
            m.use_nodes = True
        nt = m.node_tree
        nt.nodes.clear()
        em = nt.nodes.new('ShaderNodeEmission')
        em.inputs['Color'].default_value = (*lin(hex_), 1)
        nt.links.new(em.outputs[0], nt.nodes.new('ShaderNodeOutputMaterial').inputs['Surface'])
        FLAT_CACHE[hex_] = m
    return FLAT_CACHE[hex_]


def stroke(points, radius, hex_=INK):
    STROKES.setdefault(CURRENT[0], []).append(([Vector(p) for p in points], radius, hex_))


def flush_strokes():
    """One mesh per collection: a disc at every point (round ends and joins) and a quad
    between each pair, all in the x/z plane."""
    to_xz = Matrix.Rotation(math.radians(90), 4, 'X')
    for coll_name, strokes in STROKES.items():
        bm = bmesh.new()
        index = {}
        for pts, r, hex_ in strokes:
            mi = index.setdefault(hex_, len(index))
            faces = []
            for p in pts:
                ret = bmesh.ops.create_circle(bm, cap_ends=True, segments=20 if r > 0.02 else 12, radius=r)
                bmesh.ops.transform(bm, matrix=Matrix.Translation(p) @ to_xz, verts=ret['verts'])
                faces += {f for v in ret['verts'] for f in v.link_faces}
            for a, b in zip(pts, pts[1:]):
                d = Vector((b.x - a.x, 0, b.z - a.z))
                if d.length < 1e-6:
                    continue
                n = Vector((-d.z, 0, d.x)).normalized() * r
                faces.append(bm.faces.new([bm.verts.new(a + n), bm.verts.new(b + n), bm.verts.new(b - n), bm.verts.new(a - n)]))
            for f in faces:
                f.material_index = mi
        me = bpy.data.meshes.new(coll_name + "Ink")
        bm.to_mesh(me)
        bm.free()
        for hex_ in sorted(index, key=index.get):
            me.materials.append(flat(hex_))
        ob = link(bpy.data.objects.new(coll_name + "Ink", me), coll_name + "Ink")
        ob.visible_shadow = False


def catmull(pts, per=8):
    """Smooth a polyline through its points."""
    if len(pts) < 3:
        return pts
    P = [pts[0]] + pts + [pts[-1]]
    out = []
    for i in range(1, len(P) - 2):
        p0, p1, p2, p3 = P[i - 1], P[i], P[i + 1], P[i + 2]
        for k in range(per):
            t = k / per
            t2, t3 = t * t, t * t * t
            out.append(tuple(0.5 * ((2 * p1[d]) + (-p0[d] + p2[d]) * t + (2 * p0[d] - 5 * p1[d] + 4 * p2[d] - p3[d]) * t2 + (-p0[d] + 3 * p1[d] - 3 * p2[d] + p3[d]) * t3) for d in range(2)))
    out.append(pts[-1])
    return out


# Hand-lettered glyphs: strokes in a box 1 tall, (x, y). 's' strokes are smoothed, 'l' stay straight.
GLYPHS = {
    'D': (0.72, [('l', [(0, 0), (0, 1)]), ('s', [(0, 1), (0.36, 0.97), (0.64, 0.8), (0.72, 0.5), (0.64, 0.2), (0.36, 0.03), (0, 0)])]),
    'A': (0.76, [('l', [(0, 0), (0.38, 1), (0.76, 0)]), ('l', [(0.17, 0.38), (0.6, 0.38)])]),
    'B': (0.66, [('l', [(0, 0), (0, 1)]), ('s', [(0, 1), (0.4, 0.98), (0.56, 0.82), (0.46, 0.58), (0, 0.54)]), ('s', [(0, 0.54), (0.5, 0.5), (0.66, 0.27), (0.5, 0.04), (0, 0)])]),
    'I': (0.3, [('l', [(0.15, 0), (0.15, 1)]), ('l', [(0, 1), (0.3, 1)]), ('l', [(0, 0), (0.3, 0)])]),
    'S': (0.64, [('s', [(0.62, 0.84), (0.46, 0.98), (0.2, 0.96), (0.05, 0.8), (0.14, 0.6), (0.5, 0.46), (0.64, 0.26), (0.5, 0.05), (0.22, 0.02), (0.02, 0.16)])]),
    'H': (0.66, [('l', [(0, 0), (0, 1)]), ('l', [(0.66, 0), (0.66, 1)]), ('l', [(0, 0.52), (0.66, 0.52)])]),
    'O': (0.74, [('s', [(0.37, 1), (0.08, 0.86), (0, 0.5), (0.08, 0.14), (0.37, 0), (0.66, 0.14), (0.74, 0.5), (0.66, 0.86), (0.37, 1), (0.1, 0.88)])]),
    'P': (0.64, [('l', [(0, 0), (0, 1)]), ('s', [(0, 1), (0.42, 0.98), (0.62, 0.82), (0.56, 0.6), (0.36, 0.5), (0, 0.5)])]),
    '!': (0.2, [('l', [(0.1, 1), (0.09, 0.32)]), ('l', [(0.09, 0.05), (0.091, 0.04)])]),
    '1': (0.5, [('l', [(0.06, 0.78), (0.28, 1), (0.28, 0)]), ('l', [(0.06, 0), (0.5, 0)])]),
    '2': (0.66, [('s', [(0.04, 0.74), (0.16, 0.93), (0.38, 1), (0.58, 0.9), (0.62, 0.7), (0.44, 0.45), (0.02, 0.0)]), ('l', [(0.02, 0.0), (0.66, 0.0)])]),
    '3': (0.62, [('s', [(0.04, 0.86), (0.3, 1), (0.56, 0.9), (0.56, 0.68), (0.3, 0.54)]), ('s', [(0.3, 0.54), (0.6, 0.4), (0.6, 0.14), (0.34, 0.0), (0.04, 0.1)])]),
}


def letter_strokes(text, origin, height, spacing=0.16, bounce=0.0, tilt=0.0, seed=1):
    """Lay text out on the plane y=origin.y, left to right from origin (x, z = baseline).
    bounce/tilt add a hand-lettered wobble per letter."""
    rnd = random.Random(seed)
    x = origin[0]
    out = []
    for ch in text:
        if ch == ' ':
            x += height * 0.45
            continue
        w, strokes = GLYPHS[ch]
        dz = rnd.uniform(-bounce, bounce) * height
        ang = math.radians(rnd.uniform(-tilt, tilt))
        cx, cz = w / 2, 0.5
        for kind, pts in strokes:
            pts = catmull(pts) if kind == 's' else pts
            p3 = []
            for px, pz in pts:
                rx = (px - cx) * math.cos(ang) - (pz - cz) * math.sin(ang) + cx
                rz = (px - cx) * math.sin(ang) + (pz - cz) * math.cos(ang) + cz
                p3.append((x + rx * height, origin[1], origin[2] + dz + rz * height))
            out.append(p3)
        x += (w + spacing) * height
    return out


def text_width(text, height, spacing=0.16):
    return sum(height * 0.45 if ch == ' ' else (GLYPHS[ch][0] + spacing) * height for ch in text) - spacing * height


def lettering(text, origin, height, fill, ink, fill_r, **kw):
    """Cartoon lettering: a fat ink stroke under a coloured one."""
    strokes = letter_strokes(text, origin, height, **kw)
    for s in strokes:
        stroke([(p[0], p[1] + 0.004, p[2]) for p in s], fill_r + ink)
    for s in strokes:
        stroke([(p[0], p[1] - 0.004, p[2]) for p in s], fill_r, fill)


def grain(x0, x1, z0, z1, y, n=2, hex_='#7a4320', seed=None):
    """A few short wavy grain strokes on a plank face (x0..x1, z0..z1 at depth y)."""
    rnd = random.Random(seed)
    for _ in range(n):
        L = (x1 - x0) * rnd.uniform(0.2, 0.45)
        sx = rnd.uniform(x0 + 0.04, x1 - L - 0.04)
        zc = rnd.uniform(z0 + (z1 - z0) * 0.3, z1 - (z1 - z0) * 0.3)
        amp = (z1 - z0) * rnd.uniform(0.04, 0.1)
        ph = rnd.uniform(0, 6)
        stroke([(sx + L * t, y, zc + amp * math.sin(ph + t * 7)) for t in [i / 10 for i in range(11)]], 0.0045, hex_)


def grain_v(x0, x1, z0, z1, y, n=2, hex_='#5c321a', seed=None, m=None, spread=0.3):
    """Vertical grain for upright planks and posts. With m, the face is in that matrix's
    local space (a tilted box's backboard). spread keeps strokes that far in from the sides."""
    rnd = random.Random(seed)
    for _ in range(n):
        L = (z1 - z0) * rnd.uniform(0.15, 0.35)
        sz = rnd.uniform(z0 + 0.05, z1 - L - 0.05)
        xc = rnd.uniform(x0 + (x1 - x0) * spread, x1 - (x1 - x0) * spread)
        amp = (x1 - x0) * rnd.uniform(0.05, 0.1)
        ph = rnd.uniform(0, 6)
        pts = [Vector((xc + amp * math.sin(ph + t * 6), y, sz + L * t)) for t in [i / 10 for i in range(11)]]
        stroke([m @ p for p in pts] if m else pts, 0.0045, hex_)


def nail(x, z, y):
    stroke([(x, y, z), (x + 0.001, y, z)], 0.014, '#2e2624')


# ---------------------------------------------------------------- the stall
# World: x right, z up, the camera looks along +y. 1 m is about 324 px on screen.
# Ink details (grain, nails, letters) sit a couple of cm in front of the faces they're on,
# so the wobble in those faces can't swallow them.

STALL_L, STALL_R = -2.98, 1.58      # counter extent
COUNTER_TOP = 1.02
TOP_C = (STALL_L + STALL_R) / 2
TOP_TILT = 0.6                      # degrees; the counter top runs downhill to the right
POST_X = (-2.58, 1.2)


def counter_surface(x):
    """Height of the counter top's upper face at x."""
    return COUNTER_TOP + 0.085 - math.sin(math.radians(TOP_TILT)) * (x - TOP_C)


# counter front: five planks, each a bit off true
plank_h = COUNTER_TOP / 5
for i in range(5):
    z0 = i * plank_h
    zc = z0 + plank_h / 2
    block(f'CounterPlank{i}', (STALL_R - STALL_L, 0.08, plank_h - 0.012), M['plank'],
          loc=(TOP_C + random.uniform(-0.03, 0.03), 0.0, zc), rot=(0, random.uniform(-0.5, 0.5), 0), amp=0.01)
    grain(STALL_L + 0.1, STALL_R - 0.1, z0, z0 + plank_h, -0.07, n=3, seed=10 + i)
    for nx in (STALL_L + 0.12, TOP_C + 0.6, STALL_R - 0.1):
        nail(nx + random.uniform(-0.03, 0.03), zc, -0.07)

# counter top board, overhanging the front
block('CounterTop', (STALL_R - STALL_L + 0.16, 0.9, 0.09), M['light'],
      loc=(TOP_C, 0.36, COUNTER_TOP + 0.04), rot=(0, TOP_TILT, 0), amp=0.008)

# a patch nailed over a broken bit of the counter, crooked
PATCH_C, PATCH_W, PATCH_ROT = Vector((0.95, -0.065, 0.33)), 0.55, -11
patch = block('Patch', (PATCH_W, 0.03, 0.15), M['light'], loc=PATCH_C, rot=(0, PATCH_ROT, 0), amp=0.005, cuts=4)
for sx in (-1, 1):
    for sz in (-1, 1):
        p = patch.matrix_basis @ Vector((sx * (PATCH_W / 2 - 0.05), -0.035, sz * 0.035))
        nail(p.x, p.z, p.y)

# posts: standing behind the counter front, leaning a touch
for i, px in enumerate(POST_X):
    lean = (-1.6, 2.0)[i]
    block(f'Post{i}', (0.2, 0.2, 2.75), M['post'], loc=(px, 0.15, 1.36), rot=(0, lean, 0), cuts=8, amp=0.018, taper=0.06)
    grain_v(px - 0.08, px + 0.08, COUNTER_TOP + 0.15, 2.4, 0.015, n=2, seed=20 + i)

# back wall inside the stall: upright planks, darker, with a dark board behind so the gaps
# between them don't show the sky
block('BackFill', (POST_X[1] - POST_X[0], 0.04, 1.8), toon('BackFill', '#3d2212', shade=1.0), loc=((POST_X[0] + POST_X[1]) / 2, 1.02, COUNTER_TOP + 0.8), cuts=1, amp=0)
x = POST_X[0] + 0.05
i = 0
while x < POST_X[1] - 0.05:
    w = min(random.uniform(0.2, 0.27), POST_X[1] - 0.05 - x)
    block(f'Back{i}', (w - 0.012, 0.06, 1.75), M['back'], loc=(x + w / 2, 0.95, COUNTER_TOP + 0.8), rot=(0, random.uniform(-0.8, 0.8), 0), amp=0.008)
    grain_v(x + 0.02, x + w - 0.02, COUNTER_TOP + 0.1, COUNTER_TOP + 1.6, 0.9, n=1, hex_='#5a3119', seed=40 + i)
    x += w
    i += 1

# roof: a wide wooden trapezoid on the front, with a dark underside strip below it
ROOF_Y = -0.32
ROOF_BOT, ROOF_TOP = 2.55, 3.2
roof_outline = [(-3.1, ROOF_BOT + 0.01), (-1.6, ROOF_BOT - 0.015), (-0.2, ROOF_BOT - 0.025), (0.9, ROOF_BOT - 0.01), (1.75, ROOF_BOT + 0.015),
                (1.25, ROOF_TOP - 0.02), (-0.67, ROOF_TOP + 0.05), (-2.55, ROOF_TOP - 0.03)]   # the bottom edge sags a bit
prism('RoofFront', roof_outline, 0.14, M['roof'], ROOF_Y, amp=0.012)
for k, zz in enumerate((ROOF_BOT + 0.2, ROOF_BOT + 0.42)):
    span = 0.25 + k * 0.25
    stroke([(-3.0 + span, ROOF_Y - 0.03, zz), (1.65 - span, ROOF_Y - 0.03, zz + 0.01)], 0.006, '#5e2f15')
block('RoofUnder', (4.6, 1.3, 0.06), M['soffit'], loc=(-0.75, 0.38, ROOF_BOT + 0.03), amp=0.006)

# scalloped striped valance hanging under the roof edge, one stripe per scallop
VAL_TOP, VAL_DROP, SCALLOP = ROOF_BOT + 0.01, 0.2, 0.25
vx0, vx1 = -3.08, 1.73
n_sc = round((vx1 - vx0) / SCALLOP)
SCALLOP = (vx1 - vx0) / n_sc
outline = [(vx0, VAL_TOP), (vx0, VAL_TOP - VAL_DROP + 0.075)]
for k in range(n_sc):
    a = vx0 + k * SCALLOP
    for t in range(1, 13):
        u = t / 12
        outline.append((a + u * SCALLOP, VAL_TOP - VAL_DROP + 0.075 - 0.075 * math.sin(math.pi * u)))
outline.append((vx1, VAL_TOP))
prism('Valance', outline, 0.03, toon('Valance', '#fff3df', stripes=('#f59ac0', SCALLOP, vx0)), ROOF_Y - 0.035, amp=0.006)

# the sign: a light board nailed to the roof, a little crooked, hand-lettered
SIGN_C = Vector((-0.67, ROOF_Y - 0.09, (ROOF_BOT + ROOF_TOP) / 2 + 0.02))
SIGN_W, SIGN_H = 3.0, 0.5
block('Sign', (SIGN_W, 0.06, SIGN_H), M['light'], loc=SIGN_C, rot=(0, -1.8, 0), cuts=8, amp=0.01)
FACE_Y = SIGN_C.y - 0.06
grain(SIGN_C.x - SIGN_W / 2 + 0.1, SIGN_C.x + SIGN_W / 2 - 0.1, SIGN_C.z - SIGN_H / 2, SIGN_C.z + SIGN_H / 2, FACE_Y, n=3, hex_='#b07440', seed=5)
for sx in (-1, 1):
    nail(SIGN_C.x + sx * (SIGN_W / 2 - 0.09), SIGN_C.z + 0.012 * sx, FACE_Y)
TEXT, TH = "DABI SHOP!", 0.32
tw = text_width(TEXT, TH)
lettering(TEXT, (SIGN_C.x - tw / 2, FACE_Y - 0.01, SIGN_C.z - TH / 2), TH, '#e8338e', 0.016, 0.03, bounce=0.07, tilt=7, seed=11)

# display boxes on the counter: a frame round a wooden backboard, and a number tag hanging above.
# Tall, because /shop shows a Dabling wearing each item in them.
BOX_W, BOX_H, BOX_D, FR = 0.6, 0.82, 0.22, 0.05
BOX_XS = (-1.82, -0.67, 0.48)
BOX_TILTS = (-1.2, 0.9, -0.7)
BOX_Y = 0.3
boxes = []
for i, (bx, tilt) in enumerate(zip(BOX_XS, BOX_TILTS)):
    lift = math.sin(math.radians(abs(tilt - TOP_TILT))) * BOX_W / 2   # resting on its low corner
    cz = counter_surface(bx) + BOX_H / 2 + lift
    e = Matrix.Translation((bx, BOX_Y, cz)) @ Matrix.Rotation(math.radians(tilt), 4, 'Y')
    parts = [block(f'Box{i}Back', (BOX_W - 0.02, 0.03, BOX_H - 0.02), M['backboard'], loc=(0, BOX_D / 2 - 0.02, 0), amp=0.004, cuts=4)]
    grain_v(-(BOX_W / 2 - FR), BOX_W / 2 - FR, -(BOX_H / 2 - FR), BOX_H / 2 - FR, BOX_D / 2 - 0.045,
            n=5, hex_=BACKBOARD[1], seed=50 + i, m=e, spread=0.1)
    for nm, sz, lc in (('L', (FR, BOX_D, BOX_H), (-BOX_W / 2 + FR / 2, 0, 0)), ('R', (FR, BOX_D, BOX_H), (BOX_W / 2 - FR / 2, 0, 0)),
                       ('T', (BOX_W, BOX_D, FR), (0, 0, BOX_H / 2 - FR / 2)), ('B', (BOX_W, BOX_D, FR), (0, 0, -BOX_H / 2 + FR / 2))):
        rail = block(f'Box{i}{nm}', sz, M['light'], loc=lc, amp=0.004, cuts=4)
        rail.visible_shadow = False   # keep the backing evenly lit for the item
        parts.append(rail)
    for p in parts:
        p.matrix_basis = e @ p.matrix_basis
    # number tag: a round wooden tag on a string from a nail in the back wall
    tag_r = 0.1
    tag_c = Vector((bx, BOX_Y - 0.02, cz + BOX_H / 2 + 0.14))
    nail_p = Vector((bx + 0.02, 0.88, tag_c.z + 0.2))
    cylinder(f'NumTag{i}', tag_r, 0.03, M['light'], tag_c, rot=(90, random.uniform(-6, 6), 0), amp=0.004)
    for sx in (-1, 1):
        stroke([nail_p, (tag_c.x + sx * 0.045, tag_c.y, tag_c.z + tag_r * 0.88)], 0.004, '#3b2a20')
    nail(nail_p.x, nail_p.z, nail_p.y)
    digit = str(i + 1)
    dh = 0.13
    dw = GLYPHS[digit][0] * dh
    for s in letter_strokes(digit, (tag_c.x - dw / 2, tag_c.y - 0.04, tag_c.z - dh / 2), dh, seed=30 + i):
        stroke(s, 0.015)
    # price tag: a paper tag tacked to the counter's front edge under the box (/shop writes the price)
    PT_W, PT_H = 0.38, 0.2
    pt_tilt = (5, -4, 3)[i]
    pt_c = Vector((bx + 0.04, -0.15, COUNTER_TOP - 0.17))
    pt = block(f'PriceTag{i}', (PT_W, 0.012, PT_H), M['paper'], loc=pt_c, rot=(0, pt_tilt, 0), amp=0.003, cuts=4)
    pt.visible_shadow = False
    for sx in (-1, 1):   # a tack in each top corner, leaving the middle for the price
        tack = pt.matrix_basis @ Vector((sx * (PT_W / 2 - 0.03), -0.03, PT_H / 2 - 0.03))
        stroke([tack, (tack.x + 0.001, tack.y, tack.z)], 0.014, '#c8323c')
    boxes.append(dict(box=e, tag=pt))

# Dabi's stool: round seat, three splayed legs, a rung
STOOL_C = Vector((2.27, 0.25, 0.0))
SEAT_TOP = 0.66
cylinder('StoolSeat', 0.46, 0.09, M['seat'], (STOOL_C.x, STOOL_C.y, SEAT_TOP - 0.045), rot=(0, -1.5, 0), amp=0.008)
for k, ang in enumerate((210, 330, 90)):
    a = math.radians(ang)
    top = Vector((STOOL_C.x + 0.27 * math.cos(a), STOOL_C.y + 0.27 * math.sin(a), SEAT_TOP - 0.09))
    foot = Vector((STOOL_C.x + 0.42 * math.cos(a), STOOL_C.y + 0.42 * math.sin(a), -0.02))
    d = top - foot
    leg = cylinder(f'StoolLeg{k}', 0.045, d.length, M['post'], (top + foot) / 2, r_top=0.055, segs=20, amp=0.006)
    leg.rotation_mode = 'QUATERNION'
    leg.rotation_quaternion = Vector((0, 0, 1)).rotation_difference(d.normalized())
cylinder('StoolRung', 0.36, 0.035, M['post'], (STOOL_C.x, STOOL_C.y, 0.26), segs=40, amp=0.004)

# ---------------------------------------------------------------- the backdrop
# Sky, clouds, hills and grass behind the stall, in their own collection so --transparent
# can render the stall on its own. Only the right-hand side (round the stool) and a sliver
# above the roof show, so that's where the detail goes.
CURRENT[0] = 'Backdrop'


def sky_material(horizon, top, z0, z1):
    m = bpy.data.materials.new('Sky')
    if m.node_tree is None:
        m.use_nodes = True
    nt = m.node_tree
    nt.nodes.clear()
    out = nt.nodes.new('ShaderNodeOutputMaterial')
    tc = nt.nodes.new('ShaderNodeTexCoord')
    sep = nt.nodes.new('ShaderNodeSeparateXYZ')
    rng = nt.nodes.new('ShaderNodeMapRange')
    rng.inputs['From Min'].default_value, rng.inputs['From Max'].default_value = z0, z1
    ramp = nt.nodes.new('ShaderNodeValToRGB')
    ramp.color_ramp.elements[0].color = (*lin(horizon), 1)
    ramp.color_ramp.elements[1].color = (*lin(top), 1)
    em = nt.nodes.new('ShaderNodeEmission')
    nt.links.new(tc.outputs['Object'], sep.inputs[0])
    nt.links.new(sep.outputs['Z'], rng.inputs['Value'])
    nt.links.new(rng.outputs['Result'], ramp.inputs['Fac'])
    nt.links.new(ramp.outputs['Color'], em.inputs['Color'])
    nt.links.new(em.outputs[0], out.inputs['Surface'])
    return m


SKY_Y = 40
sky = block('Sky', (30, 0.1, 16), sky_material('#e4f6ff', '#8fd0f2', EYE_Z, 7.5), loc=(0, SKY_Y, 1.5), cuts=1, amp=0)
sky.visible_shadow = False

ground = block('Ground', (30, 42, 0.1), toon('Grass', '#8ccc62', shade=0.8), loc=(0, 19, -0.05), cuts=1, amp=0)


def hill_outline(x0, x1, base, tops, seed):
    rnd = random.Random(seed)
    pts = [(x0, base)]
    n = 40
    for k in range(n + 1):
        t = k / n
        x = x0 + (x1 - x0) * t
        z = sum(h * math.exp(-((x - c) / w) ** 2) for c, h, w in tops) + base
        pts.append((x, z + rnd.uniform(-0.01, 0.01)))
    pts.append((x1, base))
    return pts


# no wobble on these: bending a big flat outline folds its long thin triangles into creases the ink picks up
prism('HillFar', hill_outline(-6, 14, -0.1, [(5.2, 1.55, 2.2), (9.5, 1.85, 2.6), (13, 1.3, 2)], 3), 0.5, toon('HillFar', '#a8d98a', shade=0.85), 34, amp=0)
prism('HillNear', hill_outline(-6, 14, -0.1, [(3.6, 0.9, 1.6), (7.4, 1.15, 2.0), (11, 0.8, 1.7)], 4), 0.5, toon('HillNear', '#7fbf57', shade=0.82), 22, amp=0)


def cloud_outline(a, b, bumps, phase):
    pts = []
    for k in range(96):
        th = 2 * math.pi * k / 96
        r = 1 + 0.16 * abs(math.sin(bumps * th / 2 + phase))
        x, z = a * r * math.cos(th), b * r * math.sin(th)
        if z < 0:
            z *= 0.45   # flat-ish bottom
        pts.append((x, z))
    return pts


for name, (cx, cz, cy, a, b, bumps, ph) in {'Cloud0': (5.6, 4.3, 20, 1.05, 0.42, 9, 0.3), 'Cloud1': (7.4, 3.3, 24, 0.7, 0.28, 7, 1.1)}.items():
    c = prism(name, [(cx + x, cz + z) for x, z in cloud_outline(a, b, bumps, ph)], 0.3, toon(name, '#ffffff', shade=0.9), cy, amp=0.01)
    c.visible_shadow = False

# grass tufts and a few flowers round the stool
rnd = random.Random(21)
for _ in range(26):
    gx, gy = rnd.uniform(1.62, 3.4), rnd.uniform(-0.5, 1.4)
    hgt = rnd.uniform(0.06, 0.11)
    stroke([(gx - 0.04, gy, 0.0), (gx - 0.025, gy, hgt * 0.9), (gx - 0.008, gy, 0.01), (gx + 0.006, gy, hgt), (gx + 0.02, gy, 0.01), (gx + 0.04, gy, hgt * 0.7)], 0.006, '#4f8a35')
    if rnd.random() < 0.3:
        stroke([(gx + 0.006, gy - 0.01, hgt + 0.012), (gx + 0.007, gy - 0.01, hgt + 0.012)], 0.016, rnd.choice(('#f57fb4', '#ffd23f', '#ffffff')))

CURRENT[0] = 'Stall'
flush_strokes()

# ---------------------------------------------------------------- camera, light, ink

cam = bpy.data.objects.new("Camera", bpy.data.cameras.new("Camera"))
sc.collection.objects.link(cam)
cam.location = (0, -14, EYE_Z)   # eye height: the horizon sits here
cam.rotation_euler = (math.radians(90), 0, 0)
cam.data.lens = 85
cam.data.sensor_fit = 'HORIZONTAL'
frame_w = 14 * cam.data.sensor_width / cam.data.lens
frame_h = frame_w * H / W
cam.data.shift_y = -(EYE_Z - frame_h / 2) / frame_w   # frame bottom at z=0 on the y=0 plane
sc.camera = cam

sun = bpy.data.objects.new("Sun", bpy.data.lights.new("Sun", 'SUN'))
sun.data.energy = 3.0
sun.data.angle = math.radians(1.5)
sun.rotation_euler = (math.radians(38), math.radians(-24), math.radians(-28))
sc.collection.objects.link(sun)

sc.render.use_freestyle = True
sc.render.line_thickness_mode = 'ABSOLUTE'
vl = bpy.context.view_layer
vl.use_freestyle = True
fs = vl.freestyle_settings
fs.crease_angle = math.radians(140)


def ink_lines(ls, coll_name, thickness):
    """Wobbly hand-inked outlines, fatter in the middle of a stroke than at its ends."""
    ls.select_by_visibility = True
    ls.visibility = 'VISIBLE'
    ls.select_silhouette = ls.select_border = ls.select_crease = ls.select_external_contour = True
    ls.select_by_collection = True
    ls.collection = bpy.data.collections[coll_name]
    st = ls.linestyle
    st.color = lin(INK)
    st.thickness = thickness
    st.caps = 'ROUND'
    st.chaining = 'PLAIN'
    along = st.thickness_modifiers.new("taper", 'ALONG_STROKE')
    along.mapping = 'CURVE'
    along.blend = 'MULTIPLY'
    along.value_min, along.value_max = 0.55, 1.2
    pts = along.curve.curves[0].points
    pts[0].location = (0.0, 0.0)
    pts[1].location = (1.0, 0.0)
    pts.new(0.5, 1.0)
    along.curve.update()
    wob = st.geometry_modifiers.new("wobble", 'PERLIN_NOISE_1D')
    wob.frequency = 9
    wob.amplitude = 1.3
    wob.octaves = 2


ink_lines(fs.linesets[0], 'Stall', 4.6)
if TRANSPARENT:
    for name in ('Backdrop', 'BackdropInk'):
        bpy.data.collections[name].hide_render = True
else:
    sc.render.film_transparent = False
    ink_lines(fs.linesets.new("Backdrop"), 'Backdrop', 3.0)   # thinner lines further back

# ---------------------------------------------------------------- layout for /shop

bpy.context.view_layer.update()


def to_px(p):
    v = world_to_camera_view(sc, cam, Vector(p))
    return (v.x * W, (1 - v.y) * H)


def screen_rect(m, half_w, half_h, depth):
    """Where a camera-facing w x h rectangle (local x/z, at local y=depth) lands on screen:
    its centre and size in pixels, and its tilt in degrees clockwise, the way CSS rotate()
    takes it."""
    c = to_px(m @ Vector((0, depth, 0)))
    left, right = to_px(m @ Vector((-half_w, depth, 0))), to_px(m @ Vector((half_w, depth, 0)))
    top, bottom = to_px(m @ Vector((0, depth, half_h))), to_px(m @ Vector((0, depth, -half_h)))
    return {"cx": round(c[0], 1), "cy": round(c[1], 1), "w": round(math.dist(left, right)), "h": round(math.dist(top, bottom)),
            "angle": round(math.degrees(math.atan2(right[1] - left[1], right[0] - left[0])), 2)}


layout = {"size": [W, H], "boxes": []}
for b in boxes:
    layout["boxes"].append({
        "item": screen_rect(b['box'], BOX_W / 2 - FR, BOX_H / 2 - FR, BOX_D / 2 - 0.036),   # the backboard
        "price": screen_rect(b['tag'].matrix_world, PT_W / 2, PT_H / 2, -0.006),           # the paper tag's face
    })
seat = to_px((STOOL_C.x, STOOL_C.y, SEAT_TOP))
layout["stool_seat"] = {"cx": round(seat[0]), "y": round(seat[1])}
with open(os.path.join(OUT_DIR, "layout.json"), "w") as f:
    json.dump(layout, f, indent=2)
print("LAYOUT", json.dumps(layout))

img = sc.render.image_settings
img.file_format = 'PNG'
img.color_mode = 'RGBA' if TRANSPARENT else 'RGB'   # the sky makes it opaque, so no alpha to store
img.compression = 100                              # smallest file; it's in git
sc.render.filepath = os.path.join(OUT_DIR, "stall.png")
bpy.ops.render.render(write_still=True)
print("WROTE", sc.render.filepath)
