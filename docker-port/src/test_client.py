"""Synthetic client: exercises ge_barrack_server end-to-end without the game."""
import os
import socket
import struct
import time

import ge_crypt as gc
import ge_barrack_server as S

HOST = "127.0.0.1"
PORT = int(os.environ.get("BARRACK_PORT", "7001"))


def sec(cmd, extra=b""):
    """PKS_SECURITY_DEFAULT/ VARSIZE body: command, seq, checksum, [len], extra."""
    return struct.pack("<HII", cmd, 0, 0) + extra


def build_login(ident="Arcanine", pw="123456"):
    body = b""
    body += S.cstr(ident, S.ACC_NAME_LEN)
    body += gc_hash(pw).ljust(S.PWD_LEN, b"\0")
    body += struct.pack("<H", 0)          # serverID
    body += struct.pack("<I", 101)        # version
    body += S.cstr("DEU", 8)              # nation
    body += struct.pack("<B", 1)          # isFirstlogin
    body += struct.pack("<H", 0)          # channelType
    body += struct.pack("<Id", 0, 0.0)    # restriction
    body += b"\0" * 10                    # birthday
    n = 12 + len(body)
    return struct.pack("<HIIH", 4, 0, 0, n) + body


def gc_hash(pw):
    import hashlib
    return hashlib.md5(pw.encode()).digest()


# The split uses the CLIENT's own packet table, not a hand-written list. The old
# hardcoded sizes were wrong (LOGINOK was taken as 250 bytes instead of 57), and
# a wrong size silently swallows the NEXT packet - which is how the
# script-binding table (cmd 56) went missing from this test's output.
#
# FRAMING (measured 2026-10-21):
#   * fixed packet (table size != 0): the table's size is the TOTAL length, cmd
#     included. No length field.
#   * variable packet (table size == 0): cmd(2) + len(2) + payload, and len is
#     the TOTAL length of the whole packet - cmd and len bytes INCLUDED.
_TABLE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                      "data", "cmd_table_live_z.txt")


def load_table():
    sizes = {}
    with open(_TABLE, encoding="utf-8", errors="replace") as f:
        for line in f:
            p = line.split()
            if len(p) >= 3 and p[0].isdigit():
                sizes[int(p[0])] = int(p[1])
    return sizes


SIZES = load_table()


def read_replies(sock, timeout=3.0):
    sock.settimeout(timeout)
    buf = b""
    got = []
    while True:
        try:
            d = sock.recv(8192)
        except socket.timeout:
            break
        if not d:
            break
        buf += d
        while len(buf) >= 2:
            cmd, = struct.unpack_from("<H", buf, 0)
            size = SIZES.get(cmd)
            if size is None:
                print("   (unknown cmd %d at offset -%d, stopping)" % (cmd, len(buf)))
                return got, buf
            if size == 0:
                if len(buf) < 4:
                    break
                ln, = struct.unpack_from("<H", buf, 2)
                if ln < 4 or ln > 65535 or len(buf) < ln:
                    break
                pkt, buf = buf[:ln], buf[ln:]
            else:
                if len(buf) < size:
                    break
                pkt, buf = buf[:size], buf[size:]
            got.append((cmd, len(pkt)))
    return got, buf


def main():
    s = socket.create_connection((HOST, PORT))
    print("connected")
    s.sendall(gc.encrypt_packet(build_login()))
    got, _ = read_replies(s)
    print("after LOGIN ->", got)

    s.sendall(gc.encrypt_packet(sec(9) + struct.pack("<B", 0)))   # CB_STARTBARRACK
    got, _ = read_replies(s)
    print("after STARTBARRACK ->", got)

    s.sendall(gc.encrypt_packet(sec(15)))                          # CB_REQ_BARRACK_INFO
    got, _ = read_replies(s)
    print("after REQ_BARRACK_INFO ->", got)

    s.sendall(gc.encrypt_packet(sec(22) + struct.pack("<H", 0) + struct.pack("<HHH", 0, 255, 255)))
    got, _ = read_replies(s)
    print("after STARTGAME ->", got)

    time.sleep(0.3)
    s.close()


if __name__ == "__main__":
    main()
