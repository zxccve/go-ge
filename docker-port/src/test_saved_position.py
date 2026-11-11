"""Does a chase to a clicked NPC still poison the SAVED position?

The bug (2026-10-21): the player stood still, clicked a distant NPC, and left
the zone without arriving. The server had chased to the 140-unit ring and had
stored THAT as the player's position, so the next login resumed the character
in front of the NPC.

This drives the real zone server: it logs in, stands still, clicks a distant
NPC, and then reads the position the zone would save.
"""
import os
import socket
import struct
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ge_crypt as gc
import ge_db

HOST, PORT = "127.0.0.1", 7002
CMD_CZ_START_GAME = 3590
CMD_CZ_MOVE_PC = 3598
CMD_CZ_CLICK_TRIGGER = 3679


def sec(cmd, extra=b""):
    return struct.pack("<HII", cmd, 0, 0) + extra


def send(sock, body):
    sock.sendall(gc.encrypt_packet(body))


def drain(sock, seconds=1.0):
    end = time.time() + seconds
    out = b""
    sock.settimeout(0.2)
    while True:
        left = end - time.time()
        if left <= 0:
            break
        try:
            sock.settimeout(left)
            d = sock.recv(65536)
        except socket.timeout:
            break
        if not d:
            break
        out += d
    return out


def main():
    cid = 34
    before = ge_db.get_character_position(cid)
    print("saved position BEFORE : %s" % (before,))

    sock = socket.create_connection((HOST, PORT))
    send(sock, sec(CMD_CZ_START_GAME) + struct.pack("<II", 1, 1) + b"Arcanine\0")
    time.sleep(4.0)
    drain(sock, 1.0)

    # Stand still at a known spot, then click a NPC that is FAR away.
    here = (6000, 400, 149)
    send(sock, sec(CMD_CZ_MOVE_PC) + struct.pack("<Biii", 0, here[0], here[1], here[2]))
    time.sleep(2.0)
    drain(sock, 0.5)

    # NPC #49 is the wagon Leonardo at (3224, -1574) - about 3.2k units away.
    handle = 0x40000000 + 49
    send(sock, sec(CMD_CZ_CLICK_TRIGGER) + struct.pack("<BBI", 0, 0, handle))
    time.sleep(1.5)
    drain(sock, 1.0)
    print("clicked a distant NPC (handle 0x%08x) and did NOT walk there" % handle)

    # Now leave the zone, which is what writes the position to the database.
    sock.close()
    time.sleep(2.0)

    after = ge_db.get_character_position(cid)
    print("saved position AFTER  : %s" % (after,))
    ring = (3342, -1498, 330)
    print("the chase ring point  : %s  <- the position that must NOT be saved" % (ring,))
    if after and (abs(after[0] - ring[0]) < 30 and abs(after[1] - ring[1]) < 30):
        print("FAIL: the saved position is the chase ring - the bug is still there")
        return 1
    print("OK: the saved position is not the chase destination")
    return 0


if __name__ == "__main__":
    sys.exit(main())
