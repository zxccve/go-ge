"""decode_zone_capture - what the ORIGINAL server really sent, read off the wire.

ONE QUESTION: when the client sent an attack and then a move, did the original
server still send the attack's damage, and in what order relative to its answer
to the move?

Method - every step is a measurement, not an assumption:

  * Client -> server is Blowfish. The cipher uses the CLIENT's own scrambled
    table (source2\\geShared\\Source\\CryptTable.cpp) and the network key is
    geCrypt key_table row 11 ("hsunffalqyrqewes") - the same key
    goserver\\protocol\\crypt.go carries as geKey. Both come from ge_crypt.py,
    so there is one cipher implementation in this project, not two.
  * Client -> server transport frames ONE packet per chunk:
    [WORD blockBytes, little-endian][ciphertext]. The plaintext is padded up to
    a multiple of 8, so the chunk length is ceil(packet/8)*8. That means the
    first WORD inside a chunk is the command id and the chunk COUNT is the
    packet count - a fixed-size packet cannot be missed.
  * Server -> client is plaintext packets back to back, walked with the
    client's own table via analyze_capture.split_packets (its docstring holds
    the two framing rules and the proof that they are right).
  * Neither direction carries a timestamp, so a window is bounded by the PING
    exchange both sides make in order (CZ_PING 3688 / ZC_PING 3689). The
    byte distance is reported too, since that is all the wire has.

Damage in this engine is a pair: ZC_ATTACK_SINGLE (3564) carries the damage
number, and ZC_ADD_HP (3557) carries the HP AFTER as a FLOAT64 at payload +6.
So "damage was sent" is checkable, and so is its direction: a FALL is a hit,
a RISE is regen or a heal. That distinction is what stops a heal from being
read as combat.

Usage:
    python decode_zone_capture.py                 # every capture under data\\captures
    python decode_zone_capture.py DIR_OR_FILE...   # just those

Prints the per-direction id histogram for the whole zone stream, the combat
scan, and up to five windows with every packet in order plus a verdict.
"""
import os
import struct
import sys
import collections

import ge_crypt
import analyze_capture as ac

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
CAPTURES = os.path.join(ROOT, "data", "captures")

# Client -> server (CZ)
CZ_ATTACK = 3666          # the plain-attack request, 15 bytes (goserver CmdCZAttack)
CZ_CLICK_TRIGGER = 3679   # a click on a world object - NOT an attack
CZ_MOVE_PC = 3598
CZ_PING = 3688

# Server -> client (ZC)
ZC_MOVE_PC = 3548
ZC_ADD_HP = 3557
ZC_PING = 3689
ZC_ATTACK_IDS = (3564, 3565, 3566, 3567, 3568)

ATTACK_TRIGGERS = (CZ_ATTACK, CZ_CLICK_TRIGGER)


# --------------------------------------------------------------- reading
def client_packets(path):
    """Decrypt a client->server capture.

    Returns (rows, total_bytes, bad). Each row is
    (file_offset, chunk_bytes, cmd, plaintext).
    """
    data = open(path, "rb").read()
    rows = []
    off = 0
    bad = []
    while off + 2 <= len(data):
        block = struct.unpack_from("<H", data, off)[0]
        if block == 0 or block % 8 or off + 2 + block > len(data):
            bad.append((off, block, len(data) - off))
            break
        plain = ge_crypt.decrypt_packet(data[off:off + 2 + block])
        cmd = struct.unpack_from("<H", plain, 0)[0]
        rows.append((off, 2 + block, cmd, plain))
        off += 2 + block
    return rows, len(data), bad


def server_packets(path, sizes):
    """Split a plaintext server->client stream with the client's own table."""
    data = open(path, "rb").read()
    pkts, why = ac.split_packets(data, sizes)
    return data, pkts, why


def wire_len(data, off, cmd, plen, sizes):
    """Bytes this packet occupies on the wire."""
    return sizes[cmd] if sizes.get(cmd) else plen + 4


def id_histogram(cmds, names, limit=None):
    hist = collections.Counter(cmds)
    rows = sorted(hist.items(), key=lambda kv: (-kv[1], kv[0]))
    if limit:
        rows = rows[:limit]
    return rows, len(hist)


def hp_changes(data, pkts, sizes, names):
    """Every ZC_ADD_HP: handle -> list of HP-after floats, in stream order."""
    seq = collections.defaultdict(list)
    for off, cmd, plen in pkts:
        if cmd != ZC_ADD_HP:
            continue
        start = off + 2
        payload = data[start:start + plen]
        if len(payload) < 14:
            continue
        handle = struct.unpack_from("<I", payload, 0)[0]
        hp = struct.unpack_from("<d", payload, 4)[0]
        seq[handle].append(hp)
    return seq


