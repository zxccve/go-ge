"""Command gap: which protocol commands exist, and which our server answers.

The command universe comes from `data\\v28_binary_names.json` (every `PKS_*` name
the real 2017 server knows - see `history\\shipped-binaries.md`). What we answer is
read from the `CMD_<NAME> = <id>` constants in the server modules.

    python cmd_gap.py              # summary by direction
    python cmd_gap.py --missing CZ # list the CZ_ commands we do not handle

Run it from `src`. Read-only.
"""
import collections
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
NAMES = os.path.join(HERE, "..", "data", "v28_binary_names.json")
MODULES = ("zone_server.py", "ge_barrack_server.py")

# prefix -> (meaning, who sends it)
DIRECTIONS = {
    "CZ": "client -> zone",
    "ZC": "zone -> client",
    "CB": "client -> barrack",
    "BC": "barrack -> client",
    "CH": "client -> hub",
    "HC": "hub -> client",
    "GZ": "global -> zone",
    "ZG": "zone -> global",
    "GW": "global -> workpaper",
    "WG": "workpaper -> global",
    "GS": "global -> server",
    "SG": "server -> global",
    "BG": "barrack -> global",
    "GB": "global -> barrack",
    "HN": "hub -> node",
    "NH": "node -> hub",
    "UH": "user -> hub",
    "HU": "hub -> user",
}


def load_universe():
    with open(NAMES, encoding="utf8") as f:
        names = json.load(f)["pks"]
    return {n[len("PKS_"):] for n in names}


def load_handled():
    handled = set()
    for module in MODULES:
        path = os.path.join(HERE, module)
        if not os.path.exists(path):
            continue
        with open(path, encoding="utf8", errors="replace") as f:
            text = f.read()
        handled |= set(re.findall(r"^CMD_([A-Z0-9_]+)\s*=", text, re.M))
    return handled


def main(argv):
    universe = load_universe()
    handled = load_handled() & universe

    if "--missing" in argv:
        want = argv[argv.index("--missing") + 1].upper()
        missing = sorted(c for c in universe - handled if c.startswith(want + "_"))
        for name in missing:
            print(name)
        print("# %d missing %s_ commands" % (len(missing), want), file=sys.stderr)
        return 0

    by_prefix = collections.Counter(c.split("_")[0] for c in universe)
    print("command universe: %d" % len(universe))
    print("%-5s %-22s %6s %6s %7s" % ("pfx", "direction", "total", "ours", "cover"))
    for prefix, total in by_prefix.most_common():
        done = sum(1 for c in handled if c.split("_")[0] == prefix)
        print("%-5s %-22s %6d %6d %6.1f%%" % (
            prefix, DIRECTIONS.get(prefix, "?"), total, done,
            100.0 * done / total if total else 0.0))
    print()
    print("handled total: %d of %d (%.1f%%)" % (
        len(handled), len(universe), 100.0 * len(handled) / len(universe)))

    # Only the client-facing pairs need packets in a single-process rebuild; the
    # server-to-server ones (GZ/ZG, GW/WG, GS/SG, BG/GB, HN/NH, UH/HU, CH/HC) become
    # ordinary function calls inside our one process.
    facing = {c for c in universe if c.split("_")[0] in ("CZ", "ZC", "CB", "BC")}
    done = handled & facing
    print()
    print("client-facing only (CZ/ZC/CB/BC): %d commands, we answer %d (%.1f%%)" % (
        len(facing), len(done), 100.0 * len(done) / len(facing)))
    print("server-to-server (not needed in one process): %d commands" % (
        len(universe) - len(facing)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
