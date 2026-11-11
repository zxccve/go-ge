"""Synthetic zone client: proves the NPC click rules end to end.

Mirrors what the real client does, which matters because the CLIENT is the one
that walks the player to a clicked NPC (its AI is handed the trigger's ground
position) and it reports that walk as its own CZ_MOVE_PC steps. A move the
SERVER pushes on its own is only animated, so the server must not invent one for
a click made from a standstill.

Cases:
  1. click from a standstill -> the server sends NO move; the client walks (we
     emulate that) and the dialog opens on arrival, with no second click
  2. click while walking from a ground click -> the walk is cut short at once and
     the chase aims at the NPC
  3. click, then walk elsewhere -> never a dialog
  4. click A while walking, then click B ONCE -> B only cancels A's dialog, the
     walk to A is not redirected
  5. ... the same click on B a SECOND time -> the walk is cancelled and sent to B
     and B's dialog opens on arrival

Run it while zone_server.py is up. The server log is the evidence.
"""
import socket
import struct
import sys
import time

import ge_crypt as gc
import npc_script
import zone_server as z

HOST, PORT = "127.0.0.1", int(sys.argv[1]) if len(sys.argv) > 1 else 7002
CMD_CZ_START_GAME = 3590
CMD_CZ_MOVE_PC = 3598
CMD_CZ_CLICK_TRIGGER = 3679

DIALOG_CMD = struct.pack("<H", z.CMD_ZC_DIALOG_OK)   # 4C 0E
MOVE_CMD = struct.pack("<H", z.CMD_ZC_MOVE_PC)
TRADE_CMD = struct.pack("<H", z.CMD_ZC_DIALOG_NPC_TRADE)
CLOSE_CMD = struct.pack("<H", z.CMD_ZC_DIALOG_CLOSE)


def sec(cmd, extra=b""):
    """PKS_SECURITY_DEFAULT: command(2) + seq(4) + checksum(4)."""
    return struct.pack("<HII", cmd, 0, 0) + extra


def send(sock, body):
    sock.sendall(gc.encrypt_packet(body))


def drain(sock, seconds=1.0):
    """Read what arrives in the next `seconds` - in TOTAL, not until a gap.

    The zone sends a move step every tick while it walks the player to a
    clicked NPC, so a drain that waits for a `seconds` GAP would keep
    reading for the whole walk and would see the dialog that opens on
    arrival - and then the test would think the dialog opened too early.
    This reads for `seconds` of wall clock and stops, whatever arrives.
    """
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


def start_game(sock):
    send(sock, sec(CMD_CZ_START_GAME) + struct.pack("<II", 1, 1) + b"Arcanine\0")


def move_to(sock, pos):
    """A walk request - what the client sends for a ground click AND for its own
    AI walk toward a clicked NPC."""
    send(sock, sec(CMD_CZ_MOVE_PC) + struct.pack("<Biii", 0, pos[0], pos[1],
                                                 pos[2]))


def click(sock, handle, longdistance=0):
    send(sock, sec(CMD_CZ_CLICK_TRIGGER) + struct.pack("<BBI", 0, longdistance,
                                                       handle))


def walk_wait(a, b):
    """The server's interpolated walk time between two points, plus margin."""
    return z._dist2d(a, b) / z.MOVE_SPEED_DEFAULT + 0.5


def show(tag, reply):
    print("  %s: bytes=%d  move=%s dialog=%s"
          % (tag, len(reply), MOVE_CMD in reply, DIALOG_CMD in reply))
    return reply


def settle(sock, pos, wait=3.0):
    """Make a short walk and let it finish, so no click state carries over."""
    move_to(sock, pos)
    time.sleep(wait)
    drain(sock, 1.0)


