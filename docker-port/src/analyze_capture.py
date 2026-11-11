"""analyze_capture - read a capture and describe what the server sent.

Usage:
    python analyze_capture.py <capture.bin> [more.bin ...]
    python analyze_capture.py <path>

Two tables are used, and WHICH ONE matters:

  * `data\\cmd_table_live_z.txt` - the FULL table dumped from the RUNNING
    client (`src\\dump_cmds.py`). It has every command id: the barrack ids
    (BC_/CB_ 2..71) AND the zone ids (CZ_/ZC_ 3000..3900). This is the one
    to use for a modern capture.
  * `data\\cmd_table.txt` - an OLDER, PARTIAL dump that covers only part of
    the id range. It cannot split a login leg (the BC_* ids are missing from
    it), which is why `scan_for_address` exists.

FRAMING (measured 2026-10-21, corrected the same day - do not assume):

The client's table gives, for every command id, the size the client ALLOCATES
for it. That is NOT necessarily the size on the wire. Two things are true at
once, and mixing them up is what stops a capture from splitting:

  * a FIXED-size packet (the table's size is not 0) is `cmd(2)` + payload, and
    the table's size is the TOTAL length INCLUDING the 2-byte cmd. There is
    NO length field.
  * a VARIABLE-size packet (the table's size IS 0) is `cmd(2)` + `len(2)` +
    payload, where the len field is the TOTAL length of the WHOLE packet -
    the 2 cmd bytes and the 2 len bytes INCLUDED.

The second rule was wrong at first (it said the len field was the payload
length). That is 4 bytes short, and it is why the barrack stream used to look
like it stopped at offset 7632: `BC_HOTKEY_BIND_INFO` (id 56, size 0) was
read as 7571 payload bytes instead of 7571 total bytes, and the next packet
(`BC_ROTATION_CHAR_LIST`) was missed by 4.

Proof that the rule is right: all six live captures (3 barrack, 3 zone) split
into packets with ZERO bytes left over, and the zone leg alone is 4,200
packets. A wrong length rule cannot land on the last byte every time.

So the table's size column is the size the client ALLOCATES, not the size on
the wire. That is why the two can disagree, and why "the packet is longer
than the table says" is a normal observation rather than a contradiction.
"""
import collections
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# The full table, dumped from the running client. Falls back to the old
# partial dump if the live one is not there yet.
LIVE_TABLE = os.path.join(ROOT, "data", "cmd_table_live_z.txt")
OLD_TABLE = os.path.join(ROOT, "data", "cmd_table.txt")

CMD_ZC_SPAWN_STANDINGPC = 3520
CMD_ZC_LEAVE_PC = 3525
CMD_ZC_MOVE_PC = 3548
CMD_ZC_STOP_PC = 3549
CMD_BC_STARTGAMEOK = 25


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


def _packet_at(buf, i, sizes):
    """(cmd, total_length) if a packet starts at offset i, else None."""
    n = len(buf)
    if i + 2 > n:
        return None
    cmd = struct.unpack_from("<H", buf, i)[0]
    sz = sizes.get(cmd)
    if sz is None:
        return None
    if sz == 0:
        if i + 4 > n:
            return None
        total = struct.unpack_from("<H", buf, i + 2)[0]
        if total < 4 or i + total > n:
            return None
        return cmd, total
    if i + sz > n:
        return None
    return cmd, sz


def split_packets(buf, sizes, resync=False):
    """Walk the stream. Returns (packets, stop_reason).

    Fixed packets: the table's size is the TOTAL length, cmd included.
    Variable packets (size 0): cmd(2) + len(2) + payload, where len is the
    TOTAL length of the whole packet - the cmd bytes and the len bytes
    INCLUDED.

    `resync=True` is for a stream that is NOT packet-aligned: a pktmon capture
    starts mid-connection, so byte 0 is the middle of a packet and the walk
    above stops immediately. In that mode the walk advances ONE byte at a time
    and only accepts a position where a packet, and the packet after it, are
    both structurally valid - so random data cannot fake a packet boundary.
    The reason string then reports how many bytes were skipped.
    """
    if resync:
        out = []
        i, n = 0, len(buf)
        skipped = 0
        while i + 2 <= n:
            first = _packet_at(buf, i, sizes)
            ok = False
            if first:
                second = _packet_at(buf, i + first[1], sizes)
                ok = second is not None
            if ok:
                cmd, total = first
                plen = (total - 4) if sizes[cmd] == 0 else (total - 2)
                out.append((i, cmd, plen))
                i += total
            else:
                i += 1
                skipped += 1
        return out, "resync: %d packet(s), skipped %d of %d bytes" % (len(out), skipped, n)

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
            out.append((i, cmd, total - 4))
            i += total
        else:
            if i + sz > n:
                return out, "size %d at %d overflows the buffer" % (sz, i)
            out.append((i, cmd, sz - 2))
            i += sz
    return out, "end of buffer at %d" % i


