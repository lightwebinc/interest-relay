#!/usr/bin/env python3
"""The figures in this directory.

Run:  python3 docs/figures/figs.py      (writes the .svg files next to it)

One grammar across the set:
  dotted orange line  interest: UDP datagrams, one hop at a time
  solid blue line     objects: delivery records on a lane (TCP)
  solid teal line     publications: submission records (TCP)
  dashed gray line    what does not happen
Every figure is drawn on black.
Line style carries the meaning as well as color.

Needs Pillow and Liberation Sans, to measure text: a label that would leave
its box or the canvas fails the run instead of shipping.
"""
import math
import os
import sys

from PIL import ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
FONT = "Arial, Helvetica, sans-serif"
_DIR = "/usr/share/fonts/truetype/liberation/"
_REG, _BLD = _DIR + "LiberationSans-Regular.ttf", _DIR + "LiberationSans-Bold.ttf"
_cache = {}

# Black ground, light type, bright lines.
BG, EDGE = "#000000", "#2a3038"
INK, MUTED, HAIR, PANEL, WHITE = "#f2f5f8", "#aab4c0", "#3a424d", "#12161b", "#000000"
INT = "#ff9a45"   # interest
OBJ = "#5cabff"   # objects
PUB = "#3dd6a6"   # publications
NOT = "#808a97"   # what does not happen
ANS = "#c3ccd6"   # an answer, in a sequence
DOTS, DASH = "1.5,3.5", "5,4"

W = 800
WARN = []


def tw(s, size, bold=False):
    """Rendered width of a string (Liberation Sans has Arial's metrics)."""
    if bold not in _cache:
        _cache[bold] = ImageFont.truetype(_BLD if bold else _REG, 100)
    return _cache[bold].getlength(s) * size / 100.0


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


