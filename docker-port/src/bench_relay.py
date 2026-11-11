"""Bench relay for the LOCAL v28 server: capture both legs at ~0 ms.

Why a relay and not pktmon: a client on 127.0.0.1 has no NIC hop, so packet
capture sees nothing (measured in this project before). A userspace relay does.

Two listeners:
  7031 -> 7030   barrack (client points here via serverlist.xml)
  7032 -> 7002   zone    (reached because the barrack's STARTGAMEOK is rewritten)

The barrack's server->client stream carries the zone address the client must
dial. `127.0.0.1:7002` becomes `127.0.0.1:7032`, which is the SAME width (both
ports are 2 bytes little-endian: 7002=0x1B5A, 7032=0x1B78), so every packet
keeps its length and nothing downstream desyncs.

Every chunk is written with a timestamp so inter-packet gaps are measurable.
"""
import datetime
import itertools
import os
import socket
import struct
import sys
import tempfile
import threading

OUT = os.path.join(tempfile.gettempdir(), "bench")
os.makedirs(OUT, exist_ok=True)

BARRACK = ("127.0.0.1", 7030)
ZONE = ("127.0.0.1", 7002)
OLD_PORT = struct.pack("<H", 7002)
NEW_PORT = struct.pack("<H", 7032)
IP_MARK = b"127.0.0.1"

_ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
_seq = itertools.count(1)
_seq_lock = threading.Lock()


class Conn:
    """One accepted connection -> its own capture file pair per direction.

    Filing every connection of a kind into one file looked fine until the
    client reconnected: the bench client dials the zone THREE times at startup
    (measured 2026-11-05), and three streams landed interleaved byte-for-byte
    in one .bin, which cannot be framed afterwards. One connection, one file.
    """

    def __init__(self, kind):
        with _seq_lock:
            self.ident = "%s%d" % (kind, next(_seq))
        self.lock = threading.Lock()
        self.files = {}

    def stamp(self, direction, n, data):
        with self.lock:
            if direction not in self.files:
                p = os.path.join(OUT, "%s_%s_%s.bin" % (_ts, self.ident, direction))
                self.files[direction] = (
                    open(p, "wb"),
                    open(p.replace(".bin", ".tsv"), "w", encoding="utf-8"))
                print("capture:", p, flush=True)
            b, t = self.files[direction]
            b.write(data)
            b.flush()
            t.write("%.6f\t%d\n" % (datetime.datetime.now().timestamp(), n))
            t.flush()

    def close(self):
        for b, t in self.files.values():
            try:
                b.close()
                t.close()
            except Exception:
                pass


def rewrite_zone_port(buf):
    """Point the client at the zone relay instead of the zone itself."""
    out = bytearray(buf)
    changed = 0
    i = out.find(IP_MARK)
    while i != -1:
        # the port sits a few bytes after the NUL-terminated IP string
        window_end = min(len(out), i + len(IP_MARK) + 12)
        j = out.find(OLD_PORT, i + len(IP_MARK), window_end)
        if j != -1:
            out[j:j + 2] = NEW_PORT
            changed += 1
        i = out.find(IP_MARK, i + 1)
    return bytes(out), changed


def pump(src, dst, conn, direction, rewrite=False):
    carry = b""
    try:
        while True:
            d = src.recv(65536)
            if not d:
                break
            if rewrite:
                d = carry + d
                carry = b""
                d, n = rewrite_zone_port(d)
                if n:
                    print("rewrote zone port -> 7032 (%d)" % n, flush=True)
            conn.stamp(direction, len(d), d)
            dst.sendall(d)
    except Exception as e:
        print("pump end:", conn.ident, direction, repr(e)[:80], flush=True)
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except Exception:
            pass


def handle(client, target, kind):
    try:
        server = socket.create_connection(target, timeout=10)
    except OSError as e:
        # The client retries a refused port several times at startup; without
        # this the thread dies with a traceback and leaves the client hanging.
        print("connect %s %s -> %s FAILED: %s"
              % (kind, client.getpeername(), target, e), flush=True)
        client.close()
        return
    conn = Conn(kind)
    print("connect %s %s -> %s" % (conn.ident, client.getpeername(), target), flush=True)
    t = threading.Thread(target=pump, args=(server, client, conn, "s2c", kind == "barrack"), daemon=True)
    t.start()
    pump(client, server, conn, "c2s")
    t.join(timeout=10)
    conn.close()
    client.close()
    server.close()


def serve(port, target, kind):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(8)
    print("listening %d -> %s:%d (%s)" % (port, target[0], target[1], kind), flush=True)
    while True:
        c, _ = s.accept()
        threading.Thread(target=handle, args=(c, target, kind), daemon=True).start()


if __name__ == "__main__":
    threading.Thread(target=serve, args=(7032, ZONE, "zone"), daemon=True).start()
    serve(7031, BARRACK, "barrack")
