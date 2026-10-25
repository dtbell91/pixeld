"""Regenerates the APNG fixtures for apng_test.go with Pillow (what Home Assistant encodes with)."""
from PIL import Image


def rgb_frame(k):
    im = Image.new("RGB", (32, 32))
    im.putdata([(x * 8, y * 8, 40) for y in range(32) for x in range(32)])
    for y in range(10, 14):
        for x in range(4 * k, 4 * k + 4):
            im.putpixel((x, y), (255, 255, 255))
    return im


def rgba_frame(k):
    im = Image.new("RGBA", (32, 32), (0, 0, 0, 0))
    for y in range(10, 14):
        for x in range(4 * k, 4 * k + 4):
            im.putpixel((x, y), (255, 0, 0, 255))
    return im


rgb = [rgb_frame(k) for k in range(3)]
rgb[0].save("rgb.png", save_all=True, append_images=rgb[1:], duration=[50, 75, 120], loop=0)
rgba = [rgba_frame(k) for k in range(3)]
rgba[0].save("rgba.png", save_all=True, append_images=rgba[1:], duration=80, loop=0, disposal=1, blend=0)
prev = [rgb_frame(0).convert("RGBA")] + [rgba_frame(k) for k in (1, 2)]
prev[0].save("previous.png", save_all=True, append_images=prev[1:], duration=90, loop=0, disposal=2, blend=1)
pal = [f.convert("P", palette=Image.Palette.ADAPTIVE, colors=16) for f in rgb]
pal[0].save("palette.png", save_all=True, append_images=pal[1:], duration=60, loop=0)

# What Pillow reads back, composited onto black: the frames pixeld must produce.
for name in ("rgb", "rgba", "previous", "palette"):
    im = Image.open(f"{name}.png")
    raw = b""
    for i in range(im.n_frames):
        im.seek(i)
        black = Image.new("RGBA", im.size, (0, 0, 0, 255))
        raw += Image.alpha_composite(black, im.convert("RGBA")).convert("RGB").tobytes()
    with open(f"{name}.rgb", "wb") as f:
        f.write(raw)
