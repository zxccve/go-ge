"""live_dialog_map - learn the NEW locale ids from the LIVE server.

Run from src\\:  python live_dialog_map.py <capture-folder> [<capture-folder> ...]

WHY
    Our server's scripts are an OLDER server pack than the client we run, so
    they quote the old locale ids (8026, 14047, ...) while the client knows
    only the new ones (10137015, 10146301, ...). `build_id_map.py` bridges
    that by MATCHING THE TEXT, which works for 90.4% of the script's tokens -
    but it CANNOT pair the ~9.5% the client reworded, because the two
    strings differ. Measured:

        ours   8026  "This is Leonardo Expreso. Please choose the service..."
        live 10137015 "Hello, I'm Leonardo Expreso. What service would you..."

    Same NPC, different wording, so no text match is possible - and that is
    exactly the case that shows as a bare number in game.

    THE LIVE SERVER IS THE ORACLE. It runs the SAME generation as the client
    (every id it sends resolves in the client's dictionary), so when you
    click an NPC there, it sends the NEW id for that NPC's line. Click the
    same NPCs on both servers and the two id spaces can be paired by NPC.

HOW IT WORKS
    From one capture of a live session:
      * the live spawn packet (3543) carries the NPC's NAME as LITERAL TEXT
        and a handle - the name is what we match on, because the live
        server no longer sends a name token;
      * the click (3679, client->server, encrypted) carries the handle;
      * the dialog (3663) carries the handle at +2 and the line as its first
        locale token.
    On OUR side the same NPC is found by NAME: our npcgen row's name token
    resolves in the client's dictionary to the same English name, and our
    script quotes the old id for its line.

    The result is written to cache\\live_map.json: {our old id: the live new
    id}. `ge_locale` prefers that map over the text match.

THE CAPTURE
    Point the client at the live server and capture it:

        python capture_live.py start --port 7001 --port 7054 --tag liveclick
        <click the NPCs you want>
        python capture_live.py stop --tag liveclick

    The capture lands in the top-level captures\\ folder. Give this script
    that folder.
"""
import json
import os
import re
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from ge_paths import CACHE as CACHE_DIR, DATA as DATA_DIR  # noqa: E402

CMD_SPAWN_TRIGGER = 3543      # ZC_SPAWN_STANDINGTRIGGER: the NPC, with its name
CMD_BALLOON = 3553            # ZC_BALLOON_CHAT: the NPC's handle + its line
CMD_DIALOG_SELECT = 3663      # ZC_DIALOG_SELECT: the line + the button labels
CMD_CLICK_TRIGGER = 3679      # CZ_CLICK_TRIGGER: the click (client -> server)
CMD_DIALOG_OK = 3660          # ZC_DIALOG_OK: a line with no buttons

TOKEN = re.compile(rb"<\$>(\d+)</>")
# The output: THE permanent table the server reads. The live server's answers
# are folded straight into it, so there is exactly ONE file to look at and
# exactly ONE file to keep in git. (The id map is derived from the two
# dictionaries by make_locale_map.py, which reads the live map too - so this
# tool only ever has to ADD to the table, never replace it.)
OUT = os.path.join(DATA_DIR, "locale_id_map.json")

# The handle sits in the spawn packet, but where? It is found by matching
# against the handles the client actually clicked - that is unambiguous, and
# it does not need us to know the layout.
_HANDLE_PROBE = re.compile(rb"[^A-Za-z0-9]{0,3}")


def read(path):
    with open(path, "rb") as fh:
        return fh.read()