class Fig:
    def __init__(self, name, h, title):
        self.name, self.h, self.title, self.o = name, h, title, []

    def rect(self, x, y, w, h, fill=WHITE, stroke=HAIR, sw=1.0, rx=5, dash=None):
        d = ' stroke-dasharray="%s"' % dash if dash else ""
        s = 'stroke="%s" stroke-width="%.2f"' % (stroke, sw) if stroke else 'stroke="none"'
        self.o.append('<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="%.1f" fill="%s" %s%s/>'
                      % (x, y, w, h, rx, fill, s, d))

    def text(self, x, y, s, size=11, bold=False, fill=INK, anchor="start", ls=0):
        width = tw(s, size, bold) + ls * len(s)
        left = x if anchor == "start" else x - width / (2 if anchor == "middle" else 1)
        if left < 2 or left + width > W - 2:
            WARN.append("%s: text off canvas: %r" % (self.name, s))
        extra = ' font-weight="bold"' if bold else ""
        if ls:
            extra += ' letter-spacing="%.2f"' % ls
        self.o.append('<text x="%.1f" y="%.1f" font-size="%.1f" fill="%s" text-anchor="%s"%s>%s</text>'
                      % (x, y, size, fill, anchor, extra, esc(s)))

    def label(self, x, y, s, room, fill=MUTED, size=10.5, bold=False, anchor="middle"):
        """Text that must fit a span of the given width."""
        if tw(s, size, bold) > room:
            WARN.append("%s: label too wide: %r (%.0f > %.0f)" % (self.name, s, tw(s, size, bold), room))
        self.text(x, y, s, size, bold, fill, anchor)

    def cap(self, x, y, s, fill=MUTED, anchor="middle"):
        self.text(x, y, s.upper(), 9.5, True, fill, anchor, ls=0.6)

    def head(self, x, y, ux, uy, color, size=8.0):
        hw = size * 0.42
        bx, by = x - ux * size, y - uy * size
        self.o.append('<polygon points="%.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="%s"/>'
                      % (x, y, bx - uy * hw, by + ux * hw, bx + uy * hw, by - ux * hw, color))

    def flow(self, pts, color, dash=None, sw=1.8, arrow=True):
        """A polyline through pts, with an arrowhead at the last point."""
        pts = [(float(x), float(y)) for x, y in pts]
        (x1, y1), (x2, y2) = pts[-2], pts[-1]
        n = math.hypot(x2 - x1, y2 - y1)
        ux, uy = (x2 - x1) / n, (y2 - y1) / n
        end = (x2 - ux * 7, y2 - uy * 7) if arrow else (x2, y2)
        d = "M" + " L".join("%.1f,%.1f" % p for p in pts[:-1] + [end])
        ds = ' stroke-dasharray="%s"' % dash if dash else ""
        cap = "round" if dash == DOTS else "butt"
        self.o.append('<path d="%s" fill="none" stroke="%s" stroke-width="%.2f" stroke-linejoin="round" '
                      'stroke-linecap="%s"%s/>' % (d, color, sw, cap, ds))
        if arrow:
            self.head(x2, y2, ux, uy, color)

    def box(self, x, y, w, h, title, sub=None, stroke=INK, fill=WHITE):
        self.rect(x, y, w, h, fill, stroke, 1.2)
        cx = x + w / 2.0
        for s, size, bold in ((title, 12, True), (sub, 10.5, False)):
            if s and tw(s, size, bold) > w - 8:
                WARN.append("%s: label too wide for box: %r" % (self.name, s))
        if sub:
            self.text(cx, y + h / 2.0 - 2.5, title, 12, True, anchor="middle")
            self.text(cx, y + h / 2.0 + 11.5, sub, 10.5, False, MUTED, "middle")
        else:
            self.text(cx, y + h / 2.0 + 4.2, title, 12, True, anchor="middle")

    def sbox(self, x, y, w, h, title, sub=None):
        """A smaller box, for a figure of many parts."""
        self.rect(x, y, w, h, WHITE, INK, 1.1)
        cx = x + w / 2.0
        for t, size, bold in ((title, 11, True), (sub, 9.5, False)):
            if t and tw(t, size, bold) > w - 6:
                WARN.append("%s: label too wide for box: %r" % (self.name, t))
        if sub:
            self.text(cx, y + h / 2.0 - 2, title, 11, True, anchor="middle")
            self.text(cx, y + h / 2.0 + 10.5, sub, 9.5, False, MUTED, "middle")
        else:
            self.text(cx, y + h / 2.0 + 4, title, 11, True, anchor="middle")

    def cross(self, x, y, color=NOT, r=5.0):
        self.o.append('<path d="M%.1f,%.1f L%.1f,%.1f M%.1f,%.1f L%.1f,%.1f" stroke="%s" stroke-width="2.2" '
                      'stroke-linecap="round"/>' % (x - r, y - r, x + r, y + r, x - r, y + r, x + r, y - r, color))

    def legend(self, y, items):
        x = 14.0
        for color, dash, s in items:
            self.flow([(x, y), (x + 34, y)], color, dash)
            self.text(x + 42, y + 3.8, s, 10.5, False, MUTED)
            x += 42 + tw(s, 10.5) + 26

    def save(self):
        p = os.path.join(HERE, self.name + ".svg")
        with open(p, "w") as f:
            f.write('<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" '
                    'font-family="%s" role="img" aria-label="%s">\n' % (W, self.h, W, self.h, FONT, esc(self.title)))
            f.write("<title>%s</title>\n" % esc(self.title))
            f.write('<rect x="0.5" y="0.5" width="%d" height="%d" rx="8" fill="%s" stroke="%s"/>\n'
                    % (W - 1, self.h - 1, BG, EDGE))
            f.write("\n".join(self.o) + "\n</svg>\n")


LEG_INT = (INT, DOTS, "interest: UDP 8729, one hop at a time")
LEG_OBJ = (OBJ, None, "objects: delivery records, TCP")
LEG_PUB = (PUB, None, "publications: submission records, TCP")