def dump_spawn(p):
    """Print the fields of a ZC_SPAWN_STANDINGPC payload."""
    if len(p) < 0x100:
        print("    (spawn packet too short: %d bytes)" % len(p))
        return
    print("    spawn packet, %d bytes" % len(p))
    for off, fmt, name in ((4, "17s", "name"), (0x15, "<H", "posture"),
                           (0x145, "<i", "nameplate id")):
        try:
            v = struct.unpack_from(fmt, p, off)[0]
        except struct.error:
            v = "?"
        if isinstance(v, bytes):
            v = v.split(b"\x00")[0].decode("latin-1", "replace")
        print("      +%-5d %-14s %s" % (off, name, v))


def dump_startgameok(p):
    """Print the fields of a BC_STARTGAMEOK payload."""
    print("    startgameok, %d bytes" % len(p))
    for off, fmt, name in ((0, "<H", "cmd"), (2, "<H", "len"),
                            (4, "<I", "zone instance")):
        try:
            v = struct.unpack_from(fmt, p, off)[0]
        except struct.error:
            v = "?"
        print("      +%-5d %-14s %s" % (off, name, v))


def scan_for_address(data, ips=("15.235.233.176", "127.0.0.1", "167.86.115.184",
                                "85.11.190.15")):
    """Find the zone address in a raw stream, with no packet table.

    The packet that names the zone is easy to find by its address string:
    4 bytes before it is the command, 4 bytes after the 16-byte address field
    is the port.
    """
    found = 0
    for ip in ips:
        needle = ip.encode("ascii")
        i = 0
        while True:
            j = data.find(needle, i)
            if j < 0:
                break
            i = j + 1
            found += 1
            # The address field is at packet+6 (cmd 2 + zone instance 4), so
            # the command is 6 bytes before it and the zone instance 2.
            cmd = struct.unpack_from("<H", data, j - 6)[0] if j >= 6 else None
            inst = struct.unpack_from("<I", data, j - 4)[0] if j >= 4 else None
            port = struct.unpack_from("<I", data, j + 16)[0] if j + 20 <= len(data) else None
            print("    at file offset %d: command=%s zone instance=%s address=%r port=%s"
                  % (j, cmd, inst, ip, port))
            if ip.startswith("127.0.0.1"):
                print("      -> the relay's rewrite worked: the client will dial us")
    if not found:
        print("    (no zone address in this stream)")


def summarise(data, sizes, names, path):
    """Print the command inventory of one capture."""
    print("=" * 70)
    print("%s  (%d bytes)" % (path, len(data)))
    pkts, why = split_packets(data, sizes)
    first = struct.unpack_from("<H", data, 0)[0] if len(data) >= 2 else None
    if first is None or first not in sizes:
        print("  (the first command %s is not in the client's table - this "
              "is a login leg; scanning it raw)" % first)
        scan_for_address(data)
        return
    counts = collections.Counter(c for _, c, _ in pkts)
    print("  packets: %d   stopped: %s" % (len(pkts), why))
    print("%-6s %-7s %-6s %-40s %s" % ("cmd", "hex", "count", "name", "sizes"))
    for cmd, cnt in sorted(counts.items()):
        szs = sorted(set(s for _, c2, s in pkts if c2 == cmd))
        print("%-6d %-7s %-6d %-40s %s"
              % (cmd, hex(cmd), cnt, names.get(cmd, "?")[:40],
                 ",".join(str(x) for x in szs[:4])))
    interesting = {CMD_ZC_SPAWN_STANDINGPC: "ZC_SPAWN_STANDINGPC",
                    CMD_ZC_LEAVE_PC: "ZC_LEAVE_PC",
                    CMD_BC_STARTGAMEOK: "BC_STARTGAMEOK"}
    for cmd, name in interesting.items():
        for off, c, ln in pkts:
            if c != cmd:
                continue
            payload = data[off + 2:off + 2 + ln]
            print("  --- %s ---" % name)
            if cmd == CMD_ZC_SPAWN_STANDINGPC:
                dump_spawn(payload)
            else:
                dump_startgameok(payload)
            break


def main():
    paths = sys.argv[1:]
    if not paths:
        print(__doc__)
        return 1
    sizes, names = load_table()
    for path in paths:
        with open(path, "rb") as f:
            data = f.read()
        summarise(data, sizes, names, path)
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
