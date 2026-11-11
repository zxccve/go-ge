r"""Prove the inventory purchase reaches the CLIENT, not just the database.

`check_inventory_service.py` proves the transaction. This proves the part the
player actually sees: that the zone sends `ZC_UPDATE_PROPERTY_NUMBER` (3748) with
the new `Lock_InventoryCount`, because the client's bag capacity is its own
cached IES value - the DB alone changes nothing on screen.

It buys the 10-slot option and asserts the packet carries old+10, then restores
the property and the Feso.

Needs a zone server serving Reboldoeux.   python check_inventory_live.py [port]
"""

import os
import socket
import struct
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import check_inventory_service as base
from check_quest_live import (drain, find_command, find_packet, login, packets,
                              sec, send, wait_for)

CMD_CZ_CLICK_TRIGGER = 3679
CMD_CZ_DIALOG_SELECT = 3657
CMD_ZC_DIALOG_SELECT = 3663
CMD_ZC_UPDATE_PROPERTY_NUMBER = 3748

PROP = "Lock_InventoryCount"


def find_fix_packet(data, cmd, size):
    """The payload of a FIX packet: 2-byte id, then a fixed payload, no length.

    `find_packet` cannot work here - it reads a length at +2, which for this
    packet is the first two letters of idSpace.
    """
    pattern = struct.pack("<H", cmd)
    at = data.find(pattern)
    while at != -1:
        chunk = data[at + 2:at + 2 + size]
        if len(chunk) == size and b"\0" in chunk[:20]:
            return chunk
        at = data.find(pattern, at + 1)
    return None


def prop_value(body):
    """(property, value) out of a ZC_UPDATE_PROPERTY_NUMBER payload."""
    if len(body) < 44:
        return None, None
    name = body[20:40].split(b"\0", 1)[0].decode("latin-1", "replace")
    value = struct.unpack_from("<I", body, 40)[0]
    return name, value


def run(port, commander, handle):
    before = base.slots(commander)
    before_feso = base.held(commander, 30001)
    problems = []

    def expect(label, got, want):
        ok = got == want
        print("  %s %s: got %r, want %r"
              % ("OK  " if ok else "FAIL", label, got, want))
        if not ok:
            problems.append(label)

    print("zone :%d, commander %d, slots=%d feso=%d"
          % (port, commander, before, before_feso))
    if not 248 <= before <= 288:
        print("SKIP: needs slots between 248 and 288 so the 10-slot option exists")
        return 0

    sock = login(port, commander)
    try:
        send(sock, sec(CMD_CZ_CLICK_TRIGGER) + struct.pack("<BBI", 0, 0, handle))
        buf = wait_for(sock, CMD_ZC_DIALOG_SELECT, 25.0)
        if find_packet(buf, CMD_ZC_DIALOG_SELECT) is None:
            print("FAIL: Leonardo did not answer")
            return 1
        seen = b""
        for choice in (5, 2, 3):        # inventory -> use -> 10 slots
            send(sock, sec(CMD_CZ_DIALOG_SELECT) + bytes([0, choice]))
            time.sleep(1.2)
            seen += drain(sock, 1.0)    # KEEP it: the packet we want arrives here
        body = find_fix_packet(seen, CMD_ZC_UPDATE_PROPERTY_NUMBER, 44)
        if body is None:
            seen += drain(sock, 2.0)
            body = find_fix_packet(seen, CMD_ZC_UPDATE_PROPERTY_NUMBER, 44)
        cmds = [c for c, _ in packets(seen)]
        print("  ..   after the purchase: ids %s" % " ".join(str(c) for c in cmds[-8:]))
        expect("ZC_UPDATE_PROPERTY_NUMBER was sent", body is not None, True)
        if body is not None:
            name, value = prop_value(body)
            print("  ..   property=%r value=%r" % (name, value))
            expect("it carries Lock_InventoryCount", name, PROP)
            expect("with the NEW slot count", value, before + base.WANT)
        expect("the database agrees", base.slots(commander), before + base.WANT)
    finally:
        try:
            sock.close()
        except OSError:
            pass
        time.sleep(1.0)
        now_feso = base.held(commander, 30001)
        if now_feso < before_feso:
            import script_host
            host = script_host.ScriptHost(zone_id=10)
            host.commander_id = commander
            host.set_commander(commander)
            tx = host.tx_begin()
            host.tx_item(tx, base.COIN, before_feso - now_feso, "give")
            host.tx_end(tx)
        base._db().set_commander_quest_state(commander, PROP, before)
        print("restored: slots=%d feso=%d" % (base.slots(commander),
                                              base.held(commander, 30001)))

    ok = not problems
    print("%s (%d problem%s)" % ("PASS" if ok else "FAIL", len(problems),
                                 "" if len(problems) == 1 else "s"))
    return 0 if ok else 1


def main(argv):
    port = int(argv[0]) if argv and argv[0].isdigit() else 7002
    commander = int(argv[1]) if len(argv) > 1 and argv[1].isdigit() else 24
    return run(port, commander, 0x40000050)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