def fig_topology():
    """Interest from three hosts, through two relays, to the edge."""
    f = Fig("topology", 336, "Interest travels hop by hop, from the hosts through the relays to the delivery "
                             "edge, and stops there")
    f.rect(10, 30, 530, 252, PANEL, None)
    f.rect(722, 30, 68, 252, PANEL, None)
    f.cap(275, 22, "the consumer's network")
    f.cap(580, 22, "tunnel")
    f.cap(668, 22, "edge")
    f.cap(756, 22, "fabric")

    f.box(26, 58, 100, 42, "host h1", "wants a, b")
    f.box(26, 118, 100, 42, "host h2", "wants b, c")
    f.box(26, 218, 100, 42, "host h3", "wants d")
    f.box(160, 91, 62, 36, "router", stroke=NOT)
    f.box(262, 84, 126, 50, "relay", "holds a, b, c")
    f.box(400, 150, 126, 50, "relay", "holds a, b, c, d")
    f.box(618, 150, 96, 50, "delivery edge", "holds a, b, c, d")

    # h1 and h2 are two IP hops from their relay: the router only forwards.
    f.flow([(126, 79), (160, 103)], INT, DOTS, arrow=False)
    f.flow([(126, 139), (160, 115)], INT, DOTS, arrow=False)
    f.flow([(222, 109), (262, 109)], INT, DOTS)
    f.flow([(388, 109), (463, 109), (463, 150)], INT, DOTS)
    f.flow([(126, 239), (463, 239), (463, 200)], INT, DOTS)
    f.rect(548, 164, 62, 22, "#1d232b", NOT, 1.0, rx=11)
    f.flow([(526, 175), (618, 175)], INT, DOTS)

    f.label(191, 142, "forwards IP,", 80, size=9.5)
    f.label(191, 153, "holds nothing", 80, size=9.5)
    f.label(325, 76, "two senders below", 126, size=9.5)
    f.label(455, 216, "at the customer edge", 150, size=9.5, anchor="end")
    f.label(290, 232, "one hop: host to relay", 200, size=9.5)
    f.label(666, 216, "one sender", 100, size=9.5)
    f.label(666, 232, "interest stops here", 106, INT, 10.5, True)
    f.label(756, 152, "objects", 64, size=10.5)
    f.label(756, 166, "only,", 64, size=10.5)
    f.label(756, 180, "never", 64, size=10.5)
    f.label(756, 194, "interest", 64, size=10.5)

    f.label(275, 274, "Each host and relay is given the address of the hop above it, in the manner of a "
                      "default route.", 520, size=10.5)
    f.legend(310, [LEG_INT])
    f.text(300, 313.8, "A relay tells the hop above the union of what the hops below it want.", 10.5, False, MUTED)
    f.save()


def _row(f, y, title, desc, relay_sub, edge_sub):
    """One mode: two hosts, the relay, the edge. Returns the diagram's top."""
    f.text(14, y + 14, title, 12.5, True)
    f.text(14, y + 30, desc, 10.5, False, MUTED)
    d = y + 58
    f.box(20, d, 120, 34, "host h1")
    f.box(20, d + 64, 120, 34, "host h2")
    f.box(330, d, 140, 98, "relay", relay_sub)
    f.box(650, d, 130, 98, "delivery edge", edge_sub)
    return d


