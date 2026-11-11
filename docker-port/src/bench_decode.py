#!/usr/bin/env python3
r"""Decode a capture taken through `bench_relay.py` (the local v28 bench).

Two legs, two codecs:

  c2s  encrypted, `[u16 length][Blowfish body]` - decrypt with ge_crypt, then
       the leading u16 of the plaintext is the command id.
  s2c  PLAINTEXT. Framing (PROVEN 2026-11-05 on an 84-byte chunk holding
       exactly 8 packets - `history\shipped-binaries.md` 2c-vii):

         table size == 0   VARSIZE  {cmd u16, len u16, body...}, len = TOTAL
         table size != 0   FIXSIZE  {cmd u16, body...},        TOTAL = size
                                    NO length field - the 2 bytes after cmd
                                    are payload (e.g. the maxweight value).

       So the id->size table IS the framing authority, and it already exists:
       `data\v28_handler_table.txt`, extracted from the v28 SERVER binary
       (`ZoneServer_Release.exe`). The server writes exactly the size it
       registered, which is what the client parses. The 285146 client carries
       no such table to extract - nothing in its image references the PKS_
       name literals.

Usage (from `docker-port\src`):

    python bench_decode.py c2s "<capture>_c2s.bin"
    python bench_decode.py s2c "<capture>_s2c.bin"
    python bench_decode.py --selftest

Ids are BUILD-SPECIFIC. The table is the 285146 bench build, not the 321819
client this project targets; do not carry its numbers into the Go server.
Measured on 2026-11-05: 3688 = 24-byte position stream (3 per tick, one per
team slot, ~230 ms while walking); 3752 = 16-byte cast (one per slot);
3733 = 24-byte click-at-position.
"""
from __future__ import annotations

import argparse
import os
import struct
import sys

import ge_crypt


def _stamps(bin_path: str):
    """(abs_offset_start, abs_offset_end, timestamp) per captured chunk."""
    out = []
    pos = 0
    try:
        with open(bin_path.replace(".bin", ".tsv"), encoding="utf-8") as fh:
            for line in fh:
                t, n = line.split("\t")
                out.append((pos, pos + int(n), float(t)))
                pos += int(n)
    except OSError:
        pass
    return out


def _ts(stamps, off):
    for a, b, t in stamps:
        if a <= off < b:
            return t
    return stamps[-1][2] if stamps else 0.0


def decode_c2s(path: str, limit: int = 60, table=None) -> int:
    data = open(path, "rb").read()
    stamps = _stamps(path)
    frames = []
    off = 0
    while off + 2 <= len(data):
        ln = struct.unpack_from("<H", data, off)[0]
        if ln == 0 or off + 2 + ln > len(data):
            break
        try:
            plain = ge_crypt.decrypt_packet(data[off:off + 2 + ln])
        except Exception:
            break
        if plain and len(plain) >= 2:
            cmd = struct.unpack_from("<H", plain, 0)[0]
            frames.append((_ts(stamps, off), cmd, len(plain), plain))
        off += 2 + ln
    print("%s: %d bytes, %d packets" % (path, len(data), len(frames)))
    if not frames:
        return 0
    t0 = frames[0][0]
    counts = {}
    for _, cmd, _, _ in frames:
        counts[cmd] = counts.get(cmd, 0) + 1
    print("commands:")
    for cmd, n in sorted(counts.items(), key=lambda kv: -kv[1])[:12]:
        print("   %-5d %-44s %d" % (cmd, (table or {}).get(cmd, (0, "?"))[1], n))
    for t, cmd, ln, plain in frames[:limit]:
        print("  %8.1fms  cmd=%-5d len=%-4d  %-40s %s"
              % ((t - t0) * 1000, cmd, ln, (table or {}).get(cmd, (0, "?"))[1],
                 plain[2:18].hex(" ")))
    return len(frames)


def load_table(path: str | None = None):
    """id -> (size, name) from the v28 server's registration table."""
    if path is None:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                            "..", "data", "v28_handler_table.txt")
    table = {}
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            if line.startswith("#"):
                continue
            parts = line.rstrip("\n").split("\t")
            if len(parts) >= 3:
                table[int(parts[0], 16)] = (int(parts[1]), parts[2])
    return table