def ping_index(rows, cmd_at=2):
    """Assign each packet the number of pings seen before it.

    cmd_at differs per direction: client rows are (off, wire, cmd, plain) and
    server rows are (off, cmd, payload_len), so the id is NOT always field 2.
    """
    out = []
    n = 0
    for row in rows:
        cmd = row[cmd_at]
        out.append((n, row))
        if cmd == CZ_PING or cmd == ZC_PING:
            n += 1
    return out


# --------------------------------------------------------------- windows
def find_windows(c2s, names, max_windows=5):
    """Anchor on an attack-or-click, then take the next move, once each."""
    triggers = [i for i, r in enumerate(c2s) if r[2] in ATTACK_TRIGGERS]
    windows = []
    used = set()
    for i in triggers:
        j = None
        for k in range(i + 1, len(c2s)):
            if c2s[k][2] == CZ_MOVE_PC:
                j = k
                break
        if j is None or j in used:
            continue
        used.add(j)
        windows.append((i, j))
        if len(windows) >= max_windows:
            break
    return windows


def describe_window(c2s, i, j, c2s_ping, s2c_ping, names, sizes):
    """Every packet in the window, both directions, in order, with a verdict."""
    a = c2s[i]
    b = c2s[j]
    dist = b[0] - a[0] - a[1]
    print("  client asked, in order (byte offset = the wire position):")
    for k in range(i, j + 1):
        off, wlen, cmd, plain = c2s[k]
        tag = "<- anchor" if k == i else ("<- move" if k == j else "")
        print("    c2s %+8d  %5d %-30s wire %3d  %s"
              % (off, cmd, names.get(cmd, "?"), wlen, tag))
        if cmd == CZ_ATTACK:
            seq, chk = struct.unpack_from("<II", plain, 2)
            char_index, target = struct.unpack_from("<B", plain, 10)[0], \
                struct.unpack_from("<i", plain, 11)[0]
            print("           CZ_ATTACK seq=%d checksum=0x%08X charIndex=%d target=%d"
                  % (seq, chk, char_index, target))
    print("  byte distance between the two requests: %d bytes" % dist)

    # Server side: same ping ticks. Ping index is the only clock on the wire.
    lo = c2s_ping[i]
    hi = c2s_ping[j]
    span = [(p, r) for p, r in s2c_ping if lo <= p <= hi]
    print("  server answered, in order (ping tick %d..%d of the s2c stream):"
          % (lo, hi))
    if not span:
        print("    (nothing - the server sent no packet between those ticks)")
    for p, (off, cmd, plen) in span:
        tag = ""
        if cmd == ZC_MOVE_PC:
            tag = "  <- move echo"
        elif cmd in ZC_ATTACK_IDS:
            tag = "  <- DAMAGE"
        elif cmd == ZC_ADD_HP:
            tag = "  <- HP after"
        print("    s2c %+8d  %5d %-30s payload %3d%s"
              % (off, cmd, names.get(cmd, "?"), plen, tag))

    # Verdict.
    attack = [k for k in range(i, j + 1) if c2s[k][2] == CZ_ATTACK]
    dmg = [n for n, (p, (off, cmd, plen)) in enumerate(span) if cmd in ZC_ATTACK_IDS]
    echo = [n for n, (p, (off, cmd, plen)) in enumerate(span) if cmd == ZC_MOVE_PC]
    if not attack:
        kind = names.get(a[2], "?")
        verdict = ("no attack in this window (%s only) -> no damage at all" % kind)
    elif not dmg:
        verdict = "attack was cancelled (no damage packet answered it)"
    elif echo and min(dmg) < min(echo):
        verdict = "attack landed through the move (damage before the move echo)"
    elif echo:
        verdict = "attack landed, but the move echo came first"
    else:
        verdict = "attack landed through the move (no move echo at all)"
    print("  VERDICT: %s" % verdict)
    return verdict


