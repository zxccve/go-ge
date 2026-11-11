"""check_portability - what would break if the server ran on Linux?

Usage:
    python check_portability.py [--quiet]

The plan is a Go server on a Linux VPS, so the SERVER process must not depend on
anything Windows-only. Dev tools (capture, mem scan, crash dumps, the Ghidra
helpers) are allowed to - they never ship.

This checks the modules a server process actually imports, for:

  * `subprocess` / `os.system` / `os.popen` - a shelled-out helper is usually a
    Windows exe (today: `sqlcmd` in ge_db, `pathtool.exe` in npc_data);
  * `ctypes.windll` / `winreg` / `os.startfile` - Win32 API;
  * absolute Windows paths ("<path>", "<path>") - a baked install location;
  * non-ASCII stdout assumptions are NOT checked: that is a runtime bug, not a
    portability one.

Exit code 1 when anything is found, so it can gate a commit. `--quiet` prints the
counts only.
"""
import os
import re
import sys

# The modules a server process imports. Keep this list to the SERVER; a tool such
# as capture_live.py or mem_scan.py is expected to be Windows-only.
SERVER_MODULES = [
    "ge_crypt.py", "ge_db.py", "ge_locale.py", "ge_item_props.py",
    "stance_data.py", "skill_data.py", "npc_data.py", "world.py",
    "ge_barrack_server.py", "zone_server.py",
]

CHECKS = [
    ("shell-out", re.compile(r"\b(subprocess|os\.system|os\.popen)\b")),
    ("win32-api", re.compile(r"\b(ctypes\.windll|windll|winreg|os\.startfile)\b")),
    ("abs-win-path", re.compile(r"[\"'][A-Za-z]:[\\/]")),
]


def scan(path):
    hits = []
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            for n, line in enumerate(f, 1):
                if line.lstrip().startswith("#"):
                    continue
                for name, rx in CHECKS:
                    if rx.search(line):
                        hits.append((n, name, line.strip()[:110]))
                        break
    except OSError as exc:
        hits.append((0, "error", str(exc)))
    return hits


def main():
    quiet = "--quiet" in sys.argv
    here = os.path.dirname(os.path.abspath(__file__))
    total = 0
    counts = {}
    for name in SERVER_MODULES:
        path = os.path.join(here, name)
        if not os.path.exists(path):
            continue
        hits = scan(path)
        if not hits:
            if not quiet:
                print("%-22s clean" % name)
            continue
        for n, kind, text in hits:
            counts[kind] = counts.get(kind, 0) + 1
            total += 1
            if not quiet:
                print("%-22s %4d  %-12s %s" % (name, n, kind, text))
    print("%d Windows-only dependency site(s) in the server: %s"
          % (total, ", ".join("%s=%d" % kv for kv in sorted(counts.items())) or "none"))
    return 1 if total else 0


if __name__ == "__main__":
    sys.exit(main())
