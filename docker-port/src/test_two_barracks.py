"""test_two_barracks.py - prove the barrack serves two players at once.

The barrack used to handle one connection to completion, so the second login
waited for the first to leave. The zone was made concurrent first; this is the
same check for the barrack.

Two fake clients log in at the same time and must BOTH get their barrack
list, quickly.

Run it while ge_barrack_server.py is up:
    python test_two_barracks.py [port] [login_a] [login_b]
"""
import socket
import struct
import sys
import time

import ge_crypt as gc

HOST = "127.0.0.1"
CMD_CB_LOGIN = 4
CMD_CB_STARTBARRACK = 9
CMD_CB_REQ_BARRACK_INFO = 15

ACC_NAME_LEN = 218
PWD_LEN = 17
CMD_BC_LOGINOK = 8
CMD_BC_BARRACK_INFO = 16
CMD_BC_JOB_LIST = 66

FAILS = []


def check(cond, what):
    print(("  ok   " if cond else "  FAIL ") + what)
    if not cond:
        FAILS.append(what)


def login_packet(name):
    """CB_LOGIN: a 12-byte security header then the identity block."""
    p = struct.pack("<HIIH", CMD_CB_LOGIN, 0, 0, 0)
    p += name.encode("latin-1")[:ACC_NAME_LEN - 1].ljust(ACC_NAME_LEN, b"\0")
    p += b"\0" * PWD_LEN
    p += struct.pack("<HI", 1, 101)      # server id, version
    p += b"DEU\0\0\0\0\0"
    return p


def sec(cmd, extra=b""):
    """PKS_SECURITY_DEFAULT: command(2) + seq(4) + checksum(4)."""
    return struct.pack("<HII", cmd, 0, 0) + extra


class Client:
    def __init__(self, name):
        self.name = name
        self.data = b""
        self.sock = None

    def connect(self, port):
        self.sock = socket.create_connection((HOST, port))
        self.sock.settimeout(0.2)

    def send(self, body):
        self.sock.sendall(gc.encrypt_packet(body))

    def pump(self, seconds=1.5):
        end = time.time() + seconds
        while time.time() < end:
            try:
                d = self.sock.recv(65536)
            except socket.timeout:
                continue
            except OSError:
                break
            if not d:
                break
            self.data += d

    def has_cmd(self, cmd):
        needle = struct.pack("<H", cmd)
        return needle in self.data

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 7001
    name_a = sys.argv[2] if len(sys.argv) > 2 else "arcanine"
    name_b = sys.argv[3] if len(sys.argv) > 3 else "friend"

    print("two players in the barrack on port %d (%s and %s)"
          % (port, name_a, name_b))
    a = Client(name_a)
    b = Client(name_b)
    a.connect(port)
    b.connect(port)

    t0 = time.time()
    a.send(login_packet(name_a))
    b.send(login_packet(name_b))
    a.pump(2.0)
    b.pump(2.0)
    print("  after %.1f s: A loginok=%s, B loginok=%s"
          % (time.time() - t0, a.has_cmd(CMD_BC_LOGINOK),
             b.has_cmd(CMD_BC_LOGINOK)))
    check(a.has_cmd(CMD_BC_LOGINOK), "player A got LOGINOK")
    check(b.has_cmd(CMD_BC_LOGINOK), "player B got LOGINOK")

    a.send(sec(CMD_CB_STARTBARRACK))
    b.send(sec(CMD_CB_STARTBARRACK))
    a.pump(2.0)
    b.pump(2.0)
    check(a.has_cmd(66), "player A got the job list")
    check(b.has_cmd(66), "player B got the job list")

    a.send(sec(CMD_CB_REQ_BARRACK_INFO))
    b.send(sec(CMD_CB_REQ_BARRACK_INFO))
    waited = time.time() - t0
    a.pump(3.0)
    b.pump(3.0)
    print("  after %.1f s: A barrack=%s, B barrack=%s"
          % (waited, a.has_cmd(CMD_BC_BARRACK_INFO),
             b.has_cmd(CMD_BC_BARRACK_INFO)))
    check(a.has_cmd(CMD_BC_BARRACK_INFO), "player A got BARRACK_INFO")
    check(b.has_cmd(CMD_BC_BARRACK_INFO),
          "player B got BARRACK_INFO (it did NOT wait for A to leave)")
    check(waited < 30.0,
          "both were served quickly (%.1f s) - the barrack is not serial" % waited)

    a.close()
    b.close()
    time.sleep(0.5)
    print()
    if FAILS:
        print("FAILED %d check(s):" % len(FAILS))
        for f in FAILS:
            print("   - " + f)
        return 1
    print("two players served in the barrack at the same time")
    return 0


if __name__ == "__main__":
    sys.exit(main())
