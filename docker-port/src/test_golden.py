"""test_golden.py - the safety net for refactoring the zone server.

Drives the zone through a scripted session with a fake client and records
EVERY byte the server sends, split into packets, with the volatile fields
masked. Run it before a refactor to make a golden file, run it after, and the
two must be identical - that is what proves a refactor changed nothing on the
wire.

The game client crashes on a wrong byte, and a crash is indistinguishable
from a refactor bug, so "it still runs" is not good enough. "It sends exactly
the same bytes" is.

Usage:
    python test_golden.py <port> <outfile>     # record
    python test_golden.py <port> <outfile> --check   # compare

Needs zone_server.py running. The packet split uses the CLIENT's own packet
table (data\\cmd_table.txt): a fixed-size packet has its size there, a
variable-size one (size 0) carries its length at +2.
"""
import hashlib
import os
import socket
import struct
import sys
import time

import ge_crypt as gc

HOST = "127.0.0.1"
CMD_CZ_START_GAME = 3590
CMD_CZ_GAME_READY = 3592
CMD_CZ_MOVE_PC = 3598
CMD_CZ_CLICK_TRIGGER = 3679
CMD_CZ_PING = 3688

# The client's own packet table: id -> full wire size (0 = variable).
CMD_TABLE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                         "data", "cmd_table.txt")

# Fields that legitimately differ between two runs.
VOLATILE = {
    3591: [(28, 8),     # ZC_START_GAMEOK: serverTime + reserved
           (452, 12)],  # and the leader's SPAWN POSITION (x, y, cell)
    3548: [(6, 4)],      # ZC_MOVE_PC: tick
}
# 3591's record 0 starts at +36 and is 485 bytes; its initialPos is at +416
# inside the record, so +452 in the packet. The position is where the
# character last stood, so it changes every time the player walks - and a
# golden file that fails whenever the player moves is a golden file people
# learn to ignore. The POINT of this file is that a REFACTOR changed
# nothing on the wire, and where the character happens to be standing is
# not the code's doing.
# (A level-up still changes the record: the stance and skill lists grow, so
# the packet gets LONGER. Regenerate the golden then, and say so.)


def load_table():
    sizes = {}
    with open(CMD_TABLE, encoding="utf-8", errors="replace") as f:
        for line in f:
            p = line.split()
            if len(p) >= 3 and p[0].isdigit():
                sizes[int(p[0])] = int(p[1])
    return sizes


def sec(cmd, extra=b""):
    """PKS_SECURITY_DEFAULT: command(2) + seq(4) + checksum(4)."""
    return struct.pack("<HII", cmd, 0, 0) + extra


def send(sock, body):
    sock.sendall(gc.encrypt_packet(body))


def split_packets(buf, sizes):
    """Split the plaintext stream into packets using the client's table."""
    out = []
    i = 0
    while i + 2 <= len(buf):
        cmd, = struct.unpack_from("<H", buf, i)
        size = sizes.get(cmd)
        if size is None:
            out.append((cmd, buf[i:]))
            break
        if size == 0:
            if i + 4 > len(buf):
                break
            total, = struct.unpack_from("<H", buf, i + 2)
            if total < 4 or i + total > len(buf):
                break
            out.append((cmd, buf[i:i + total]))
            i += total
        else:
            if i + size > len(buf):
                break
            out.append((cmd, buf[i:i + size]))
            i += size
    return out


def masked(cmd, data):
    b = bytearray(data)
    for off, ln in VOLATILE.get(cmd, []):
        for k in range(off, min(off + ln, len(b))):
            b[k] = 0
    return bytes(b)


def record(port, outfile):
    sizes = load_table()
    sock = socket.create_connection((HOST, port))
    sock.settimeout(1.0)
    got = b""

    def drain(idle=1.5, cap=90.0):
        """Read until the server goes quiet for `idle` seconds.

        Timing-independent on purpose: the point is WHAT the server sends, not
        when. The cap covers a server that is still busy with an earlier
        connection - the zone handles one connection at a time today, so a
        second test run has to wait for the first to end.
        """
        nonlocal got
        start = time.time()
        last = start
        sock.settimeout(0.2)
        while time.time() - start < cap:
            try:
                d = sock.recv(65536)
            except socket.timeout:
                if got and time.time() - last > idle:
                    return
                continue
            except OSError:
                return
            if not d:
                return
            got += d
            last = time.time()

    # 1. enter the world
    send(sock, sec(CMD_CZ_START_GAME) + struct.pack("<II", 1, 1) + b"Arcanine\0")
    drain()

    # 2. the world-entry handshake
    send(sock, sec(CMD_CZ_GAME_READY))
    drain()

    # 3. a walk
    send(sock, sec(CMD_CZ_MOVE_PC) + struct.pack("<Biii", 0, 6500, 400, 149))
    drain()

    # 4. a ping, so the stream has a known tail
    send(sock, sec(CMD_CZ_PING))
    drain(idle=1.0)

    try:
        sock.close()
    except OSError:
        pass

    lines = []
    for cmd, data in split_packets(got, sizes):
        # These packets depend on WHO ELSE is in the zone, so they are not part
        # of the single-player golden stream: the spawn/leave of other players,
        # and the move/stop traffic of other players. Skip them, or the test
        # fails whenever somebody else is playing.
        if cmd in (3520, 3525, 3548, 3549):
            continue
        m = masked(cmd, data)
        lines.append("%-6d %-6d %s" % (cmd, len(data),
                                        hashlib.sha1(m).hexdigest()[:16]))
    text = "\n".join(lines) + "\n"
    with open(outfile, "w", encoding="utf-8") as f:
        f.write(text)
    print("recorded %d packets to %s" % (len(lines), outfile))
    for l in lines:
        print("   " + l)
    return 0


def check(port, outfile):
    sizes = load_table()
    import tempfile
    tmp = tempfile.mktemp(suffix=".txt")
    record(port, tmp)
    with open(tmp, encoding="utf-8") as f:
        now = f.read()
    os.remove(tmp)
    if not os.path.exists(outfile):
        print("NO GOLDEN FILE at %s - record one first" % outfile)
        return 1
    with open(outfile, encoding="utf-8") as f:
        was = f.read()
    if was == now:
        print("GOLDEN OK - the server sends exactly the same bytes")
        return 0
    print("GOLDEN MISMATCH - the server's output changed!")
    a = was.splitlines()
    b = now.splitlines()
    for i in range(max(len(a), len(b))):
        x = a[i] if i < len(a) else "-"
        y = b[i] if i < len(b) else "-"
        if x != y:
            print("   line %d:\n     was: %s\n     now: %s" % (i + 1, x, y))
    return 1


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 7002
    outfile = sys.argv[2] if len(sys.argv) > 2 else "zone_golden.txt"
    if "--check" in sys.argv:
        return check(port, outfile)
    return record(port, outfile)


if __name__ == "__main__":
    sys.exit(main())
