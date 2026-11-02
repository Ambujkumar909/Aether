"""Cut the brand artwork into the assets the product needs, on both grounds.

The source is one 1254px square on white: a glass mark, the wordmark, a
tagline. Two obvious cut-out methods both fail on this artwork:

  * Key out white, or unmultiply against it. Glows and soft edges survive
    perfectly -- but the artwork uses white as its LIGHT SOURCE, and the
    specular highlights along the glass are near-white, so they turn
    transparent and the mark goes dull and lifeless on a dark ground.

  * Cut at the saturated silhouette. The highlights stay bright, but the pale
    outer glow is faintly tinted: a fixed chroma threshold keeps it and leaves
    a white fringe, and a relative threshold drops it along with the
    highlights, so the mark comes apart.

Both at once is what works. The artwork BODY is found by chroma relative to
lightness, then morphologically closed so the highlights -- which run out to
the edge of the glass in places -- end up enclosed and get filled in. Inside
that silhouette the original pixels are kept exactly. Outside it, alpha comes
from distance to white, so the glow fades out instead of ending in a hard pale
edge.
"""

from PIL import Image, ImageFilter, ImageOps
from collections import deque
import os

SRC = r'C:\Users\ambuj\Downloads\zoom_arch\logo.png'
OUT = r'C:\Users\ambuj\Downloads\zoom_arch\web\brand'
PROOF = r'C:\Users\ambuj\AppData\Local\Temp\claude\C--Users-ambuj-Downloads-zoom-arch\95c8f209-0df3-4aac-8874-e9c8eab02635\scratchpad'
os.makedirs(OUT, exist_ok=True)

im = Image.open(SRC).convert('RGB')
W, H = im.size
MARK_BOX = (0, 0, W, 908)
WORD_BOX = (0, 915, W, 1014)
TAG_BOX = (0, 1060, W, 1094)


def flood_border(mask):
    """Zero the border-connected white of a binary mask, in place."""
    w, h = mask.size
    mp = mask.load()
    seen = bytearray(w * h)
    q = deque()
    for x in range(w):
        for y in (0, h - 1):
            if mp[x, y] > 127 and not seen[y * w + x]:
                seen[y * w + x] = 1
                q.append((x, y))
    for y in range(h):
        for x in (0, w - 1):
            if mp[x, y] > 127 and not seen[y * w + x]:
                seen[y * w + x] = 1
                q.append((x, y))
    while q:
        x, y = q.popleft()
        mp[x, y] = 0
        for dx, dy in ((1, 0), (-1, 0), (0, 1), (0, -1)):
            nx, ny = x + dx, y + dy
            if 0 <= nx < w and 0 <= ny < h and not seen[ny * w + nx] and mp[nx, ny] > 127:
                seen[ny * w + nx] = 1
                q.append((nx, ny))


def fill_holes(m):
    holes = ImageOps.invert(m)
    flood_border(holes)
    m.paste(255, (0, 0), holes)
    return m


def body_mask(rgb, rel_sat, dark_floor, close_px):
    """The artwork's silhouette, closed so highlights fall inside it."""
    w, h = rgb.size
    px = rgb.load()
    m = Image.new('L', (w, h), 0)
    mp = m.load()
    for y in range(h):
        for x in range(w):
            r, g, b = px[x, y]
            mx, mn = max(r, g, b), min(r, g, b)
            if (mx and (mx - mn) / mx >= rel_sat) or mx <= dark_floor:
                mp[x, y] = 255

    # Close: dilate, fill what is now enclosed, erode back. The dilation
    # bridges the gaps where a highlight runs out to the edge of the glass, so
    # the fill can reach around it.
    if close_px:
        k = close_px * 2 + 1
        m = m.filter(ImageFilter.MaxFilter(k))
        m = fill_holes(m)
        m = m.filter(ImageFilter.MinFilter(k))
    return fill_holes(m)


def cut(src, rel_sat=0.30, dark_floor=200, close_px=7, glow=True):
    rgb = src.convert('RGB')
    w, h = rgb.size
    px = rgb.load()
    body = body_mask(rgb, rel_sat, dark_floor, close_px)
    body = body.filter(ImageFilter.GaussianBlur(0.8))
    bp = body.load()

    out = Image.new('RGBA', (w, h))
    op = out.load()
    for y in range(h):
        for x in range(w):
            r, g, b = px[x, y]
            inside = bp[x, y]
            if inside >= 250:
                op[x, y] = (r, g, b, 255)
                continue
            mn = min(r, g, b)
            a = (255 - mn) if glow else 0
            # On the feathered rim the two blend, so the silhouette does not
            # end in a step.
            a = a * inside // 255 if inside else a
            # Below this the glow is invisible and only serves to stop the
            # image being trimmed to its real bounds.
            if a <= 8 or mn >= 255:
                op[x, y] = (0, 0, 0, 0)
                continue
            f = 255.0 / (255 - mn)
            op[x, y] = (min(255, int((r - mn) * f)),
                        min(255, int((g - mn) * f)),
                        min(255, int((b - mn) * f)),
                        min(255, a))
    bb = out.getbbox()
    return out.crop(bb) if bb else out


def recolour(img, rgb):
    """Repaint flat artwork one colour, keeping its alpha. Wordmark only: the
    mark is glass with real shading, and repainting it would flatten it."""
    solid = Image.new('RGBA', img.size, rgb + (255,))
    solid.putalpha(img.getchannel('A'))
    return solid


def save(img, name, sizes=None):
    img.save(os.path.join(OUT, name + '.png'))
    print('  %-24s %s' % (name + '.png', img.size))
    for s in sizes or []:
        w, h = img.size
        img.resize((s, max(1, round(h * s / w))), Image.LANCZOS).save(
            os.path.join(OUT, '%s-%d.png' % (name, s)))


print('mark')
mark = cut(im.crop(MARK_BOX), rel_sat=0.30, dark_floor=200, close_px=7)
save(mark, 'mark', [512, 256, 128, 64, 48, 32])

print('wordmark')
word = cut(im.crop(WORD_BOX), rel_sat=0.10, dark_floor=215, close_px=2, glow=False)
save(recolour(word, (0x1E, 0x3A, 0x30)), 'wordmark-light', [512, 256])
save(recolour(word, (0xEC, 0xF1, 0xEE)), 'wordmark-dark', [512, 256])

print('tagline')
tag = cut(im.crop(TAG_BOX), rel_sat=0.08, dark_floor=228, close_px=1, glow=False)
save(recolour(tag, (0x59, 0x61, 0x6E)), 'tagline-light', [420])
save(recolour(tag, (0x9F, 0xB3, 0xAA)), 'tagline-dark', [420])

side = max(mark.size)
sq = Image.new('RGBA', (side, side), (0, 0, 0, 0))
sq.paste(mark, ((side - mark.width) // 2, (side - mark.height) // 2), mark)
sq.save(os.path.join(OUT, 'aether.ico'),
        sizes=[(256, 256), (128, 128), (64, 64), (48, 48), (32, 32), (16, 16)])
print('  aether.ico               multi-resolution')

m = Image.open(os.path.join(OUT, 'mark-256.png'))
for name, bg in (('dark', (14, 26, 32)), ('light', (246, 247, 249))):
    c = Image.new('RGB', m.size, bg)
    c.paste(m, (0, 0), m)
    c.save(os.path.join(PROOF, 'proof-%s.png' % name))
print('  proofs written')
