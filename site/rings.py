"""Erzeugt die Hero-Figur der ctx-Homepage: sieben zyklische Dimensionen als
konzentrische Ringe, der Wochentag-Ring trägt die Gauß-Aktivierung für
"every Tuesday". Nur Präsentationsattribute + Klassen (CSP ohne unsafe-inline)."""
import math

CX, CY = 220, 220
RINGS = [  # innen nach außen
    ("daily", 46),
    ("weekday", 72),
    ("monthday", 96),
    ("week", 118),
    ("month", 140),
    ("quarter", 162),
    ("seasonal", 184),
]
ACTIVE = "weekday"
DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
TUE = 1


def pt(r, a):
    return CX + r * math.cos(a), CY + r * math.sin(a)


def ang(i, n):
    # 0 oben, im Uhrzeigersinn
    return -math.pi / 2 + 2 * math.pi * i / n


out = []
out.append('<svg class="rings" viewBox="0 0 520 430" role="img" '
           'aria-labelledby="rings-title rings-desc">')
out.append('<title id="rings-title">Cyclic temporal gravity</title>')
out.append('<desc id="rings-desc">Seven concentric rings, one per time cycle: '
           'time of day, weekday, day of month, week, month, quarter, season. '
           'The query &#8220;every Tuesday&#8221; raises a Gaussian peak on the '
           'weekday ring at Tuesday; the other rings stay flat.</desc>')

for name, r in RINGS:
    cls = "ring ring-active" if name == ACTIVE else "ring"
    out.append(f'<circle class="{cls}" cx="{CX}" cy="{CY}" r="{r}"/>')

# Wochentag-Ticks
r0 = dict(RINGS)[ACTIVE]
for i, d in enumerate(DAYS):
    a = ang(i, 7)
    x1, y1 = pt(r0 - 4, a)
    x2, y2 = pt(r0 + 4, a)
    out.append(f'<line class="tick" x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}"/>')

# Gauß-Ausschlag um Dienstag: nur das Band zwischen Ring und Kurve, ±3 Sigma
A, SIG = 34.0, 0.36  # Amplitude px, Sigma in Tagen
N = 120
span = [TUE - 3 * SIG + 6 * SIG * k / N for k in range(N + 1)]
outer = [pt(r0 + A * math.exp(-(t - TUE) ** 2 / (2 * SIG * SIG)), ang(t, 7)) for t in span]
inner = [pt(r0, ang(t, 7)) for t in reversed(span)]
pts = " ".join(f"{x:.1f},{y:.1f}" for x, y in outer + inner)
out.append(f'<polygon class="swell" points="{pts}"/>')

# Peak-Marker + Beschriftung
a = ang(TUE, 7)
px, py = pt(r0 + A, a)
out.append(f'<circle class="peak" cx="{px:.1f}" cy="{py:.1f}" r="4"/>')
lx, ly = pt(206, a)
out.append(f'<line class="leader" x1="{px:.1f}" y1="{py:.1f}" x2="{lx:.1f}" y2="{ly:.1f}"/>')
out.append(f'<text class="q" x="{lx + 6:.1f}" y="{ly - 6:.1f}">&#8220;every Tuesday&#8221;</text>')
out.append(f'<text class="q-sub" x="{lx + 6:.1f}" y="{ly + 12:.1f}">weekday · Tue</text>')

# Ring-Namen links, entlang der Neun-Uhr-Achse
labels = {"daily": "time of day", "monthday": "day of month", "seasonal": "season"}
for name, r in RINGS:
    x, y = pt(r, math.pi / 2)
    cls = "rl rl-active" if name == ACTIVE else "rl"
    out.append(f'<text class="{cls}" x="{x:.1f}" y="{y:.1f}" text-anchor="middle" '
               f'dominant-baseline="central" paint-order="stroke">{labels.get(name, name)}</text>')

out.append("</svg>")
print("\n".join(out))
