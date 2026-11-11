#!/usr/bin/env python3
"""Read IPF members whose LOCAL header name is ciphertext (and whose data is
PKZIP-traditionally encrypted) - the archives `ipf_unpack.py` cannot touch.

Why this exists: an `.ipf` is a zip with encrypted names. Most archives store the
local header name in plain form, so `lib.imczipfile` opens them. Some do not:
`ge\\dictionary.ipf` (all 140 members), the `uixml\\*.xml` members of `ge\\ui.ipf`
and at least one `.ies` in `ge\\ies10000.ipf` fail there with

    BadZipFile: File name in directory 'uixml\\accomplishment.xml' and
    header b'\\x81\\x93...' differ.

The CENTRAL directory is fine and carries the real name, sizes, CRC and offset, so
this module takes the entry from there, decrypts from the local header on with the
same `ZipDecrypter` + IPF key the shipped reader uses, skips the 12-byte encryption
header, and inflates. Identical bytes to what the game reads.

Usage (run from `docker-port\\src`):

    python ipf_read_encrypted.py --list   "<path>"
    python ipf_read_encrypted.py --hist   "<path>"
    python ipf_read_encrypted.py -o OUT --only "uixml/*.xml" "...\\ge\\ui.ipf"
    python ipf_read_encrypted.py --stat   "<path>"

`--stat` prints name, size, encrypt flag and timestamp per member WITHOUT
decrypting (the central directory already has them), and is what to run first on a
683 MB dictionary: decrypting every member runs at ~4 MB/s, so read only what you
need. `--hist` counts members and file extensions - again no writing.

The IPF key and reader live in the shipped server tools; `GE_IPF_TOOLS` overrides
the path. Importing this module does nothing (`main()` is guarded).
"""
from __future__ import annotations

import argparse
import binascii
import collections
import fnmatch
import glob
import os

import struct
import sys
import zlib
from ge_paths import GAME_ROOT

TOOLS = os.environ.get("GE_IPF_TOOLS", os.path.join(GAME_ROOT, "v28", "rGE_server", "tools"))
KEY = (
    "256620256620257320257320FFFFFFFF3F5B20FFFFFFFF3F"
    "25662025662025732068202573202E3F2E20202058FF2424"
)

if TOOLS not in sys.path:
    sys.path.insert(0, TOOLS)

from lib.imczipfile import ZipFile, ZIP_DEFLATED, ZipDecrypter  # noqa: E402


def open_archive(path: str) -> ZipFile:
    return ZipFile(path, "r", ZIP_DEFLATED, True, binascii.unhexlify(KEY))


def read_member(path: str, info, pwd: bytes) -> bytes:
    """Return one member's bytes, decrypting when the entry is flagged encrypted."""
    enc = bool(info.flag_bits & 0x1)
    with open(path, "rb") as fp:
        fp.seek(info.header_offset)
        head = fp.read(30)
        if head[:4] != b"PK\x03\x04":
            raise ValueError(f"bad local header at {info.header_offset}")
        name_len, extra_len = struct.unpack_from("<HH", head, 26)
        fp.seek(info.header_offset + 30 + name_len + extra_len)
        data = fp.read(info.compress_size + (12 if enc else 0))
    if enc:
        data = ZipDecrypter(pwd)(data)[12:]
    if info.compress_type == 8:
        return zlib.decompress(data, -15)
    return data


def expand(patterns: list[str]) -> list[str]:
    out: list[str] = []
    for pat in patterns:
        hits = sorted(glob.glob(pat))
        if not hits and os.path.isfile(pat):
            hits = [pat]
        out.extend(h for h in hits if h not in out)
    return out


def cmd_stat(archives: list[str]) -> int:
    pwd = binascii.unhexlify(KEY)
    for path in archives:
        infos = open_archive(path).infolist()
        enc = sum(1 for i in infos if i.flag_bits & 0x1)
        print(f"== {os.path.basename(path)}: {len(infos)} members, {enc} encrypted")
        for i in infos:
            flag = "enc" if i.flag_bits & 1 else "raw"
            print(f"   {i.filename:<34} {i.file_size:>10} {flag} {i.date_time}")
    return 0


def cmd_list(archives: list[str]) -> int:
    for path in archives:
        names = open_archive(path).namelist()
        print(f"== {os.path.basename(path)}: {len(names)} members")
        for n in sorted(names):
            print("   ", n)
    return 0


def cmd_hist(archives: list[str]) -> int:
    for path in archives:
        infos = open_archive(path).infolist()
        names = [i.filename for i in infos]
        ext = collections.Counter(
            ("." + n.rsplit(".", 1)[-1].lower()) if "." in n else "(none)" for n in names
        )
        print(f"{os.path.basename(path):<22} members={len(infos):>6} unique={len(set(names)):>6}  {ext.most_common(8)}")
    return 0


def cmd_extract(archives: list[str], outdir: str, only: str | None) -> int:
    pwd = binascii.unhexlify(KEY)
    for path in archives:
        infos = open_archive(path).infolist()
        sel = {}
        for i in infos:
            if only and not fnmatch.fnmatch(i.filename.lower(), only.lower()):
                continue
            sel[i.filename] = i          # last copy wins: the patch stack appends
        written = 0
        for name, info in sel.items():
            try:
                data = read_member(path, info, pwd)
            except Exception as exc:  # noqa: BLE001 - report and keep going
                print(f"   FAIL {name}: {type(exc).__name__}: {exc}")
                continue
            dest = os.path.join(outdir, name.replace("\\", "/"))
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            with open(dest, "wb") as fh:
                fh.write(data)
            written += 1
        print(f"== {os.path.basename(path)}: {len(infos)} members, wrote {written} to {outdir}")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Read IPF members that ipf_unpack.py cannot (encrypted names).")
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--stat", action="store_true",
                      help="per-member name, size, encrypt flag, timestamp (no decrypt)")
    mode.add_argument("--list", action="store_true", help="print member names")
    mode.add_argument("--hist", action="store_true",
                      help="member count, unique count and extension histogram (no decrypt)")
    mode.add_argument("-o", "--out", metavar="DIR", help="extract matching members")
    ap.add_argument("--only", metavar="GLOB", help="with -o: member glob, e.g. 'uixml/*.xml'")
    ap.add_argument("archives", nargs="+", help="one or more .ipf files")
    args = ap.parse_args(argv)

    archives = expand(args.archives)
    if not archives:
        print("no archive matched", file=sys.stderr)
        return 2
    if args.stat:
        return cmd_stat(archives)
    if args.list:
        return cmd_list(archives)
    if args.hist:
        return cmd_hist(archives)
    return cmd_extract(archives, args.out, args.only)


if __name__ == "__main__":
    raise SystemExit(main())
