"""Which LUA features does the translated .scp corpus actually need?

The question this answers: could a pure-Go Lua (gopher-lua is Lua 5.1) run our
scripts, or do we need a real Lua 5.3+/5.4+/5.5 library through cgo? Guessing
from the source dialect is wrong, because the source is NOT Lua - `scp_to_lua.py`
translates it. So the census is run on the EMITTED Lua, with strings and comments
stripped so a word inside a dialog line is not counted as a keyword.

Usage:
    python check_lua_features.py              # whole corpus, summary
    python check_lua_features.py --examples   # up to 3 files per feature
    python check_lua_features.py path.scp     # one file, detailed

Nothing is written and nothing is compiled: it is a text census, so it runs in
seconds. `check_scp_lua.py` is the one that proves the scripts COMPILE.
"""

import argparse
import os
import re
import sys

from ge_paths import GE_SCRIPT
from scp_to_lua import translate_file

# feature -> regex over STRIPPED lua (strings and comments already removed)
FEATURES = {
    # Lua 5.2+
    "goto/label":        re.compile(r"\bgoto\s+[A-Za-z_]|\b::[A-Za-z_]"),
    "table.unpack":      re.compile(r"\btable\.unpack\b"),
    "bit32":             re.compile(r"\bbit32\."),
    # Lua 5.3+ (integers, bitwise, integer division)
    "integer //":        re.compile(r"//"),
    "bitwise &":         re.compile(r"(?<![&])&(?![&])"),
    "bitwise |":         re.compile(r"(?<![|])\|(?![|])"),
    "bitwise ~ xor":     re.compile(r"(?<![~=])~(?![=])"),
    "shift << >>":       re.compile(r"<<|>>"),
    "math.type":         re.compile(r"\bmath\.type\b"),
    "math.tointeger":    re.compile(r"\bmath\.tointeger\b"),
    "string.pack":       re.compile(r"\bstring\.pack\b|\bstring\.unpack\b"),
    # Lua 5.4+
    "<const>/<close>":   re.compile(r"<const>|<close>"),
    "coroutine.close":   re.compile(r"\bcoroutine\.close\b"),
    # Lua 5.5+
    "global decl":       re.compile(r"\bglobal\s+[A-Za-z_]"),
    # 5.1-ONLY constructs: present means the emitted code is NOT 5.2+ clean
    "5.1 unpack":        re.compile(r"(?<![\w.])unpack\s*\("),
    "5.1 module()":      re.compile(r"\bmodule\s*\("),
}

# Lua 5.1 is the floor for everything else; these lift the floor.
VERSION_FLOOR = [
    ("goto/label", "5.2"), ("table.unpack", "5.2"), ("bit32", "5.2"),
    ("integer //", "5.3"), ("bitwise &", "5.3"), ("bitwise |", "5.3"),
    ("bitwise ~ xor", "5.3"), ("shift << >>", "5.3"), ("math.type", "5.3"),
    ("math.tointeger", "5.3"), ("string.pack", "5.3"),
    ("<const>/<close>", "5.4"), ("coroutine.close", "5.4"),
    ("global decl", "5.5"),
]


def strip_lua(text):
    """Remove Lua comments and string literals, keeping the code.

    A census that counts keywords inside dialog text is worse than no census: it
    reports a version requirement that does not exist. Long strings (`[[ ]]`),
    line comments (`-- ...`) and short strings with escapes are handled here.
    """
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c == "-" and text.startswith("--", i):
            if text.startswith("--[[", i) or text.startswith("--[=[", i):
                end = text.find("]]", i)
                i = n if end < 0 else end + 2
            else:
                end = text.find("\n", i)
                i = n if end < 0 else end
            continue
        if c == "[" and text.startswith("[[", i):
            end = text.find("]]", i)
            i = n if end < 0 else end + 2
            continue
        if c in "\"'":
            quote = c
            i += 1
            while i < n:
                if text[i] == "\\":
                    i += 2
                    continue
                if text[i] == quote:
                    i += 1
                    break
                i += 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


def scan_text(lua_text):
    """feature -> number of occurrences in one translated script."""
    code = strip_lua(lua_text)
    return {name: len(rx.findall(code)) for name, rx in FEATURES.items()}


def scan_corpus(root=None):
    """Returns (filenames, {feature: [files that use it]})."""
    root = root or GE_SCRIPT
    files = sorted(os.path.join(d, f) for d, _s, fs in os.walk(root)
                   for f in fs if f.endswith(".scp"))
    hits = {name: [] for name in FEATURES}
    names = []
    for path in files:
        rel = os.path.relpath(path, root)
        names.append(rel)
        try:
            counts = scan_text(translate_file(path))
        except Exception:                    # a file the translator rejects is
            continue                         # check_scp_lua.py's business
        for feat, count in counts.items():
            if count:
                hits[feat].append(rel)
    return names, hits


def floor_from(hits):
    """The lowest Lua version that covers what the corpus uses."""
    order = ["5.1", "5.2", "5.3", "5.4", "5.5"]
    need = "5.1"
    for feat, version in VERSION_FLOOR:
        if hits.get(feat):
            if order.index(version) > order.index(need):
                need = version
    return need


def main():
    ap = argparse.ArgumentParser(description="Census of Lua features in the .scp corpus.")
    ap.add_argument("files", nargs="*", help="specific .scp files (default: whole corpus)")
    ap.add_argument("--examples", action="store_true", help="list up to 3 files per feature")
    args = ap.parse_args()

    if args.files:
        for path in args.files:
            counts = scan_text(translate_file(path))
            print(os.path.basename(path))
            for feat, count in counts.items():
                if count:
                    print(f"  {count:5d}  {feat}")
        return 0

    names, hits = scan_corpus()
    print(f"scripts scanned : {len(names)}")
    print(f"Lua floor       : {floor_from(hits)}")
    print()
    print(f"{'feature':18} {'files':>7}  needs")
    for feat, _rx in FEATURES.items():
        used = hits[feat]
        if not used:
            continue
        version = dict(VERSION_FLOOR).get(feat, "5.1 only")
        print(f"{feat:18} {len(used):7d}  {version}")
        if args.examples:
            for rel in used[:3]:
                print(f"{'':18}          {rel}")
    unused = [f for f, used in hits.items() if not used]
    if unused:
        print()
        print("not used anywhere:", ", ".join(unused))
    return 0


if __name__ == "__main__":
    sys.exit(main())
