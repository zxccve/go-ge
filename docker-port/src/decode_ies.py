"""decode_ies - the .ies datatables are XOR-0x01 obfuscated."""
import sys
from ge_paths import GAME_ROOT

path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(GAME_ROOT, "ge", "ies", "datatable_static_maps.ies")
limit = int(sys.argv[2]) if len(sys.argv) > 2 else 2000
d = open(path, "rb").read()
dec = bytes(b ^ 1 for b in d)
out = "".join(chr(c) if 32 <= c < 127 else ("\n" if c == 0 else ".") for c in dec)
print(out[:limit])
