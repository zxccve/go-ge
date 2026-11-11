"""Quick re-check of the two 818-byte captures (read-only analysis)."""
import os
import re, zlib, gzip, io
from collections import Counter

from ge_paths import CAPTURES


def grabs(path):
    txt = open(path, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    return [bytes(int(x, 16) for x in h.split()) for h in ms]


# barrack_stub.log holds HELLO-TEST (16B) + old 818B; take the 818 one
old = [p for p in grabs(os.path.join(CAPTURES, "barrack_stub.log")) if len(p) == 818][0]
new = grabs(os.path.join(CAPTURES, "barrack_stub2.log"))[0]
print("old len", len(old), "new len", len(new))

for name, pkt in (("old", old), ("new", new)):
    body = pkt[2:]
    print(name, "hdr", pkt[:2].hex(), "= len", int.from_bytes(pkt[:2], "little"))
    print(name, "entropy/byte (0-8):",
          round(len(set(body)) and 0, 3))
    # repeated 8-byte blocks + positions
    c = Counter(body[i:i+8] for i in range(0, len(body)-7, 8))
    hot = c.most_common(1)[0]
    print(name, "top 8B blocks:", [(b.hex(), n) for b, n in c.most_common(4)])
    hb = hot[0]
    pos = [i for i in range(0, len(body)-7, 8) if body[i:i+8] == hb]
    print(name, "hot block idx:", pos)
    # all positions (any alignment)
    posall = [i for i in range(0, len(body)-7) if body[i:i+8] == hb]
    print(name, "hot block ANY-align off:", posall)

# where do old/new agree (8-byte blocks, payload start aligned)
print("\nblock-aligned equal old/new (payload off 2):")
same = [i // 8 for i in range(0, 816 - 7, 8) if old[2+i:2+i+8] == new[2+i:2+i+8]]
print(" idx:", same)
# try decompress
for name, pkt in (("old", old), ("new", new)):
    for tag, fn in (("zlib", lambda b: zlib.decompress(b)),
                    ("gzip", lambda b: gzip.decompress(b)),
                    ("raw-deflate", lambda b: zlib.decompress(b, -15))):
        for off in (2, 0, 4, 6):
            try:
                d = fn(pkt[off:])
                print("DECOMPRESS", name, tag, "off", off, "->", len(d), d[:64])
            except Exception:
                pass
# ascii runs
for name, pkt in (("old", old), ("new", new)):
    runs = re.findall(rb"[\x20-\x7e]{5,}", pkt)
    print(name, "ascii runs:", runs[:10])