def fig_modes():
    """What crosses each link in the relay's three modes."""
    f = Fig("modes", 642, "What crosses each link when the relay carries interest only, when it distributes, "
                          "and when it is the publish point")
    f.cap(80, 20, "hosts")
    f.cap(400, 20, "relay")
    f.cap(715, 20, "edge")
    room_l, room_r, xl, xr = 184, 174, 235, 560

    d = _row(f, 30, "Interest only (the default)",
             "The relay carries no objects. The consumer's lane ends where it is provisioned, as it does "
             "without a relay.", "interest only", "ends interest")
    f.flow([(140, d + 17), (330, d + 17)], INT, DOTS)
    f.flow([(140, d + 81), (330, d + 81)], INT, DOTS)
    f.flow([(470, d + 30), (650, d + 30)], INT, DOTS)
    f.label(xl, d + 10, "interest: ANNOUNCE, DIGEST", room_l)
    f.label(xl, d + 31, "answers: ACK, RESYNC", room_l)
    f.label(xr, d + 23, "the union, as one sender", room_r)
    f.flow([(715, d + 98), (715, d + 120), (80, d + 120), (80, d + 98)], OBJ)
    f.label(400, d + 134, "the consumer's lane: the edge dials the provisioned address, here h2", 560)

    d = _row(f, 238, "Distribution (-distribute-listen)",
             "The relay ends the lane and sends each record on to the hosts that asked for its topic at a "
             "delivery address.", "distributor", "delivers as provisioned")
    for o in (0, 64):
        f.flow([(140, d + o + 10), (330, d + o + 10)], INT, DOTS)
        f.flow([(330, d + o + 24), (140, d + o + 24)], OBJ)
    f.label(xl, d + 3, "interest + h1's lane address", room_l)
    f.label(xl, d + 39, "h1's topics: the relay dials h1", room_l)
    f.label(xl, d + 67, "interest + h2's lane address", room_l)
    f.label(xl, d + 103, "h2's topics: the relay dials h2", room_l)
    f.flow([(470, d + 30), (650, d + 30)], INT, DOTS)
    f.flow([(650, d + 68), (470, d + 68)], OBJ)
    f.label(xr, d + 23, "the union + the relay's lane", room_r)
    f.label(xr, d + 83, "the consumer's lane:", room_r)
    f.label(xr, d + 96, "the edge dials the relay", room_r)

    d = _row(f, 412, "Publishing (-publish-listen, with distribution)",
             "Hosts publish to the relay. It publishes for them from the consumer's own address and copies "
             "to their siblings.", "publish point", "to all other consumers")
    f.flow([(140, d + 17), (330, d + 17)], PUB)
    f.flow([(470, d + 30), (650, d + 30)], PUB)
    f.flow([(330, d + 81), (140, d + 81)], OBJ)
    f.label(xl, d + 10, "h1 publishes: h1 dials the relay", room_l)
    f.label(xl, d + 74, "copy at once: a delivery record", room_l)
    f.label(xl, d + 95, "never back to h1", room_l)
    f.label(xr, d + 23, "up the tunnel: the relay dials", room_r)
    f.label(xr, d + 44, "from the consumer's address", room_r)
    f.flow([(650, d + 68), (470, d + 68)], NOT, DASH)
    f.cross(560, d + 68)
    f.label(xr, d + 86, "nothing of the consumer's", room_r)
    f.label(xr, d + 99, "own comes back", room_r)
    f.label(400, d + 120, "Interest and the lane run as in the row above; only the publication's path is "
                          "drawn.", 700)

    f.legend(620, [LEG_INT, LEG_OBJ, LEG_PUB])
    f.save()


def _panel(f, x0, n, title, sub):
    f.rect(x0, 34, 252, 276, PANEL, None)
    f.text(x0 + 14, 54, "%d  %s" % (n, title), 11.5, True)
    f.label(x0 + 14, 69, sub, 232, anchor="start", size=10)
    f.box(x0 + 66, 84, 120, 34, "delivery edge")
    f.box(x0 + 66, 158, 120, 34, "relay")
    f.box(x0 + 14, 236, 96, 34, "host h1")
    f.box(x0 + 142, 236, 96, 34, "host h2")


