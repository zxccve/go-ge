r"""Prove the Linden quest through the REAL zone server, and check the packets.

`check_quest.py` proves the logic with no server. This one is the other half:
it connects the way the client does, clicks the NPC, and reads the bytes the
zone sends back - so a missing `ZC_ADDITEM` or a stale `ZC_QUEST_INFO` fails
here even when the database is right.

What it asserts, in order:
  1. the first click commits and the zone sends `ZC_ADDITEM` (the reward) AND
     `ZC_QUEST_INFO` whose `Quest_00001` byte is 2 - the panel's own data;
  2. the SECOND click is refused by the script itself, so the zone sends the
     "already taken" line and NEITHER `ZC_ADDITEM` NOR `ZC_QUEST_INFO`;
  3. the database agrees with the packets (state moved once, one reward).

Needs a zone server on 127.0.0.1:7002 serving Reboldoeux (`reb_basic`). The
commander is put into the pre-quest state and restored afterward.

Usage
-----
    python check_quest_live.py                 # port 7002, commander 24
    python check_quest_live.py 7002
"""

import os
import socket
import struct
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import check_quest

# client -> server
CMD_CZ_START_GAME = 3590
CMD_CZ_CLICK_TRIGGER = 3679
# server -> client
CMD_ZC_DIALOG_OK = 3660
CMD_ZC_ADDITEM = 3607
CMD_ZC_QUEST_INFO = 3872

LINDEN_HANDLE = 0x40000001        # zone_npcs(10) index 1, script C1_REB_LINDEN
CHARACTER = "Arcanine"            # the name the zone maps to account 35 / commander 24


def sec(cmd, extra=b""):
    return struct.pack("<HII", cmd, 0, 0) + extra


def send(sock, body):
    import ge_crypt
    sock.sendall(ge_crypt.encrypt_packet(body))


def drain(sock, seconds=1.5):
    """Every server->client byte for the next few seconds (plaintext on that side)."""
    end = time.time() + seconds
    out = b""
    while True:
        left = end - time.time()
        if left <= 0:
            return out
        try:
            sock.settimeout(left)
            chunk = sock.recv(65536)
        except socket.timeout:
            return out
        if not chunk:
            return out
        out += chunk


def packets(data):
    """[(cmd, body)] from a server->client stream: cmd(2) len(2) body.

    Resynchronises on a bad header instead of stopping. A drain can end in the
    middle of a packet (the chase and the dialog arrive seconds apart), and a
    parser that gives up there silently hides every later packet - which is how
    a working shop window looked like a failure.
    """
    out = []
    i = 0
    while i + 4 <= len(data):
        cmd, ln = struct.unpack_from("<HH", data, i)
        if ln < 4 or i + ln > len(data):
            i += 1                      # mis-framed: skip a byte and resync
            continue
        out.append((cmd, data[i + 4:i + ln]))
        i += ln
    return out


def find_packet(data, cmd):
    """The body of `cmd`, found at ANY offset - or None.

    Deliberately not a strict stream walk. A drain can begin in the middle of a
    packet, and a walk that takes one bogus length can never backtrack, so it
    reports "the packet never came" while the server log shows it sent. Scanning
    for the 2-byte id and accepting the first plausible length cannot drift.
    """
    pattern = struct.pack("<H", cmd)
    at = data.find(pattern)
    while at != -1:
        if at + 4 <= len(data):
            ln = struct.unpack_from("<H", data, at + 2)[0]
            if 4 <= ln <= len(data) - at:
                return data[at + 4:at + ln]
        at = data.find(pattern, at + 1)
    return None


def find_command(data, cmd):
    """True when a COMMAND-ONLY packet appears.

    Some packets carry no length field (`ZC_DIALOG_CLOSE` is exactly the 2-byte
    id), so `find_packet` cannot see them - it needs 4 bytes to trust a header.
    """
    return struct.pack("<H", cmd) in data


def wait_for(sock, cmd, seconds):
    """Collect raw bytes until `cmd` appears or the time runs out; return them.

    A click on a distant NPC CHASES first, so the answer arrives seconds after
    the click; the caller should not guess a fixed wait.
    """
    end = time.time() + seconds
    buf = b""
    while time.time() < end:
        buf += drain(sock, 1.0)
        if find_packet(buf, cmd) is not None:
            return buf
    return buf


def quest_byte(cmd_body, quest="Quest_00001"):
    """The state byte a ZC_QUEST_INFO body carries for one quest."""
    import zone_server
    names = zone_server.quest_class_names()
    if quest not in names:
        return None
    index = names.index(quest)
    if index >= len(cmd_body):
        return None
    return cmd_body[index]


