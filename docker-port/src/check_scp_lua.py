"""Compile every translated script with a real Lua and report the failures.

This is the proof that `scp_to_lua.py` produces valid Lua, not just text that
looks balanced. It compiles only (no run): scripts call ~2,400 builtins that
would each need a stub, and a compile error is the thing being measured here.

    python check_scp_lua.py               # whole corpus, summary + first 20
    python check_scp_lua.py --failures 100
    python check_scp_lua.py ge\\script\\util.scp

Needs `lupa` (Lua 5.x inside Python). The structural check in `scp_to_lua.py
--check` runs without it; use this when you want syntax proved.
"""

import argparse
import os
import sys

from ge_paths import GE_SCRIPT
from scp_to_lua import translate_file


def lua_runtime():
    from lupa import LuaRuntime
    return LuaRuntime(unpack_returned_tuples=True)


def compile_all(root=None, limit=None):
    """Returns (total, good, [(relative path, message)])."""
    root = root or GE_SCRIPT
    files = sorted(os.path.join(d, f) for d, _s, fs in os.walk(root)
                   for f in fs if f.endswith(".scp"))
    if limit:
        files = files[:limit]
    lua = lua_runtime()
    bad = []
    for path in files:
        rel = os.path.relpath(path, root)
        try:
            lua.compile(translate_file(path), name=rel)
        except Exception as exc:                 # LuaSyntaxError, plus I/O
            bad.append((rel, str(exc).splitlines()[0][:160]))
    return len(files), len(files) - len(bad), bad


def main():
    ap = argparse.ArgumentParser(description="Compile translated .scp files with Lua.")
    ap.add_argument("files", nargs="*", help="specific .scp files (default: whole corpus)")
    ap.add_argument("--failures", type=int, default=20, help="how many failures to print")
    args = ap.parse_args()

    if args.files:
        lua = lua_runtime()
        bad = 0
        for path in args.files:
            try:
                lua.compile(translate_file(path), name=os.path.basename(path))
                print(f"ok      {path}")
            except Exception as exc:
                bad += 1
                print(f"FAIL    {path}: {str(exc).splitlines()[0][:160]}")
        return 1 if bad else 0

    total, good, bad = compile_all()
    print(f"files    : {total}")
    print(f"compiles : {good}  ({good * 100 // max(total, 1)}%)")
    print(f"fails    : {len(bad)}")
    for rel, msg in bad[:args.failures]:
        print(f"  {rel}: {msg}")
    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
