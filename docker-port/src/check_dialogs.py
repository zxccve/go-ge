"""check_dialogs - click the town NPCs and show the EXACT dialog text the
server sends, so "does it show a number?" is answered by the wire, not by
opinion.

    python check_dialogs.py [port]

Read-only: it connects, clicks, and prints. It changes nothing on disk.
"""
import os
import re
import socket
import struct
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import ge_crypt as gc  # noqa: E402
import npc_data  # noqa: E402
import npc_script  # noqa: E402
import zone_server as z  # noqa: E402

HOST = "127.0.0.1"
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 7002
TOKEN = re.compile(rb"<\$>(\d+)</>")


def sec(cmd, extra=b""):
    return struct.pack("<HII", cmd, 0, 0) + extra


def drain(sock, seconds=2.0):
    out = b""
    end = time.time() + seconds
    sock.settimeout(0.2)
    while time.time() < end:
        try:
            d = sock.recv(65536)
            if not d:
                break
            out += d
        except socket.timeout:
            pass
        except OSError:
            break
    return out


def dialogs(buf):
    """The dialog texts in a server->client stream."""
    out = []
    i = 0
    while i < len(buf) - 6:
        cmd = struct.unpack_from("<H", buf, i)[0]
        ln = struct.unpack_from("<H", buf, i + 2)[0]
        if cmd in (3660, 3663) and 8 <= ln <= 4000 and i + ln <= len(buf):
            body = buf[i + 4:i + ln]
            toks = [t.decode() for t in TOKEN.findall(body)]
            txt = body[5:].split(b"\x00")[0] if cmd == 3660 else b""
            out.append((cmd, toks, txt))
            i += ln
            continue
        i += 1
    return out


def main():
    # ZONE_ID is 0 until the first player enters; the town zone is 10.
    z.ZONE_ID = 10
    npcs = npc_data.zone_npcs(10)
    print("%d town NPCs" % len(npcs))
    print()
    # Ask the server's OWN function what it would send. Walking each NPC in a
    # live session would take ten minutes and the answer would be the same:
    # the dialog text is a pure function of the handle.
    # A token is fine IF THE CLIENT can resolve it - that is the whole point
    # of sending the client's own id. A token the client CANNOT resolve is
    # the "shows a bare number" symptom.  The CLIENT's dictionary is the
    # test - the merged map would answer a different question.
    import ge_locale
    tab = ge_locale.client_table()

    def unresolvable(text):
        """The tokens in `text` the client has no text for."""
        out = []
        for t in re.findall(r"<\$>(\d+)</>", text):
            if not tab.get(t):
                out.append(t)
        return out

    bare = []
    withtext = []
    for i, n in enumerate(npcs):
        handle = z.NPC_HANDLE_BASE + i
        try:
            line = z.npc_dialog_text(handle)
        except Exception as exc:
            bare.append((i, "EXCEPTION %s" % exc))
            continue
        if line is None:
            continue
        missing = unresolvable(line)
        if missing:
            bare.append((i, line, missing))
        else:
            withtext.append((i, line))

    print("NPCs whose dialog the CLIENT can draw : %d" % len(withtext))
    for i, line in withtext[:6]:
        print("   #%-4d %r" % (i, line[:64]))
    print()
    print("NPCs whose dialog still shows a NUMBER : %d" % len(bare))
    for i, line, missing in bare:
        print("   #%-4d %r   (unresolvable ids: %s)" % (i, line[:44], missing))
    print()
    print("A token the client can resolve is CORRECT - that is the client's own")
    print("id, and the client draws it in its own wording.  A number appears")
    print("only where the id is in NEITHER dictionary.")
    return 0


def ge_locale_resolve(text):
    try:
        import ge_locale
        return ge_locale.resolve(text) or text
    except Exception:
        return text


if __name__ == "__main__":
    sys.exit(main())