def fig_publish():
    """Why hosts behind one relay publish to it."""
    f = Fig("publish", 344, "Why hosts behind one relay publish to it: three ways for host h1 to publish, "
                            "and what reaches h1 and its sibling h2")
    f.cap(400, 22, "three ways for h1 to publish an object that h1 and h2 both asked for")

    x0 = 12
    _panel(f, x0, 1, "From its own address", "h1 publishes straight to the edge.")
    f.flow([(x0 + 34, 236), (x0 + 34, 101), (x0 + 66, 101)], PUB)
    f.flow([(x0 + 126, 118), (x0 + 126, 158)], OBJ)
    f.flow([(x0 + 100, 192), (x0 + 78, 236)], OBJ)
    f.flow([(x0 + 152, 192), (x0 + 184, 236)], OBJ)
    f.label(x0 + 135, 142, "down the lane", 110, anchor="start", size=9.5)
    f.label(x0 + 126, 287, "h1 is sent its own object back,", 240, INK, 10)
    f.label(x0 + 126, 301, "and the consumer pays for that copy.", 250, INK, 10)

    x0 = 274
    _panel(f, x0, 2, "From the consumer's address", "h1 publishes straight to the edge.")
    f.flow([(x0 + 34, 236), (x0 + 34, 101), (x0 + 66, 101)], PUB)
    f.flow([(x0 + 126, 118), (x0 + 126, 158)], NOT, DASH)
    f.cross(x0 + 126, 136)
    f.flow([(x0 + 152, 192), (x0 + 184, 236)], NOT, DASH)
    f.label(x0 + 138, 140, "left off the", 110, anchor="start", size=9.5)
    f.label(x0 + 138, 151, "consumer's lane", 110, anchor="start", size=9.5)
    f.label(x0 + 126, 287, "Nothing comes down the lane,", 240, INK, 10)
    f.label(x0 + 126, 301, "so the sibling h2 never gets it.", 250, INK, 10)

    x0 = 536
    _panel(f, x0, 3, "To the relay", "The relay is the publish point.")
    f.flow([(x0 + 78, 236), (x0 + 100, 192)], PUB)
    f.flow([(x0 + 126, 158), (x0 + 126, 118)], PUB)
    f.flow([(x0 + 152, 192), (x0 + 184, 236)], OBJ)
    f.label(x0 + 135, 135, "from the", 110, anchor="start", size=9.5)
    f.label(x0 + 135, 146, "consumer's address", 110, anchor="start", size=9.5)
    f.label(x0 + 180, 207, "copy,", 70, anchor="start", size=9.5)
    f.label(x0 + 180, 218, "at once", 70, anchor="start", size=9.5)
    f.label(x0 + 126, 287, "h2 has it at once, and nothing", 240, INK, 10)
    f.label(x0 + 126, 301, "of h1's own comes back or is paid for.", 250, INK, 10)

    f.legend(328, [LEG_PUB, LEG_OBJ, (NOT, DASH, "does not happen")])
    f.save()


