"""Is our packet the same SHAPE as the live server's?

The packet owns the shape; the script owns the text. So compare two streams
with the text tokens blanked out:

    <$>10139385</>   ->   <$>T</>

Then a difference means the PACKET is wrong (field order, length, a missing
field) and no amount of script fixing helps. No difference means the packet is
right, and any remaining complaint is about the ids - i.e. the script.

Raw byte equality is the wrong test, because the ids are SUPPOSED to differ
between the two generations.

    python compare_packet.py <live.bin> --ours <ours.bin>
    python compare_packet.py <live.bin> --ours <our.log>          # the plain: lines

Framing is done by `analyze_capture.split_packets` in resync mode, because a
capture starts mid-connection: byte 0 is the middle of a packet, so a plain
walk stops at once. The tool reports how many bytes it had to skip.
"""

import argparse
import os
import re
import sys

from analyze_capture import load_table as _load_table, split_packets

TOKEN = re.compile(rb"<\$>(\d+)</>")
PLAIN = re.compile(r"plain:\s*((?:[0-9A-Fa-f]{2}[\s,]*)+)")


def read_bin(path):
    """Split a capture. A straight walk first; resync only if it cannot finish.

    The straight walk is preferred on purpose: the resync mode needs a valid
    packet AFTER the one it accepts, so it always loses the last packet. It is
    a fallback for a capture that really does start mid-packet, not the normal
    path.
    """
    sizes, _names = _load_table()
    with open(path, "rb") as fh:
        buf = fh.read()
    packets, reason = split_packets(buf, sizes)
    consumed = 0
    if packets:
        off, cmd, plen = packets[-1]
        consumed = off + plen + (4 if sizes.get(cmd) == 0 else 2)
    if consumed < len(buf) - 2:
        packets, reason = split_packets(buf, sizes, resync=True)
        reason = "resynced - " + reason
    else:
        reason = "%d of %d bytes, aligned" % (consumed, len(buf))
    out = []
    for off, cmd, plen in packets:
        total = plen + (4 if sizes.get(cmd) == 0 else 2)
        out.append((cmd, buf[off:off + total]))
    return out, reason


def read_log(path):
    packets = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = PLAIN.search(line)
            if not m:
                continue
            data = bytes(int(x, 16) for x in re.findall(r"[0-9A-Fa-f]{2}", m.group(1)))
            if len(data) >= 2:
                packets.append((int.from_bytes(data[:2], "little"), data))
    return packets, "%d plain packet(s) from the log" % len(packets)


def normalise(body):
    """Mask every locale token, so only the packet's SHAPE is left to compare."""
    return TOKEN.sub(b"<$>T</>", body)


def _hex(b, limit=56):
    return " ".join("%02X" % c for c in b[:limit]) + (" ..." if len(b) > limit else "")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("live", help="the reference stream (.bin capture or .log)")
    parser.add_argument("--ours", required=True, help="what OUR server sent")
    parser.add_argument("--show", type=int, default=3, help="how many differing packets to print")
    args = parser.parse_args(argv)

    for name, path in (("live", args.live), ("ours", args.ours)):
        if not os.path.exists(path):
            print("%s not found: %s" % (name, path))
            return 2

    def load(path):
        return read_log(path) if path.lower().endswith(".log") else read_bin(path)

    live, how_live = load(args.live)
    ours, how_ours = load(args.ours)

    print("live : %-42s %4d packets  %s" % (os.path.basename(args.live), len(live), how_live))
    print("ours : %-42s %4d packets  %s" % (os.path.basename(args.ours), len(ours), how_ours))
    if not live or not ours:
        print("\nnot enough packets to compare - the framing note above says where it stopped")
        return 1

    by_live, by_ours = {}, {}
    for cmd, body in live:
        by_live.setdefault(cmd, []).append(body)
    for cmd, body in ours:
        by_ours.setdefault(cmd, []).append(body)

    only_live = sorted(set(by_live) - set(by_ours))
    only_ours = sorted(set(by_ours) - set(by_live))
    shared = sorted(set(by_live) & set(by_ours))

    print("\ncommands only the live server sends: %s"
          % (", ".join("%d x%d" % (c, len(by_live[c])) for c in only_live[:8]) or "-"))
    print("commands only OUR server sends     : %s"
          % (", ".join("%d x%d" % (c, len(by_ours[c])) for c in only_ours[:8]) or "-"))

    same_cmds, diff_cmds = 0, 0
    details = []
    print("\nshape per command (every locale token masked to <$>T</>):")
    print("   cmd     live  ours  verdict")
    for cmd in shared:
        left, right = by_live[cmd], by_ours[cmd]
        differences = [i for i in range(min(len(left), len(right)))
                       if normalise(left[i]) != normalise(right[i])]
        if differences:
            diff_cmds += 1
            verdict = "DIFFERS"
            details.append((cmd, left, right, differences))
        else:
            same_cmds += 1
            verdict = "same shape"
        print("   %-6d  %3d   %3d   %s" % (cmd, len(left), len(right), verdict))

    for cmd, left, right, differences in details[:args.show]:
        i = differences[0]
        print("\n  cmd %d, packet #%d of that command:" % (cmd, i + 1))
        print("     live: %s" % _hex(normalise(left[i])))
        print("     ours: %s" % _hex(normalise(right[i])))

    print("\n%d command(s) same shape, %d differ" % (same_cmds, diff_cmds))
    if diff_cmds:
        print("A shape difference is a PACKET fault - fixing the script cannot help.")
        return 1
    print("Every shared command has the same shape, so the packet is right. Any")
    print("remaining complaint is about the ids: check them with check_script_ids.py.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
