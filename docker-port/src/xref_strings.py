"""Find cross-references to strings in ge.exe."""
import struct
from ge_paths import GAME_ROOT

d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()

# .text
t_off, t_va, t_raw = 0x400, 0x1000, 6058496
# .rdata
r_off, r_va, r_raw = 0x5c7600, 0x5c9000, 1182208

def va_of(off):
    if t_off <= off < t_off + t_raw:
        return off - t_off + t_va
    if r_off <= off < r_off + r_raw:
        return off - r_off + r_va
    return None

IMG = 0x400000
targets = {}
for s in [b"commander_lv_current", b"commander_name", b"commandername",
          b"commander_lv_max", b"party_name", b"selectteam"]:
    off = d.find(s)
    if off >= 0:
        targets[s.decode()] = IMG + va_of(off)
print("string VAs:", {k: hex(v) for k, v in targets.items()})

# search .text for 4-byte little-endian VA references
text = d[t_off:t_off + t_raw]
for name, va in targets.items():
    pat = struct.pack("<I", va)
    hits = []
    i = text.find(pat)
    while i >= 0 and len(hits) < 12:
        hits.append(t_off + i)
        i = text.find(pat, i + 1)
    print(f"{name} @ {va:#x} -> refs at VAs: {[hex(va_of(h)) for h in hits]}")

try:
    import capstone
    print("capstone available", capstone.__version__)
except Exception as ex:
    print("capstone NOT available:", ex)