def fig_shapes():
    """The shapes a network of relays takes."""
    f = Fig("shapes", 372, "Four shapes: no relay, one relay at the customer edge, a chain of relays, "
                           "and one relay announcing to two delivery edges")

    def panel(px, py, title, sub):
        f.rect(px, py, 384, 164, PANEL, None)
        f.text(px + 12, py + 20, title, 11.5, True)
        f.label(px + 12, py + 35, sub, 360, anchor="start", size=10)
        return px, py + 46

    x, y = panel(10, 10, "No relay", "The host's own machine holds the tunnel and announces to the edge.")
    f.sbox(x + 40, y + 36, 110, 38, "host", "holds the tunnel")
    f.sbox(x + 250, y + 36, 94, 38, "delivery edge")
    f.flow([(x + 150, y + 55), (x + 250, y + 55)], INT, DOTS)

    x, y = panel(406, 10, "One relay at the customer edge", "Every host is given the relay's address.")
    for i in range(3):
        f.sbox(x + 14, y + 6 + i * 36, 70, 28, "host")
        f.flow([(x + 84, y + 20 + i * 36), (x + 146, y + 48 + (i - 1) * 9)], INT, DOTS)
    f.sbox(x + 146, y + 34, 98, 40, "relay", "customer edge")
    f.sbox(x + 280, y + 35, 94, 38, "delivery edge")
    f.flow([(x + 244, y + 54), (x + 280, y + 54)], INT, DOTS)

    x, y = panel(10, 184, "A chain of relays", "Relays where the parts of a network meet. Each hop adds one "
                                               "to the hop count.")
    for i, site in enumerate(("site A", "site B")):
        yy = y + 8 + i * 54
        f.sbox(x + 8, yy, 62, 38, "hosts", site)
        f.sbox(x + 94, yy, 62, 38, "relay", site)
        f.flow([(x + 70, yy + 19), (x + 94, yy + 19)], INT, DOTS)
        f.flow([(x + 156, yy + 19), (x + 182, y + 46 + i * 16)], INT, DOTS)
    f.sbox(x + 182, y + 34, 94, 40, "relay", "customer edge")
    f.sbox(x + 298, y + 35, 80, 38, "delivery edge")
    f.flow([(x + 276, y + 54), (x + 298, y + 54)], INT, DOTS)

    x, y = panel(406, 184, "Two delivery edges", "A topic goes to every upstream that serves its group.")
    f.sbox(x + 14, y + 35, 70, 38, "hosts")
    f.sbox(x + 122, y + 34, 98, 40, "relay", "-upstream A,B")
    f.flow([(x + 84, y + 54), (x + 122, y + 54)], INT, DOTS)
    for i, n in enumerate(("A", "B")):
        f.sbox(x + 270, y + 6 + i * 58, 104, 38, "delivery edge " + n)
        f.flow([(x + 220, y + 48 + i * 12), (x + 270, y + 25 + i * 58)], INT, DOTS)

    f.legend(360, [LEG_INT])
    f.save()


def seq(name, title, parts, steps):
    """A sequence figure. Steps: ("msg", from, to, text) for what a sender
    says, ("ans", from, to, text) for an answer, ("note", a, b, text) over
    the lifelines a to b, ("loop", text, n) around the next n steps."""
    xs = {2: [200, 600], 3: [130, 400, 670]}[len(parts)]
    y, top = 78.0, 14
    body, frames, open_loops = [], [], []
    f = Fig(name, 0, title)
    for st in steps:
        for lp in open_loops:
            lp[2] -= 1
        if st[0] == "loop":
            open_loops.append([y - 16, st[1], st[2]])
            y += 20
            continue
        if st[0] == "note":
            a, b, text = xs[st[1]], xs[st[2]], st[3]
            w = max(b - a + 60, tw(text, 10.5) + 24)
            cx = (a + b) / 2.0
            cx = min(max(cx, w / 2 + 8), W - w / 2 - 8)
            body.append(("note", cx, y, w, text))
            y += 36
        else:
            body.append((st[0], xs[st[1]], xs[st[2]], y, st[3]))
            y += 34
        for lp in [l for l in open_loops if l[2] <= 0]:
            frames.append((lp[0], y - 14, lp[1]))
            open_loops.remove(lp)
            y += 12
    bottom = y - 6
    f.h = int(bottom + 44)
    for x in xs:
        f.flow([(x, top + 32), (x, bottom)], HAIR, None, 1.2, arrow=False)
    for y0, y1, text in frames:
        f.rect(xs[0] - 104, y0, xs[-1] - xs[0] + 208, y1 - y0, "none", NOT, 1.0, rx=4, dash="4,3")
        f.text(xs[0] - 96, y0 + 14, text, 10, True, MUTED)
    for x, p in zip(xs, parts):
        f.sbox(x - 64, top, 128, 32, p)
    for b in body:
        if b[0] == "note":
            _, cx, yy, w, text = b
            f.rect(cx - w / 2, yy - 16, w, 24, "#1d232b", HAIR, 1.0, rx=4)
            f.text(cx, yy, text, 10.5, False, INK, "middle")
        else:
            kind, x1, x2, yy, text = b
            f.label((x1 + x2) / 2.0, yy - 6, text, abs(x2 - x1) - 12, INK if kind == "msg" else ANS)
            if kind == "msg":
                f.flow([(x1, yy), (x2, yy)], INT, None, 1.6)
            else:
                f.flow([(x1, yy), (x2, yy)], ANS, DASH, 1.3)
    f.legend(f.h - 16, [(INT, None, "from the sender: ANNOUNCE, DIGEST"), (ANS, DASH, "from the receiver: ACK, RESYNC")])
    f.save()