def parse_s2c(data: bytes, table):
    """Frames -> (frames, stop); frames = [(off, cmd, total, name)]."""
    off = 0
    frames = []
    while off + 4 <= len(data):
        cmd, ln = struct.unpack_from("<HH", data, off)
        if cmd in table:
            size, name = table[cmd]
            if size:
                total = size
            else:
                if ln < 4:
                    return frames, (off, cmd, "varsize with len %d" % ln)
                total = ln
        else:
            # Unknown to the table: a non-zero length is all we can trust.
            if ln < 4:
                return frames, (off, cmd, "unknown id, len %d" % ln)
            total = ln
            name = "?%#06x" % cmd
        if off + total > len(data):
            return frames, (off, cmd, "truncated packet (%d > %d)"
                            % (total, len(data) - off))
        frames.append((off, cmd, total, name))
        off += total
    return frames, None


def decode_s2c(path: str, limit: int = 60, table=None) -> int:
    data = open(path, "rb").read()
    stamps = _stamps(path)
    table = table or load_table()
    frames, stop = parse_s2c(data, table)
    print("%s: %d bytes, %d packets%s"
          % (path, len(data), len(frames), "" if stop is None else " (INCOMPLETE)"))
    if not frames:
        if stop:
            print("  STOP at %d: cmd=%d %s" % stop)
        return 0
    t0 = stamps[0][2] if stamps else 0.0
    counts = {}
    for _o, _c, _t, name in frames:
        counts[name] = counts.get(name, 0) + 1
    print("commands:")
    for name, n in sorted(counts.items(), key=lambda kv: -kv[1]):
        print("   %-46s %d" % (name, n))
    print("first %d packets:" % min(limit, len(frames)))
    for off, cmd, total, name in frames[:limit]:
        print("  %8.1fms  cmd=%-5d len=%-5d %-44s %s"
              % ((_ts(stamps, off) - t0) * 1000, cmd, total, name,
                 data[off + 2:off + 18].hex(" ")))
    if stop:
        print("  STOP at offset %d: cmd=%d %s" % stop)
    return len(frames)


def _selftest() -> int:
    """Round-trip: build a stream from the table with both packet shapes."""
    table = load_table()
    warm = [(c, s) for c, (s, _n) in sorted(table.items()) if s]
    var = [(c, s) for c, (s, _n) in sorted(table.items()) if not s]
    if not warm or not var:
        print("selftest: table is empty")
        return 1
    stream = bytearray()
    expect = []
    for i in range(30):
        if i % 3 == 0:
            cmd, size = warm[i % len(warm)]
            stream += struct.pack("<H", cmd) + b"\xAA" * (size - 2)
        else:
            cmd, _s = var[i % len(var)]
            body = bytes([i]) * 12
            stream += struct.pack("<HH", cmd, 4 + len(body)) + body
        expect.append(cmd)
    frames, stop = parse_s2c(bytes(stream), table)
    got = [f[1] for f in frames]
    ok = stop is None and got == expect
    print("selftest: %d packets, stop=%s, ids match=%s -> %s"
          % (len(frames), stop, got == expect, "PASS" if ok else "FAIL"))
    return 0 if ok else 1


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Decode a bench capture (c2s or s2c).")
    ap.add_argument("leg", nargs="?", choices=["c2s", "s2c"])
    ap.add_argument("capture", nargs="?", help="the *_c2s.bin / *_s2c.bin file")
    ap.add_argument("--limit", type=int, default=60)
    ap.add_argument("--table", help="id->size table (default data\\v28_handler_table.txt)")
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args(argv)
    if a.selftest:
        return _selftest()
    if not a.leg or not a.capture:
        ap.error("leg and capture are required")
    table = load_table(a.table)
    if a.leg == "c2s":
        decode_c2s(a.capture, a.limit, table)
    else:
        decode_s2c(a.capture, a.limit, table)
    return 0


if __name__ == "__main__":
    sys.exit(main())
