"""analyze_live_click - read a live capture and print the NPC-click exchange.

Usage:
    python analyze_live_click.py <capture-dir> [--port 7061] [--t0]

Reads the zone leg of a capture (the files `pcap_extract` wrote) and prints, in
TIME ORDER and with the delay from the first packet, everything that happened
around a click on an NPC:

  * the client's CZ_CLICK_TRIGGER (and every other client packet, so you can
    see what the client did NOT send - the live client sends NO position
    reports for a click, and that is the fact that decides who owns the walk),
  * the server's ZC_MOVE_PC with its `reason` field decoded,
  * the dialog packets and the click voice.

The `reason` field is the whole point: GE's `enum MOVE_REASON { MR_MOVE,
MR_ATTACK, MR_SKILL, MR_PICK, MR_DIALOG }` - so `reason=4` means "this walk is
a walk to a clicked NPC" and the live server sends exactly that.

The capture dir is the one `capture_live.py stop` writes. Its zone leg is
found by the port, and the port is NOT always 7054: the live server runs one
zone process per map and each map has its own port, so take it from the
`BC_STARTGAMEOK` in the barrack leg, or pass --port.
"""
import os
import struct
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ge_crypt

MOVE_REASON = {0: "MR_MOVE", 1: "MR_ATTACK", 2: "MR_SKILL", 3: "MR_PICK",
               4: "MR_DIALOG"}


def load_table():
    """The client's own command table: id -> wire size (0 = variable)."""
    path = os.path.join(os.path.dirname(os.path.dirname(
        os.path.abspath(__file__))), "data", "cmd_table_live_z.txt")
    sizes, names = {}, {}
    for line in open(path, encoding="utf-8", errors="replace"):
        q = line.split()
        if len(q) >= 3 and q[0].isdigit():
            sizes[int(q[0])] = int(q[1])
            names[int(q[0])] = q[2]
    return sizes, names


def zone_port(capture_dir):
    """The zone port this capture used, read out of the barrack's STARTGAMEOK."""
    for f in sorted(os.listdir(capture_dir)):
        if not f.startswith("s2c_7001_"):
            continue
        d = open(os.path.join(capture_dir, f), "rb").read()
        # BC_STARTGAMEOK (cmd 25): command(2) len(2) zoneInstance(4) ip(16)
        # port(4). The IP is a NUL-terminated string, so scan for the port that
        # follows it.
        i = d.find(b"\x19\x00")
        while i >= 0:
            blob = d[i:i + 64]
            if len(blob) >= 28:
                ip = blob[8:24].split(b"\x00")[0]
                if ip.count(b".") == 3:
                    port = struct.unpack_from("<I", blob, 24)[0]
                    if 1024 < port < 65536:
                        return port
            i = d.find(b"\x19\x00", i + 1)
    return None


def split_s2c(path, sizes):
    """Split the server->client stream into packets."""
    d = open(path, "rb").read()
    out = []
    p = 0
    while p + 2 <= len(d):
        cmd = struct.unpack_from("<H", d, p)[0]
        sz = sizes.get(cmd)
        if sz is None:
            break
        total = struct.unpack_from("<H", d, p + 2)[0] if sz == 0 else sz
        out.append((cmd, d[p:p + total]))
        p += total
    return out


def split_c2s(path):
    """Split the client->server stream into decrypted packets."""
    d = open(path, "rb").read()
    out = []
    p = 0
    while p + 2 <= len(d):
        blk = struct.unpack_from("<H", d, p)[0]
        if blk == 0 or p + 2 + blk > len(d):
            break
        plain = ge_crypt.decrypt_packet(d[p:p + 2 + blk])
        cmd = struct.unpack_from("<H", plain, 0)[0] if len(plain) >= 2 else -1
        out.append((cmd, plain))
        p += 2 + blk
    return out


def describe_c2s(cmd, plain, names):
    if cmd == 3679:                     # CZ_CLICK_TRIGGER
        return ("CLICK handle=0x%08x long=%d"
                % (struct.unpack_from("<I", plain, 12)[0], plain[11]))
    if cmd == 3598:                     # CZ_MOVE_PC
        x, y, cell = struct.unpack_from("<iii", plain, 11)
        return "MOVE_PC pos=(%d,%d,%d)" % (x, y, cell)
    if cmd == 3657:
        return "DIALOG_SELECT choice=%d" % plain[11]
    if cmd == 3656:
        return "ACK_DIALOG next=%d" % plain[11]
    if cmd == 3688:
        return None                     # ping: noise, but COUNT it
    return "cmd=%d %s" % (cmd, names.get(cmd, "?"))


def describe_s2c(cmd, raw, names):
    if cmd == 3548 and len(raw) >= 30:  # ZC_MOVE_PC
        h = struct.unpack_from("<I", raw, 2)[0]
        x, y, z = struct.unpack_from("<iii", raw, 10)
        spd = struct.unpack_from("<f", raw, 22)[0]
        reason = struct.unpack_from("<i", raw, 26)[0]
        return ("MOVE h=%d pos=(%d,%d,%d) spd=%.0f reason=%d (%s)"
                % (h, x, y, z, spd, reason,
                   MOVE_REASON.get(reason, "?")))
    if cmd == 3515 and len(raw) >= 6:  # ZC_PLAY_NPC_CLICK_VOICE
        return ("NPC_CLICK_VOICE handle=0x%08x"
                % struct.unpack_from("<I", raw, 2)[0])
    if cmd in (3660, 3663, 3680, 3665, 3543, 3515):
        return "%s len=%d" % (names.get(cmd, "cmd%d" % cmd), len(raw))
    return None


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    if not args:
        print(__doc__)
        return 1
    cap = args[0]
    port = None
    for i, a in enumerate(sys.argv):
        if a == "--port":
            port = int(sys.argv[i + 1])
    sizes, names = load_table()
    if port is None:
        port = zone_port(cap)
        if port:
            print("zone port from the barrack's STARTGAMEOK: %d" % port)
    if port is None:
        print("could not work out the zone port - pass --port")
        return 1

    c2s_file = None
    s2c_file = None
    for f in os.listdir(cap):
        if f.startswith("c2s_") and f.endswith("_%d.bin" % port):
            c2s_file = os.path.join(cap, f)
        if f.startswith("s2c_%d_" % port) and f.endswith(".bin"):
            s2c_file = os.path.join(cap, f)
    if not c2s_file or not s2c_file:
        print("no zone leg for port %d in %s" % (port, cap))
        return 1

    print("=== client -> server")
    pings = 0
    for cmd, plain in split_c2s(c2s_file):
        desc = describe_c2s(cmd, plain, names)
        if desc is None:
            pings += 1
            continue
        print("  %s" % desc)
    print("  (%d ping packets, not listed)" % pings)

    print()
    print("=== server -> client")
    for cmd, raw in split_s2c(s2c_file, sizes):
        desc = describe_s2c(cmd, raw, names)
        if desc:
            print("  %s" % desc)
    return 0


if __name__ == "__main__":
    sys.exit(main())