# --------------------------------------------------------------- sections
def report_session(tag, c2s_path, s2c_path, sizes, names):
    print("\n" + "=" * 78)
    print("capture: %s" % tag)
    print("  c2s: %s" % os.path.basename(c2s_path))
    print("  s2c: %s" % os.path.basename(s2c_path))
    print("  cipher: client table + %s (key_table row 11)"
          % ge_crypt.KEY.decode("latin-1"))

    c2s, c2s_size, bad = client_packets(c2s_path)
    print("  c2s decoded: %d chunks for %d bytes%s"
          % (len(c2s), c2s_size,
             ", ZERO bytes left over" if not bad else ", STOPPED at %r" % (bad[0],)))
    mism = [(off, cmd, wlen) for off, wlen, cmd, plain in c2s
            if sizes.get(cmd) and (wlen - 2) != (sizes[cmd] + 7) // 8 * 8]
    print("  c2s fixed-size packets whose chunk length disagrees with the "
          "client's table: %d" % len(mism))

    if not os.path.exists(s2c_path):
        print("  s2c: MISSING - no server reply capture for this leg")
        return
    data, s2c, why = server_packets(s2c_path, sizes)
    print("  s2c split: %d packets, %s" % (len(s2c), why))

    # ---- whole-stream histogram, client side
    rows, distinct = id_histogram([r[2] for r in c2s], names)
    print("\n  -- id histogram, zone leg client->server (%d packets, %d ids) --"
          % (len(c2s), distinct))
    for cmd, n in rows:
        print("    %5d %-32s %4d" % (cmd, names.get(cmd, "?"), n))

    # ---- whole-stream histogram, server side
    rows, distinct = id_histogram([p[1] for p in s2c], names)
    print("\n  -- id histogram, zone leg server->client (%d packets, %d ids) --"
          % (len(s2c), distinct))
    for cmd, n in rows:
        mark = ""
        if cmd in ZC_ATTACK_IDS:
            mark = "  <== DAMAGE PACKET"
        elif cmd == ZC_MOVE_PC:
            mark = "  <== move echo"
        elif cmd == ZC_ADD_HP:
            mark = "  <== HP after (damage or regen)"
        print("    %5d %-32s %4d%s" % (cmd, names.get(cmd, "?"), n, mark))

    # ---- combat scan
    c2s_cmds = [r[2] for r in c2s]
    s2c_cmds = [p[1] for p in s2c]
    n_attack = c2s_cmds.count(CZ_ATTACK)
    n_click = c2s_cmds.count(CZ_CLICK_TRIGGER)
    n_move = c2s_cmds.count(CZ_MOVE_PC)
    n_dmg = sum(s2c_cmds.count(c) for c in ZC_ATTACK_IDS)
    print("\n  -- combat scan, whole zone stream --")
    print("    CZ_ATTACK 3666 (the plain-attack request) .... %d" % n_attack)
    print("    CZ_MOVE_PC 3598 ............................. %d" % n_move)
    print("    CZ_CLICK_TRIGGER 3679 (a click, not a fight) . %d" % n_click)
    print("    ZC_ATTACK_* 3564/65/66/67/68 (damage) ....... %d" % n_dmg)
    print("    ZC_MOVE_PC 3548 (move echo) ................. %d"
          % s2c_cmds.count(ZC_MOVE_PC))

    # A packet hidden INSIDE a chunk would be invisible to the histogram, so look
    # for the raw two-byte id anywhere in the decrypted bytes as well.
    blob = b"".join(r[3] for r in c2s)
    raw_hits = 0
    start = 0
    while True:
        k = blob.find(struct.pack("<H", CZ_ATTACK), start)
        if k < 0:
            break
        raw_hits += 1
        start = k + 1
    print("    raw scan: the bytes of CZ_ATTACK's id anywhere in the decrypted")
    print("    client stream (catches a packet hidden inside a chunk) .. %d" % raw_hits)

    hp = hp_changes(data, s2c, sizes, names)
    falls = rises = 0
    for handle, values in hp.items():
        for a, b in zip(values, values[1:]):
            if b < a - 1e-9:
                falls += 1
            elif b > a + 1e-9:
                rises += 1
    print("    ZC_ADD_HP 3557 HP changes ................... %d (rises %d, falls %d)"
          % (sum(len(v) for v in hp.values()), rises, falls))
    for handle, values in list(hp.items())[:3]:
        if values:
            print("      handle %-8d %.4f -> %.4f over %d updates"
                  % (handle, values[0], values[-1], len(values)))

    if not n_attack:
        print("\n  NO COMBAT IN THIS CAPTURE.")
        print("    The client never sent CZ_ATTACK 3666, so there is no attack to")
        print("    land or cancel, and the server never sent a damage packet.")
        if n_click:
            print("    The click in this session was CZ_CLICK_TRIGGER 3679 and the")
            print("    server answered it with no damage - that is not a swing.")
        print("    The nearest window to the question is shown below anyway:")
        print("    the click, then the next move, and what the server sent.")

    # ---- windows
    c2s_ping = ping_index(c2s)
    s2c_ping = ping_index(s2c, 1)
    windows = find_windows(c2s, names)
    print("\n  -- windows: attack/click, then the next move (up to 5) --")
    print("  ping ticks: client %d, server %d"
          % (sum(1 for r in c2s if r[2] == CZ_PING),
             sum(1 for p in s2c if p[1] == ZC_PING)))
    print("  NOTE: this capture has no timestamps, so a window is bounded by the")
    print("  ping ticks only. It can be wider than one second; there is no")
    print("  smaller unit on the wire to cut it with.")
    verdicts = []
    if not windows:
        print("    (no move followed an attack-or-click in this session)")
    for n, (i, j) in enumerate(windows, 1):
        print("\n  window %d" % n)
        verdicts.append(describe_window(c2s, i, j, _ping_map(c2s), s2c_ping,
                                        names, sizes))

    return {
        "capture": tag,
        "attack": n_attack,
        "click": n_click,
        "move": n_move,
        "damage": n_dmg,
        "echo": s2c_cmds.count(ZC_MOVE_PC),
        "rises": rises,
        "falls": falls,
        "verdicts": verdicts,
    }


def _ping_map(rows):
    """packet index -> ping tick (how many pings came before it)."""
    out = {}
    n = 0
    for i, row in enumerate(rows):
        out[i] = n
        if row[2] in (CZ_PING, ZC_PING):
            n += 1
    return out


# --------------------------------------------------------------- driver
def pairs(capdir):
    """(tag, c2s_path, s2c_path) for one capture folder.

    The file names carry the two ports in opposite order:
      c2s_<client port>_<server port>.bin
      s2c_<server port>_<client port>.bin
    so the pair is matched on the (server, client) port set, not on one field.
    """
    files = sorted(os.listdir(capdir))
    out = []
    s2c_files = [f for f in files if f.startswith("s2c_") and f.endswith(".bin")]
    for f in [f for f in files if f.startswith("c2s_") and f.endswith(".bin")]:
        parts = os.path.splitext(f)[0].split("_")
        if len(parts) != 3:
            out.append(("%s/%s" % (os.path.basename(capdir), f),
                        os.path.join(capdir, f), ""))
            continue
        client_port, server_port = parts[1], parts[2]
        match = ""
        for g in s2c_files:
            gp = os.path.splitext(g)[0].split("_")
            if len(gp) != 3:
                continue
            if gp[1:] == parts[1:]:                      # same suffix (x_7054_zone)
                match = os.path.join(capdir, g)
                break
            if gp[1] == server_port and gp[2] == client_port:   # swapped ports
                match = os.path.join(capdir, g)
                break
        out.append(("%s/%s" % (os.path.basename(capdir), f),
                    os.path.join(capdir, f), match))
    return out


def main(argv):
    sizes, names = ac.load_table()
    ge_crypt.load_tables()

    targets = argv[1:]
    if not targets:
        targets = [os.path.join(CAPTURES, d) for d in sorted(os.listdir(CAPTURES))
                   if os.path.isdir(os.path.join(CAPTURES, d))]

    sessions = []
    for t in targets:
        if os.path.isdir(t):
            for tag, c2s, s2c in pairs(t):
                # A zone leg is any client stream whose PEER port is not the
                # barrack's 7001: the live zone moves port between builds
                # (7054 on 2026-10-21, 7061 on 2026-11-01), so matching the
                # literal "7054" silently skipped a whole capture.
                base = os.path.basename(c2s)
                fields = base.split("_")
                peer = fields[2].split(".")[0] if len(fields) > 2 else ""
                if "zone" in base or (peer.isdigit() and peer != "7001"):
                    sessions.append((tag, c2s, s2c))
        else:
            sessions.append((os.path.basename(t), t, ""))

    facts = []
    for tag, c2s, s2c in sessions:
        f = report_session(tag, c2s, s2c, sizes, names)
        if f:
            facts.append(f)

    print("\n" + "=" * 78)
    print("-- summary over every zone capture scanned --")
    print("%-42s %8s %8s %8s" % ("capture", "CZ_ATTK", "ZC_DMG", "verdict"))
    for f in facts:
        if f["attack"] == 0:
            verdict = "no combat -> question cannot be answered from it"
        elif not f["damage"]:
            verdict = "attack cancelled"
        else:
            verdict = "attack landed"
        print("%-42s %8d %8d %s"
              % (f["capture"].split("/")[-1], f["attack"], f["damage"], verdict))
    total_attack = sum(f["attack"] for f in facts)
    total_dmg = sum(f["damage"] for f in facts)
    print("\nCZ_ATTACK 3666 in every capture scanned: %d" % total_attack)
    print("ZC_ATTACK_* damage packets in every capture scanned: %d" % total_dmg)
    if total_attack == 0:
        print("\nANSWER FROM THE WIRE: the captures hold NO attack request, so they")
        print("cannot say whether the original server lands a swing through a move.")
        print("No capture here contains an attack + move pair - not one.")
    return 0

    print("\nzones scanned: %d, sessions measured: %d" % (len(sessions), len(facts)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv) or 0)
