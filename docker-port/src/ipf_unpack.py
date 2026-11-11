#!/usr/bin/env python3
"""Read Granado Espada `.ipf` archives (list or extract) WITHOUT multiprocessing.

Why this exists: the shipped tool `v28\\rGE_server\\tools\\ipf2.py` does the same
job but routes every archive through `multiprocessing`, and in this environment
that dies at process spawn with

    PermissionError: [WinError 5] Access is denied   (reduction.duplicate)

so it cannot be used here at all. This script imports the SAME reader
(`lib.imczipfile.ZipFile`) and the SAME key, so archives are unpacked
identically - it only drops the worker pool.

Usage (run from `docker-port\\src`):

    python ipf_unpack.py --list  "<path>"
    python ipf_unpack.py --grep barrack <path>
    python ipf_unpack.py -f OUTDIR  <path>

The `--grep` mode only prints matching member names and touches no disk - use
it first, so a 350 MB UI archive is not unpacked to read one file.

Importing this module does nothing (main() is guarded).
"""
from __future__ import annotations

import argparse
import binascii
import fnmatch
import glob
import os

import sys
from ge_paths import GAME_ROOT

# The reader and the archive key both live in the shipped server tools.
TOOLS = os.environ.get("GE_IPF_TOOLS", os.path.join(GAME_ROOT, "v28", "rGE_server", "tools"))
KEY = (
    "256620256620257320257320FFFFFFFF3F5B20FFFFFFFF3F"
    "25662025662025732068202573202E3F2E20202058FF2424"
)

if TOOLS not in sys.path:
    sys.path.insert(0, TOOLS)

from lib.imczipfile import ZipFile, ZIP_DEFLATED  # noqa: E402


def expand(patterns: list[str]) -> list[str]:
    """Glob each pattern ourselves, so the caller does not rely on the shell."""
    out: list[str] = []
    for pat in patterns:
        hits = glob.glob(pat)
        if not hits and os.path.isfile(pat):
            hits = [pat]
        for h in hits:
            if h not in out:
                out.append(h)
    return out


def open_archive(path: str) -> ZipFile:
    return ZipFile(path, "r", ZIP_DEFLATED, True, binascii.unhexlify(KEY))


def cmd_list(archives: list[str]) -> int:
    for path in archives:
        names = open_archive(path).namelist()
        print(f"== {os.path.basename(path)}  ({len(names)} members)")
        for n in sorted(names):
            print("   ", n)
    return 0


def cmd_grep(archives: list[str], needle: str) -> int:
    """Print member names matching `needle` (case-insensitive). Does not write."""
    n = needle.lower()
    total = 0
    for path in archives:
        names = open_archive(path).namelist()
        hits = [x for x in names if n in x.lower()]
        total += len(hits)
        if hits:
            print(f"== {os.path.basename(path)}  ({len(names)} members)")
            for x in sorted(hits):
                print("   ", x)
    print(f"-- {total} match(es) for {needle!r}")
    return 0


def cmd_extract(archives: list[str], outdir: str, only: str | None,
                force: bool) -> int:
    os.makedirs(outdir, exist_ok=True)
    written = skipped = 0
    for path in archives:
        z = open_archive(path)
        names = z.namelist()
        for name in names:
            if only and not fnmatch.fnmatch(name.lower(), only.lower()):
                continue
            dest = os.path.join(outdir, name.replace("/", os.sep))
            if os.path.exists(dest) and not force:
                skipped += 1
                continue
            z.extract(name, outdir)
            written += 1
        print(f"== {os.path.basename(path)}: {len(names)} members")
    print(f"-- wrote {written} file(s) to {outdir}, kept {skipped} existing")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--list", action="store_true", help="print member names")
    mode.add_argument("--grep", metavar="TEXT",
                      help="print member names containing TEXT (no writing)")
    mode.add_argument("-o", "--out", metavar="DIR",
                      help="extract every member into DIR")
    ap.add_argument("-f", "--force", action="store_true",
                    help="overwrite files already in DIR")
    ap.add_argument("--only", metavar="GLOB",
                    help="with -o: extract only members matching GLOB")
    ap.add_argument("archives", nargs="+", help="one or more .ipf files")
    args = ap.parse_args(argv)

    archives = expand(args.archives)
    if not archives:
        print("no archive matched", file=sys.stderr)
        return 2

    if args.list:
        return cmd_list(archives)
    if args.grep is not None:
        return cmd_grep(archives, args.grep)
    return cmd_extract(archives, args.out, args.only, args.force)


if __name__ == "__main__":
    raise SystemExit(main())
