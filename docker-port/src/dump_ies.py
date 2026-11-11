"""Dump an .ies table to text."""
import sys
from ge_paths import GAME_ROOT
path = os.path.join(GAME_ROOT, "ge", "ies", "datatable_map.ies")
off = int(sys.argv[1]) if len(sys.argv) > 1 else 0
limit = int(sys.argv[2]) if len(sys.argv) > 2 else 2000
d = open(path, "rb").read()
dec = bytes(b ^ 1 for b in d)
chunk = dec[off:off + limit]
out = "".join(chr(c) if 32 <= c < 127 else ("\n" if c == 0 else ".") for c in chunk)
print(out)