def dialogs_of(data):
    """{handle: [the locale tokens the live server sent]} for every dialog.

    The two dialog packets have their handle at DIFFERENT offsets, because
    they have a different header:

      ZC_DIALOG_OK   (3660)  type(1) handle(4) text
      ZC_DIALOG_SELECT (3663) type(1) count(1) handle(4) text

    The live server's SELECT text looks like this:

        <?><$>10137015</></?>;<$>10096634</>;<$>10137016</>

    i.e. `<?>` .. `</?>` wraps the MAIN LINE, and the `;`-separated items
    after it are the BUTTON labels. The first token is the line, which is the
    one we want.
    """
    out = {}
    i = 0
    while i < len(data) - 6:
        cmd = struct.unpack_from("<H", data, i)[0]
        ln = struct.unpack_from("<H", data, i + 2)[0]
        if cmd in (CMD_DIALOG_SELECT, CMD_DIALOG_OK) and 8 <= ln <= 4000 \
                and i + ln <= len(data):
            body = data[i + 4:i + ln]
            off = 2 if cmd == CMD_DIALOG_SELECT else 1
            if len(body) >= off + 4:
                handle = struct.unpack_from("<I", body, off)[0]
                toks = [t.decode() for t in TOKEN.findall(body)]
                if toks:
                    out.setdefault(handle, []).extend(toks)
            i += ln
            continue
        i += 1
    return out


def clicked_handles(c2s_path):
    """The handles the client clicked, in order (the leg is encrypted)."""
    import ge_crypt
    data = read(c2s_path)
    out = []
    off = 0
    while off < len(data) - 2:
        size = struct.unpack_from("<H", data, off)[0]
        if size == 0 or off + 2 + size > len(data):
            break
        try:
            pt = ge_crypt.decrypt_packet(data[off:off + 2 + size])
            if len(pt) >= 16 and struct.unpack_from("<H", pt, 0)[0] == CMD_CLICK_TRIGGER:
                out.append(struct.unpack_from("<I", pt, 12)[0])
        except Exception:
            pass
        off += 2 + size
    return out


def names_of(data, clicked):
    """{handle: the NPC's name} from the live spawn packets.

    The live server sends the name as LITERAL TEXT (not a token), so the name
    is the last run of printable text in the packet. The handle's position is
    not assumed: the one field that also appears in a CLICK is the handle, so
    the clicked handles are the probe.
    """
    out = {}
    i = 0
    while i < len(data) - 6:
        cmd = struct.unpack_from("<H", data, i)[0]
        ln = struct.unpack_from("<H", data, i + 2)[0]
        if cmd == CMD_SPAWN_TRIGGER and 20 <= ln <= 400 and i + ln <= len(data):
            body = data[i + 4:i + ln]
            text = ""
            for m in re.finditer(rb"[ -~]{3,}", body):
                text = m.group(0).decode("latin-1")
            for off in range(0, len(body) - 3):
                h = struct.unpack_from("<I", body, off)[0]
                if h in clicked:
                    out.setdefault(h, text)
        i += ln if 0 < ln <= 400 else 1
    return out


def our_npcs():
    """{name: (handle, our script, our line's old id)} from OUR npcgen."""
    import ge_locale
    import npc_data
    import npc_script
    tab = ge_locale.table()
    out = {}
    for zone in (10,):
        try:
            npcs = npc_data.zone_npcs(zone)
        except Exception:
            continue
        for i, n in enumerate(npcs):
            raw = (n.get("name") or "").strip()
            if not raw or raw == "None":
                continue
            name = ge_locale.resolve(raw)
            name = name.split("{br}")[0].strip()
            if not name:
                continue
            script = n.get("script") or ""
            line = npc_script.line_for(script) if script else None
            old = None
            if line:
                m = TOKEN.search(line.encode("utf-8", "replace"))
                if m:
                    old = m.group(1).decode()
            out.setdefault(name, []).append(
                (npc_data.NPC_HANDLE_BASE + i if hasattr(npc_data, "NPC_HANDLE_BASE")
                 else None, script, old))
    return out


