r"""Prove a MENU NPC opens the window its branch picks - Leonardo's shop is option 3.

Leonardo (`LEO_SERVICE`, Reboldoeux, handle 0x40000050) is the case that a
"one NPC = one shop" shortcut gets wrong. His click script asks a question and
only ONE of the answers is a shop:

    select = ShowSelDlg(pc, "<$>10137015</>", ... 7 options ...)
    else if (select == 3) { ShowNPCTradeDlg(self, pc, 'ETC_LEONARDO', shoptype); }

Before this check, the zone decided the shop by scanning the script file at click
time. That opens the window for ARMANDO-style single-shop NPCs, but for a menu it
opens the shop without the player having chosen anything - and a shop chosen by a
RESUMED branch sent nothing at all, because `ShowNPCTradeDlg` was a stub.

What this asserts, through the real zone server:
  1. the click asks a QUESTION (ZC_DIALOG_SELECT), and does NOT open a shop yet;
  2. answering "3" opens the shop window (ZC_DIALOG_NPC_TRADE).

Needs a zone server on 127.0.0.1:7002 serving Reboldoeux. It answers option 3
(the shop) and then drops the connection, so it buys nothing.

Usage
-----
    python check_leonardo.py            # port 7002
    python check_leonardo.py 7002
"""

import os
import socket
import struct
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from check_quest_live import (drain, find_command, find_packet, login, packets,
                              sec, send, wait_for)

CMD_CZ_CLICK_TRIGGER = 3679
CMD_CZ_DIALOG_SELECT = 3657
CMD_CZ_ACK_DIALOG = 3656
CMD_ZC_DIALOG_OK = 3660
CMD_ZC_DIALOG_CLOSE = 3665
CMD_ZC_DIALOG_SELECT = 3663
CMD_ZC_DIALOG_NPC_TRADE = 3680
CMD_ZC_DIALOG_NPC_CUSTOM_TRADE = 3681

LEONARDO = 0x40000050          # zone_npcs(10) index 80, script LEO_SERVICE
SHOP_OPTION = 3                # the "shop" answer in Leonardo's menu


def dialog_options(body):
    """The option strings of a ZC_DIALOG_SELECT body: type(1) count(1) handle(4)."""
    if len(body) < 6:
        return []
    count = body[1]
    text = body[6:].split(b"\0")
    text = [t.decode("latin-1", "replace") for t in text if t]
    return text[:count - 1] if count > 1 else text


def run(port):
    problems = []

    def expect(label, got, want):
        ok = got == want
        print("  %s %s: got %r, want %r"
              % ("OK  " if ok else "FAIL", label, got, want))
        if not ok:
            problems.append(label)

    print("zone 127.0.0.1:%d, Leonardo handle 0x%08x" % (port, LEONARDO))
    sock = login(port, 24)
    try:
        # 1. the click: a QUESTION, not a shop
        send(sock, sec(CMD_CZ_CLICK_TRIGGER)
             + struct.pack("<BBI", 0, 0, LEONARDO))
        buf = wait_for(sock, CMD_ZC_DIALOG_SELECT, 25.0)
        seen = [cmd for cmd, _ in packets(buf)]
        menu = find_packet(buf, CMD_ZC_DIALOG_SELECT)
        print("  ..   first click: %d bytes, ids %s"
              % (len(buf), " ".join(str(c) for c in seen[:12])))
        expect("the click asks a question", menu is not None, True)
        expect("no shop before the answer",
               find_packet(buf, CMD_ZC_DIALOG_NPC_TRADE) is None, True)
        options = dialog_options(menu) if menu else []
        print("  ..   prompt=%r" % (options[0] if options else None))
        print("  ..   options=%r" % (options[1:] if options else []))

        # 2. answer "shop": the window opens straight away, and NOTHING else.
        #    Captured from the live server 2026-10-23: DIALOG_SELECT ->
        #    NPC_TRADE -> (shop dismissed) -> DIALOG_CLOSE.
        send(sock, sec(CMD_CZ_DIALOG_SELECT) + bytes([0, SHOP_OPTION]))
        buf = wait_for(sock, CMD_ZC_DIALOG_NPC_TRADE, 8.0)
        seen = [cmd for cmd, _ in packets(buf)]
        print("  ..   choice %d: %d bytes, ids %s"
              % (SHOP_OPTION, len(buf), " ".join(str(c) for c in seen[:12])))
        expect("the shop window opened",
               find_packet(buf, CMD_ZC_DIALOG_NPC_TRADE) is not None, True)
        expect("no extra OK dialog",
               find_packet(buf, CMD_ZC_DIALOG_OK) is None, True)
        expect("no close before the shop is dismissed",
               find_command(buf, CMD_ZC_DIALOG_CLOSE), False)
        expect("it is the NORMAL shop, not custom",
               find_packet(buf, CMD_ZC_DIALOG_NPC_CUSTOM_TRADE) is None, True)

        # 3. the player closes the shop: the client sends CZ_Ack_Dialog (that is
        #    CGENormalUIHandler::OnMsgCloseNpcTrade) and the dialog must end -
        #    this is the step that stops the player being locked in it.
        send(sock, sec(CMD_CZ_ACK_DIALOG) + bytes([0, 0]))
        time.sleep(1.0)
        after = drain(sock, 2.0)
        expect("the dialog closed when the shop was dismissed",
               find_command(after, CMD_ZC_DIALOG_CLOSE), True)
    finally:
        try:
            sock.close()
        except OSError:
            pass

    ok = not problems
    print("%s (%d problem%s)" % ("PASS" if ok else "FAIL", len(problems),
                                 "" if len(problems) == 1 else "s"))
    return 0 if ok else 1


def main(argv):
    port = int(argv[0]) if argv and argv[0].isdigit() else 7002
    return run(port)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