def main():
    z.load_players()
    spawn = z.CUR.pos
    npcs = z.zone_npcs(z.ZONE_ID)
    print("zone=%d spawn=%s npcs=%d" % (z.ZONE_ID, spawn, len(npcs)))

    idx = 5
    npc = npcs[idx]
    handle = z.NPC_HANDLE_BASE + idx
    at_npc = (npc["x"], npc["y"], npc["cell"])
    far = (npc["x"] + 2828, npc["y"] + 2000, npc["cell"])
    b_idx = 52                                   # a second NPC 791 units away
    b_npc = npcs[b_idx]
    b_handle = z.NPC_HANDLE_BASE + b_idx
    print("npc #%d at (%d,%d) handle=0x%08x; npc #%d at (%d,%d) handle=0x%08x"
          % (idx, npc["x"], npc["y"], handle, b_idx, b_npc["x"], b_npc["y"],
             b_handle))

    sock = socket.create_connection((HOST, PORT))
    print("connected")
    start_game(sock)
    time.sleep(4.0)
    drain(sock, 1.0)                     # entry packets

    # ---- 1. a click from a standstill: the CLIENT walks, the server does not --
    move_to(sock, far)
    time.sleep(walk_wait(spawn[:2], far[:2]))
    drain(sock, 0.5)
    print("standing %.0f units away -> clicking"
          % z._dist2d(far[:2], (npc["x"], npc["y"])))
    click(sock, handle)
    reply = show("reply to the click", drain(sock, 1.5))
    assert MOVE_CMD in reply, "a far click must be answered with a chase"
    assert DIALOG_CMD not in reply, "the dialog must not open before arriving"
    move_to(sock, at_npc)                # the client's own AI walks the player
    time.sleep(walk_wait(far[:2], at_npc[:2]) + 1.0)
    reply = show("on arrival (no second click)", drain(sock, 3.0))
    assert DIALOG_CMD in reply, "the remembered click must answer on arrival"

    # ---- 2. a click during a GROUND walk cuts it short ----------------------
    print("clicking while walking somewhere else")
    move_to(sock, far)
    time.sleep(walk_wait(at_npc[:2], far[:2]))
    drain(sock, 0.5)
    other = (npc["x"] - 1500, npc["y"] - 1200, npc["cell"])
    move_to(sock, other)
    click(sock, handle)
    reply = show("reply to the mid-walk click", drain(sock, 1.5))
    assert DIALOG_CMD not in reply, "nothing may open mid-stride"
    i = reply.rfind(MOVE_CMD)            # the echo of the ground move comes first
    assert i >= 0, "the click must cut the ground walk short, not wait it out"
    mx, my = struct.unpack_from("<ii", reply, i + 10)
    d = z._dist2d((mx, my), (npc["x"], npc["y"]))
    print("  the redirect aims %.0f units from the NPC" % d)
    assert d <= z.NPC_CLICK_RANGE, "the redirect must aim at the NPC"
    move_to(sock, at_npc)
    time.sleep(walk_wait(far[:2], at_npc[:2]) + 1.0)
    reply = show("on arrival (no second click)", drain(sock, 3.0))
    assert DIALOG_CMD in reply, "a mid-walk click must be answered too"

    # ---- 3. click, then walk elsewhere: never a dialog ----------------------
    print("clicking the NPC, then walking somewhere else")
    move_to(sock, far)
    time.sleep(walk_wait(other[:2], far[:2]))
    drain(sock, 0.5)
    click(sock, handle)
    drain(sock, 0.3)
    away = (npc["x"] + 4000, npc["y"] + 3000, npc["cell"])
    move_to(sock, away)
    time.sleep(walk_wait(far[:2], away[:2]) + 2.5)
    reply = show("after walking elsewhere", drain(sock, 3.0))
    assert DIALOG_CMD not in reply, "a ground click elsewhere must cancel it"

    # ---- 4. click A while walking, then B ONCE: the walk stays LOCKED --------
    # The lock exists while a chase is in flight, so start from a walk.
    print("clicking A mid-walk, then B once")
    settle(sock, (npc["x"] + 300, npc["y"] + 300, npc["cell"]))
    move_to(sock, far)
    time.sleep(walk_wait(away[:2], far[:2]))
    drain(sock, 0.5)
    move_to(sock, other)                 # set off, so A's click chases
    click(sock, handle)
    reply = show("reply to the click on A", drain(sock, 1.5))
    assert MOVE_CMD in reply, "a click mid-walk chases, which sets the lock"
    click(sock, b_handle)                # B once
    reply = show("reply to the first click on B", drain(sock, 2.0))
    assert MOVE_CMD not in reply, "one click on B must not redirect the walk"
    assert DIALOG_CMD not in reply, "nothing may open mid-stride"
    time.sleep(walk_wait(far[:2], at_npc[:2]) + 2.0)
    reply = show("after arriving at A", drain(sock, 3.0))
    assert DIALOG_CMD not in reply, "the dialog was dropped by the click on B"

    # ---- 5. ... and B a SECOND time cancels the walk and redirects it -------
    print("clicking A mid-walk, then B twice")
    settle(sock, (npc["x"] + 300, npc["y"] + 300, npc["cell"]))
    move_to(sock, far)
    time.sleep(walk_wait(at_npc[:2], far[:2]))
    drain(sock, 0.5)
    move_to(sock, other)
    click(sock, handle)                  # A
    drain(sock, 1.5)
    click(sock, b_handle)                # B once: only the dialog is cancelled
    drain(sock, 1.0)
    click(sock, b_handle)                # B again: cancel and REDIRECT
    reply = show("reply to the SECOND click on B", drain(sock, 2.0))
    assert MOVE_CMD in reply, "the second click must redirect the walk"
    move_to(sock, (b_npc["x"], b_npc["y"], b_npc["cell"]))
    # The wait must cover the ACTUAL walk, not a fixed number: the NPC list
    # grew from 83 to 119 rows on 2026-10-22 (the client's generation), so the
    # NPC at this index is no longer 791 units away and a fixed 4 s no longer
    # covers the walk - the dialog had not opened yet when the test looked.
    time.sleep(walk_wait(other[:2], (b_npc["x"], b_npc["y"])) + 2.0)
    reply = show("after arriving at B", drain(sock, 3.0))
    assert DIALOG_CMD in reply, "the new target's dialog must open on arrival"

    # ---- 6. a SHOP NPC: the line first, then the item list when OK is pressed --
    # The original shows a dialog and opens the shop from it (that dialog is what
    # keeps the character in place), so the trade window must NOT appear on the
    # click itself.
    print("clicking a shop NPC, then pressing OK")
    # Find the shop NPC by what it IS, not by its row number: the NPC list grew
    # from 83 to 119 rows on 2026-10-22 (the client's generation), so the old
    # hardcoded index pointed at a different NPC and the shop never opened.
    # The shop's group name comes from its own script.
    s_idx = None
    for i, n in enumerate(npcs):
        script = n.get("script")
        if not script:
            continue
        trade = npc_script.trade_for(str(script))
        if trade and trade[1] == "ETC_LEONARDO":
            s_idx = i
            break
    assert s_idx is not None, "the ETC_LEONARDO shop NPC must exist"
    s_npc = npcs[s_idx]
    settle(sock, (s_npc["x"], s_npc["y"], s_npc["cell"]), wait=13.0)
    click(sock, z.NPC_HANDLE_BASE + s_idx)
    reply = show("reply to the shop click", drain(sock, 2.0))
    assert DIALOG_CMD in reply, "a shop NPC answers with its line first"
    assert TRADE_CMD not in reply, "the shop must wait for the OK"
    send(sock, sec(z.CMD_CZ_ACK_DIALOG) + struct.pack("<BB", 0, 0))
    reply = show("after OK", drain(sock, 2.0))
    assert TRADE_CMD in reply, "the item list must follow the OK"
    assert CLOSE_CMD not in reply, \
        "the dialog must STAY open: that is what keeps the character in place"
    i = reply.rfind(TRADE_CMD)
    cmd, ln = struct.unpack_from("<HH", reply, i)
    print("  trade packet: cmd=%d len=%d" % (cmd, ln))
    # The CURRENT design sends the shop GROUP NAME and no item ids: the client
    # resolves the stock from its own shop table (captured from the live
    # server, 2026-10-17). So the packet is the 40-byte group40 shape. The
    # old "13 + 4 * count" shape is the v28/source2 layout and is not what we
    # send.
    assert cmd == 3680, "the shop window is ZC_DIALOG_NPC_TRADE (3680)"
    assert ln == 40, "the group-name shop packet is 40 bytes (got %d)" % ln
    assert b"ETC_LEONARDO" in reply[i:i + ln], \
        "the packet must name the shop group"
    # The client's shop CLOSE sends this same ACK packet, so this one ends the
    # dialog.
    send(sock, sec(z.CMD_CZ_ACK_DIALOG) + struct.pack("<BB", 0, 0))
    reply = show("after closing the shop", drain(sock, 2.0))
    assert CLOSE_CMD in reply, "closing the shop must close the dialog"
    assert TRADE_CMD not in reply, "no second shop window"

    sock.close()
    print("done - all cases passed")

if __name__ == "__main__":
    main()