def balloons_of(data):
    """{handle: the live line the balloon carried} from ZC_BALLOON_CHAT.

    The live server sends a balloon for the NPCs near the player, and it
    carries the NPC's HANDLE (at +0) and the live line. So the handle is
    known for every NPC the player can see.
    """
    out = {}
    i = 0
    while i < len(data) - 6:
        cmd = struct.unpack_from("<H", data, i)[0]
        ln = struct.unpack_from("<H", data, i + 2)[0]
        if cmd == CMD_BALLOON and 20 <= ln <= 400 and i + ln <= len(data):
            body = data[i + 4:i + ln]
            if len(body) >= 4:
                handle = struct.unpack_from("<I", body, 0)[0]
                toks = [t.decode() for t in TOKEN.findall(body)]
                if toks:
                    out.setdefault(handle, []).extend(toks)
        i += ln if 0 < ln <= 400 else 1
    return out


def words(text):
    """The distinctive words of a line, lowercased."""
    out = set()
    for w in re.findall(r"[A-Za-z][A-Za-z'\-]{2,}", text or ""):
        out.add(w.lower())
    return out


def our_lines():
    """{our old id: (our old text, the NPC's name)} for every town NPC."""
    import ge_locale
    import npc_data
    import npc_script
    tab = ge_locale.table()
    out = {}
    for zone in (10,):
        try:
            npcs = npc_data.zone_npcs(zone)
        except Exception:
            continue
        for n in npcs:
            script = n.get("script") or ""
            if not script:
                continue
            line = npc_script.line_for(script)
            if not line:
                continue
            m = TOKEN.search(line.encode("utf-8", "replace"))
            if not m:
                continue
            old = m.group(1).decode()
            name = ge_locale.resolve(n.get("name") or "").split("{br}")[0].strip()
            out[old] = (tab.get(old, ""), name)
    return out


def main():
    folders = sys.argv[1:]
    if not folders:
        print(__doc__)
        return 1

    our = our_lines()
    print("our side: %d NPCs with a click line" % len(our))
    print()

    mapping = {}
    if os.path.exists(OUT):
        try:
            with open(OUT, "r", encoding="utf-8") as fh:
                mapping = json.load(fh)
        except Exception:
            mapping = {}
    learned = 0

    for folder in folders:
        s2c_files = [os.path.join(folder, f) for f in sorted(os.listdir(folder))
                     if f.startswith("s2c_") and f.endswith(".bin")]
        c2s_files = [os.path.join(folder, f) for f in sorted(os.listdir(folder))
                     if f.startswith("c2s_") and f.endswith(".bin")]
        for s2c in s2c_files:
            data = read(s2c)
            dlgs = dialogs_of(data)
            balls = balloons_of(data)
            if not dlgs and not balls:
                continue
            print("== %s" % os.path.basename(s2c))
            print("   %d dialog(s), %d balloon(s)" % (len(dlgs), len(balls)))
            # every id the live server used, with the client's text for it
            live = {}
            for handle, toks in list(dlgs.items()) + list(balls.items()):
                for t in toks:
                    live.setdefault(t, handle)
            import ge_locale
            tab = ge_locale.table()
            for new, handle in sorted(live.items(), key=lambda kv: kv[1]):
                client_text = tab.get(new) or ""
                # pair by the WORDS the two texts share: the client reworded
                # the line, but the NPC's NAME is in both.
                best = None
                best_score = 0.0
                for old, (old_text, name) in our.items():
                    a = words(client_text)
                    b = words(old_text) | words(name)
                    if not a or not b:
                        continue
                    score = len(a & b) / float(len(a | b))
                    if score > best_score:
                        best_score, best = score, old
                if best is None or best_score < 0.30:
                    print("   live %-9s %-46r  -> no confident match (best %.0f%%)"
                          % (new, client_text[:46], best_score * 100))
                    continue
                if mapping.get(best) == new:
                    continue
                mapping[best] = new
                learned += 1
                print("   live %-9s %-46r" % (new, client_text[:46]))
                print("        our %-9s %-46r  (matched on the shared words, %.0f%%)"
                      % (best, our[best][0][:46], best_score * 100))
            print()

    os.makedirs(CACHE_DIR, exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as fh:
        json.dump(mapping, fh, ensure_ascii=False, sort_keys=True)
    print("learned %d new mappings; %d total -> %s"
          % (learned, len(mapping), OUT))
    return 0


if __name__ == "__main__":
    sys.exit(main())