H, R, E = 0, 1, 2
THREE = ("host", "relay", "delivery edge")


def fig_sequences():
    seq("seq-lifecycle", "A topic's life on the wire: first contact, announce, refresh, withdraw", THREE, [
        ("msg", H, R, "ANNOUNCE a, b"),
        ("ans", R, H, "ACK: a token, nothing applied"),
        ("msg", H, R, "ANNOUNCE a, b (with the token)"),
        ("ans", R, H, "ACK: ok, digest of {a, b}, lifetime 300 s"),
        ("msg", R, E, "ANNOUNCE a, b (the union changed)"),
        ("ans", E, R, "ACK: ok"),
        ("loop", "every 100 s", 4),
        ("msg", H, R, "DIGEST of {a, b}"),
        ("ans", R, H, "ACK: matched, everything refreshed"),
        ("msg", R, E, "DIGEST of the union"),
        ("ans", E, R, "ACK: matched"),
        ("msg", H, R, "ANNOUNCE a (lifetime 0: withdraw)"),
        ("ans", R, H, "ACK: ok"),
        ("msg", R, E, "ANNOUNCE a (withdraw)"),
        ("ans", E, R, "ACK: ok"),
    ])
    seq("seq-status", "Each hop answers for itself; what the hop above says later reaches the host after a "
                      "poke", THREE, [
        ("msg", H, R, "ANNOUNCE a, b"),
        ("ans", R, H, "ACK: a token, nothing applied"),
        ("msg", H, R, "ANNOUNCE a, b (with the token)"),
        ("ans", R, H, "ACK: ok"),
        ("msg", R, E, "ANNOUNCE a, b (the union changed)"),
        ("ans", E, R, "ACK: a ok, b refused (outside the bound)"),
        ("ans", R, H, "RESYNC"),
        ("msg", H, R, "DIGEST"),
        ("ans", R, H, "ACK: matched, b held but refused upstream"),
    ])
    seq("seq-refresh", "Soft state: a digest refreshes the whole set, and a silent host's topics lapse",
        ("host", "relay"), [
        ("note", 0, 1, "granted a lifetime of 300 s"),
        ("loop", "every 100 s", 2),
        ("msg", 0, 1, "DIGEST of the whole set"),
        ("ans", 1, 0, "ACK: matched, every topic refreshed"),
        ("note", 0, 0, "the host dies"),
        ("note", 1, 1, "300 s after the last refresh: the topics lapse, and the union shrinks"),
    ])
    seq("seq-restart", "A relay restart: the edge keeps delivering while the relay learns its set again",
        ("hosts", "relay", "delivery edge"), [
        ("note", R, R, "restarts, and holds nothing"),
        ("msg", R, E, "DIGEST (a probe: compares and removes nothing)"),
        ("ans", E, R, "ACK: the groups delivered"),
        ("note", E, E, "keeps what the previous run announced"),
        ("msg", H, R, "DIGEST (the next refresh, within 100 s)"),
        ("ans", R, H, "ACK: a new token: the relay holds nothing"),
        ("msg", H, R, "ANNOUNCE (the whole set, replayed)"),
        ("msg", R, E, "ANNOUNCE (the union, as it is learned)"),
        ("note", R, R, "-restart-hold ends (105 s)"),
        ("msg", R, E, "DIGEST of the union"),
        ("ans", E, R, "ACK: what was not announced again is removed"),
    ])


if __name__ == "__main__":
    fig_topology()
    fig_modes()
    fig_publish()
    fig_shapes()
    fig_sequences()
    for w in WARN:
        print("WARN", w, file=sys.stderr)
    sys.exit(1 if WARN else 0)
