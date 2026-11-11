"""extract_replay_packets - cut a capture into one file per packet.

Usage:
    python extract_replay_packets.py <capture.bin> [--out DIR] [--cmd ID ...]

It exists so a "replay" file is always reproducible from a capture instead of
being a hand-copied blob: run this on a capture and the replay file is written
by the same code that reads the capture.

FRAMING (measured 2026-10-21, corrected the same day - do not "fix" this
back to the old rule):

  * FIXED packet (the client's table size is not 0): the table's size is the
    TOTAL length of the packet, the 2 cmd bytes INCLUDED. There is no length
    field on the wire.
  * VARIABLE packet (the table's size IS 0): cmd(2) + len(2) + payload, and the
    len field is the TOTAL length of the WHOLE packet - the 2 cmd bytes and the
    2 len bytes included.

The old rule said the len field was the payload length. That is 4 bytes short,
and it is exactly why the barrack stream used to look like it stopped at
offset 7632: BC_HOTKEY_BIND_INFO (id 56) was read as 7571 payload bytes
instead of 7571 total bytes, and the next packet (BC_ROTATION_CHAR_LIST) was
missed by 4.

Proof that the rule is right: all six live captures (3 barrack, 3 zone) split
into packets with ZERO bytes left over, and the zone leg alone is 4,200
packets. A wrong length rule cannot land on the last byte every time.

Output:
    <out>/<CMD_NAME>_<id>.bin    the packet bytes, exactly as on the wire
    <out>/manifest.json          name, id, offset, length, sha256
"""
import argparse
import hashlib
import json
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)

from ge_paths import DATA  # noqa: E402

LIVE_TABLE = os.path.join(ROOT, "data", "cmd_table_live_z.txt")
OLD_TABLE = os.path.join(ROOT, "data", "cmd_table.txt")


def load_table(path=None):
    """id -> (size, name)."""
    path = path or (LIVE_TABLE if os.path.exists(LIVE_TABLE) else OLD_TABLE)
    sizes, names = {}, {}
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            p = line.split()
            if len(p) >= 3 and p[0].isdigit():
                cid = int(p[0])
                sizes[cid] = int(p[1])
                names[cid] = p[2]
    return sizes, names


def split_packets(buf, sizes):
    """Walk the stream. Returns (packets, stop_reason).

    packets: list of (offset, cmd, total_length).
    """
    out = []
    i, n = 0, len(buf)
    while i + 2 <= n:
        cmd = struct.unpack_from("<H", buf, i)[0]
        sz = sizes.get(cmd)
        if sz is None:
            return out, "unknown command id %d at offset %d" % (cmd, i)
        if sz == 0:
            if i + 4 > n:
                return out, "truncated length at %d" % i
            total = struct.unpack_from("<H", buf, i + 2)[0]
            if total < 4:
                return out, "silly length %d at %d" % (total, i)
            if i + total > n:
                return out, "length %d at %d overflows the buffer" % (total, i)
            out.append((i, cmd, total))
            i += total
        else:
            if i + sz > n:
                return out, "size %d at %d overflows the buffer" % (sz, i)
            out.append((i, cmd, sz))
            i += sz
    return out, "end of buffer at %d" % i


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("capture")
    ap.add_argument("--out", default=os.path.join(DATA, "replay"),
                    help="output folder (default data\\replay)")
    ap.add_argument("--cmd", type=int, action="append",
                    help="only write this command id (repeatable)")
    ap.add_argument("--list", action="store_true",
                    help="only print the inventory, write nothing")
    args = ap.parse_args()

    sizes, names = load_table()
    with open(args.capture, "rb") as f:
        data = f.read()

    pkts, why = split_packets(data, sizes)
    print("capture : %s (%d bytes)" % (args.capture, len(data)))
    print("packets : %d   stopped: %s" % (len(pkts), why))
    left = len(data) - (pkts[-1][0] + pkts[-1][2] if pkts else 0)
    print("bytes left over: %d   %s" % (left, "(GOOD)" if left == 0 else "(BAD)"))
    for off, cmd, total in pkts:
        print("  %8d  cmd %-5d %-42s %d bytes"
              % (off, cmd, names.get(cmd, "?")[:42], total))
    if args.list:
        return 0

    os.makedirs(args.out, exist_ok=True)
    manifest = []
    used = {}
    for off, cmd, total in pkts:
        if args.cmd and cmd not in args.cmd:
            continue
        blob = data[off:off + total]
        base = "%s_%d" % (names.get(cmd, "CMD%d" % cmd), cmd)
        # A name can repeat (the live barrack sends nine rotation packets), so
        # the second one gets a _2 suffix. Two files with the same name would
        # be one file - and a capture tool that overwrites its own baseline
        # makes a comparison read as "100% identical" against itself.
        used[base] = used.get(base, 0) + 1
        name = base + ".bin" if used[base] == 1 else "%s_%d.bin" % (base, used[base])
        path = os.path.join(args.out, name)
        with open(path, "wb") as f:
            f.write(blob)
        manifest.append({
            "file": name, "cmd": cmd, "name": names.get(cmd, "?"),
            "offset": off, "length": total,
            "sha256": hashlib.sha256(blob).hexdigest(),
        })
        print("wrote %s (%d bytes)" % (path, total))
    mpath = os.path.join(args.out, "manifest.json")
    with open(mpath, "w", encoding="utf-8") as f:
        json.dump({"capture": os.path.abspath(args.capture),
                   "packets": manifest}, f, indent=2)
    print("wrote %s" % mpath)
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