def login(port, commander):
    """A socket standing where the client stands. It does NOT move the character.

    An earlier version sent `CZ_MOVE_PC` to stand next to the NPC and passed the
    NPC's own `z` (a height) as the map CELL. The zone saved that, and the next
    real login resumed in a cell with no terrain - a black world. Clicking from
    here lets the server chase instead, so nothing persistent changes.
    """
    sock = socket.create_connection(("127.0.0.1", port))
    send(sock, sec(CMD_CZ_START_GAME) + struct.pack("<II", 1, 1)
         + CHARACTER.encode("latin-1") + b"\0")
    time.sleep(5.0)
    drain(sock, 1.0)
    return sock


def click(sock, wait=8.0):
    """Click the NPC. The click may CHASE first, so wait for the arrival."""
    send(sock, sec(CMD_CZ_CLICK_TRIGGER) + struct.pack("<BBI", 0, 0, LINDEN_HANDLE))
    time.sleep(1.0)
    return packets(drain(sock, wait))


def run(port, commander):
    import ge_db
    import ge_item_index

    class_id = ge_item_index.class_id(check_quest.GIVEN)
    original = ge_db.get_commander_quest_states(commander)
    before_props = {name: original.get(name, 0) for name in check_quest.WATCH}
    before_item = check_quest.held(commander, class_id)
    problems = []

    def expect(label, got, want):
        ok = got == want
        print("  %s %s: got %r, want %r"
              % ("OK  " if ok else "FAIL", label, got, want))
        if not ok:
            problems.append(label)

    print("zone 127.0.0.1:%d, commander %d, quest %s"
          % (port, commander, check_quest.SCRIPT))
    print("before: props=%s held=%d" % (before_props, before_item))

    sock = None
    try:
        for name, value in check_quest.SEED.items():
            if not ge_db.set_commander_quest_state(commander, name, value):
                print("FAIL: could not seed %s=%d" % (name, value))
                return 1

        sock = login(port, commander)

        # 1. the accept
        first = click(sock)
        cmds = [cmd for cmd, _ in first]
        print("  ..   first click sent %d packets: %s"
              % (len(cmds), " ".join(str(c) for c in cmds)))
        expect("ZC_ADDITEM sent on accept", CMD_ZC_ADDITEM in cmds, True)
        expect("ZC_QUEST_INFO sent on accept", CMD_ZC_QUEST_INFO in cmds, True)
        info = [body for cmd, body in first if cmd == CMD_ZC_QUEST_INFO]
        expect("panel byte Quest_00001", quest_byte(info[-1]) if info else None, 2)
        expect("AlqMission after accept",
               ge_db.get_commander_quest_states(commander).get("AlqMission"), 2)
        expect("%s given once" % check_quest.GIVEN,
               check_quest.held(commander, class_id), before_item + 1)

        # 2. the SECOND click: the script's own guard refuses it
        second = click(sock)
        cmds = [cmd for cmd, _ in second]
        print("  ..   second click sent %d packets: %s"
              % (len(cmds), " ".join(str(c) for c in cmds)))
        expect("no ZC_ADDITEM on the repeat", CMD_ZC_ADDITEM in cmds, False)
        expect("no ZC_QUEST_INFO on the repeat", CMD_ZC_QUEST_INFO in cmds, False)
        text = b"".join(body for cmd, body in second if cmd == CMD_ZC_DIALOG_OK)
        expect("refusal line is 10146202", b"10146202" in text, True)
        expect("still one reward", check_quest.held(commander, class_id),
               before_item + 1)
        expect("AlqMission unchanged",
               ge_db.get_commander_quest_states(commander).get("AlqMission"), 2)
    finally:
        if sock is not None:
            try:
                sock.close()
            except OSError:
                pass
        time.sleep(1.0)
        for name, value in before_props.items():
            ge_db.set_commander_quest_state(commander, name, value)
        if check_quest.held(commander, class_id) > before_item:
            import script_host
            host = script_host.ScriptHost(zone_id=10)
            host.commander_id = commander
            host.set_commander(commander)
            tx = host.tx_begin()
            host.tx_item(tx, check_quest.GIVEN,
                         check_quest.held(commander, class_id) - before_item, "take")
            host.tx_end(tx)
        print("restored: props=%s held=%d"
              % ({name: ge_db.get_commander_quest_states(commander).get(name)
                  for name in check_quest.WATCH},
                 check_quest.held(commander, class_id)))

    ok = not problems
    print("%s (%d problem%s)" % ("PASS" if ok else "FAIL", len(problems),
                                 "" if len(problems) == 1 else "s"))
    return 0 if ok else 1


def main(argv):
    port = int(argv[0]) if argv and argv[0].isdigit() else 7002
    commander = int(argv[1]) if len(argv) > 1 and argv[1].isdigit() else 24
    return run(port, commander)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
