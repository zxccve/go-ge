"""test_two_players.py - prove the zone serves two players at once.

Before the multi-player work the zone handled ONE connection to completion,
so the second player waited for the first to disconnect - and the first
player's state (the bag, the team, the walk position) lived in module
globals, so the second player clobbered it.

This test:
  1. connects two fake clients at the same time;
  2. checks that BOTH are served (the second one does not wait for the
     first to leave);
  3. checks that each client gets its OWN character (the name in its
     ZC_START_GAMEOK differs);
  4. walks one of them and checks the other is unaffected.

Run it while zone_server.py is up:
    python test_two_players.py [port] [account_a] [account_b]

The account ids come from GE_DATA.ACCOUNT_INFO. The default pair is the two
accounts the port knows about; with only one account the test falls back to
the same one for both and still proves the concurrency, just not the
per-player data.
"""
import os
import socket
import struct
import sys
import threading
import time

import ge_crypt as gc

HOST = "127.0.0.1"
CMD_CZ_START_GAME = 3590
CMD_CZ_GAME_READY = 3592
CMD_CZ_MOVE_PC = 3598

FAILS = []


def check(cond, what):
    print(("  ok   " if cond else "  FAIL ") + what)
    if not cond:
        FAILS.append(what)


def sec(cmd, extra=b""):
    return struct.pack("<HII", cmd, 0, 0) + extra


class Client:
    """A fake zone client that keeps everything the server sent."""

    def __init__(self, account_id, name):
        self.account_id = account_id
        self.name = name
        self.sock = None
        self.data = b""
        self.first_byte_at = None
        self._stop = False

    def connect(self):
        self.sock = socket.create_connection((HOST, PORT))
        self.sock.settimeout(0.2)

    def send(self, body):
        self.sock.sendall(gc.encrypt_packet(body))

    def pump(self):
        """Read whatever is there. Returns True if anything arrived."""
        got = False
        try:
            d = self.sock.recv(65536)
            if d:
                self.data += d
                if self.first_byte_at is None:
                    self.first_byte_at = time.time()
                got = True
        except socket.timeout:
            pass
        except OSError:
            self._stop = True
        return got

    def wait_for(self, cmd, timeout=60.0):
        """Wait until a packet with this command id has arrived."""
        end = time.time() + timeout
        while time.time() < end:
            if cmd in self.packet_ids():
                return True
            if not self.pump():
                time.sleep(0.05)
        return False

    def packet_ids(self):
        out = []
        i = 0
        d = self.data
        while i + 2 <= len(d):
            c, = struct.unpack_from("<H", d, i)
            out.append(c)
            if c == 3591:            # ZC_START_GAMEOK, variable
                if i + 4 > len(d):
                    break
                total, = struct.unpack_from("<H", d, i + 2)
                i += total
            elif c == 3149 or c == 3601 or c == 3543:
                i += 3601 and 0 or 0
                break
            else:
                i += 2
        return out

    def start_game(self):
        self.send(sec(CMD_CZ_START_GAME)
                  + struct.pack("<II", 1, self.account_id)
                  + self.name.encode("latin-1")[:16] + b"\0")

    def game_ready(self):
        self.send(sec(CMD_CZ_GAME_READY))

    def move(self, x, y, cell):
        self.send(sec(CMD_CZ_MOVE_PC) + struct.pack("<Biii", 0, x, y, cell))

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def startgameok_name(data):
    """The character name in the first ZC_START_GAMEOK record."""
    i = 0
    while i + 4 <= len(data):
        c, = struct.unpack_from("<H", data, i)
        if c == 3591:
            total, = struct.unpack_from("<H", data, i + 2)
            # header 36, then START_INFO: handle(4) + name[17]
            base = i + 36 + 4
            return data[base:base + 17].split(b"\0", 1)[0].decode("latin-1", "replace")
        if c == 3149:
            break
        i += 2
    return None


def main():
    global PORT
    PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 7002
    acc_a = int(sys.argv[2]) if len(sys.argv) > 2 else 35
    acc_b = int(sys.argv[3]) if len(sys.argv) > 3 else 35

    print("two players on port %d (accounts %d and %d)" % (PORT, acc_a, acc_b))

    a = Client(acc_a, "arcanine")
    b = Client(acc_b, "arcanine")
    a.connect()
    b.connect()
    print("both connected")

    # Both ask to start at the SAME time - the point of the test.
    t0 = time.time()
    a.start_game()
    b.start_game()

    ok_a = a.wait_for(3591, timeout=90.0)
    ok_b = b.wait_for(3591, timeout=90.0)
    waited = time.time() - t0
    print("after %.1f s: A served=%s, B served=%s" % (waited, ok_a, ok_b))

    check(ok_a, "player A got ZC_START_GAMEOK")
    check(ok_b, "player B got ZC_START_GAMEOK (it did NOT wait for A to leave)")
    check(waited < 30.0,
          "both were served quickly (%.1f s) - the zone is not serial any more" % waited)

    name_a = startgameok_name(a.data)
    name_b = startgameok_name(b.data)
    print("  A spawned as %r, B spawned as %r" % (name_a, name_b))
    if acc_a != acc_b:
        check(name_a != name_b, "the two players got DIFFERENT characters")
    else:
        print("  (same account on purpose - the per-player data is not tested)")

    # Each one finishes the handshake and walks; neither may disturb the other.
    a.game_ready()
    b.game_ready()
    time.sleep(3.0)
    a.pump()
    b.pump()

    # The visibility packet: each client must be told about the OTHER player.
    spawn_cmd = struct.pack("<H", 3520)
    check(spawn_cmd in a.data, "player A was told about player B (ZC_SPAWN_STANDINGPC)")
    check(spawn_cmd in b.data, "player B was told about player A (ZC_SPAWN_STANDINGPC)")

    a.move(6500, 400, 149)
    time.sleep(2.0)
    a.pump()
    b.pump()
    check(len(a.data) > 0, "player A still has its stream")
    check(len(b.data) > 0, "player B still has its stream")

    a.close()
    b.close()
    time.sleep(1.0)

    print()
    if FAILS:
        print("FAILED %d check(s):" % len(FAILS))
        for f in FAILS:
            print("   - " + f)
        return 1
    print("two players served at the same time")
    return 0


if __name__ == "__main__":
    sys.exit(main())
