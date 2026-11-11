"""make_locale_map - build the PERMANENT locale id table, once.

Run from src\\:  python make_locale_map.py

WHAT IT DOES
    Our server's scripts say "say text number 8026".  The client we run only
    knows text numbers like 10137015, because the client renumbered the
    whole locale id space.  So the server has to translate before it sends
    anything, or the client draws a bare number.

    This script produces THE TABLE that does it, and it is PERMANENT: it is
    written to `docker-port\\data\\locale_id_map.json`, which is in git, so
    the server never has to work it out again.

    Two sources, and the second is the one that matters:

      1. MATCHING THE TEXT.  The server pack's dictionary and the client's
         dictionary share 83.6% of their TEXTS even though they share almost
         no ids, so an id whose text exists in both can be translated
         exactly.  That covers 90.4% of the script's tokens.

      2. THE LIVE SERVER.  For the lines the client REWORDED, the text does
         not match - but the live server runs the same generation as the
         client, so the id it sends IS the client's id.  Reading that off a
         capture of the live server (see live_dialog_map.py) covers the rest.
         The live server's answer is the authoritative one.

    This is a ONE-TIME OFFLINE STEP.  The server only ever LOOKS UP the
    table it produces; it never derives anything, and it needs no second
    dictionary at runtime.

    If the table is missing an id, the server leaves the token alone and
    writes the id to `data\\unresolved_ids.txt` - the worklist of ids that
    still need the live server's answer.
"""
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from ge_paths import DATA as DATA_DIR, GAME_ROOT  # noqa: E402, 

GE = os.path.join(GAME_ROOT, "ge")
CLIENT_DICT = os.path.join(GE, "dictionary", "dictionary_local.xml")
SERVERPACK_DICT = os.path.join(GE, "dictionary", "dictionary_local_serverpack.xml")
OUT = os.path.join(DATA_DIR, "locale_id_map.json")

_ENTRY = re.compile(r'<Text\s+ClassID="(\d+)"\s+Text="((?:[^"]|&quot;)*)"\s*/?>')
_ENTITIES = (("&lt;", "<"), ("&gt;", ">"), ("&quot;", '"'),
              ("&apos;", "'"), ("&amp;", "&"))


def _unescape(s):
    for a, b in _ENTITIES:
        s = s.replace(a, b)
    return s


def _parse(path):
    """{id: text} for one dictionary file."""
    out = {}
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        blob = fh.read()
    for cid, text in _ENTRY.findall(blob):
        out[cid] = _unescape(text)
    return out


_pack = None


def pack_table():
    """The SERVER PACK's id -> text. Parsed once.

    Used to find the wording behind an id the client cannot draw: the pack has
    the text, the client has that same text under a different id.
    """
    global _pack
    if _pack is None:
        _pack = _parse(SERVERPACK_DICT) if os.path.exists(SERVERPACK_DICT) else {}
    return _pack


def build():
    client = _parse(CLIENT_DICT)
    pack = _parse(SERVERPACK_DICT)
    # text -> the client's id. The first one wins, so the mapping is stable
    # even where the client has the same string twice.
    by_text = {}
    for cid, text in client.items():
        by_text.setdefault(text, cid)

    # START from what is already there. The table is the permanent record, and
    # its entries include the ids LEARNED from the live server - which this
    # function cannot derive from any file. Deriving first and then adding
    # would silently drop them on every re-run, so the existing entries are
    # the floor and a derivation only ever fills a gap.
    mapping = {}
    if os.path.exists(OUT):
        try:
            with open(OUT, "r", encoding="utf-8") as fh:
                mapping = json.load(fh)
        except Exception as exc:
            print("  could not read the existing table: %s" % exc)

    keep = len(mapping)
    for cid, text in pack.items():
        if cid in mapping:
            continue                    # an answer we already have WINS
        new = by_text.get(text)
        if new and new != cid:
            mapping[cid] = new

    # The live server's answers are the authoritative ones - they override a
    # text match even where both exist. They live in the table already (the
    # tool writes them there), so this only re-asserts the file if it is
    # present as a separate list.
    live_path = os.path.join(DATA_DIR, "live_map.json")
    if os.path.exists(live_path):
        try:
            with open(live_path, "r", encoding="utf-8") as fh:
                live = json.load(fh)
            for old, new in live.items():
                mapping[old] = new
            print("  %d ids from the LIVE server (these win)" % len(live))
        except Exception as exc:
            print("  could not read %s: %s" % (live_path, exc))
    print("  %d entries kept, %d added by text matching" % (keep, len(mapping) - keep))
    return mapping, len(client), len(pack)


def main():
    mapping, n_client, n_pack = build()
    os.makedirs(DATA_DIR, exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as fh:
        json.dump(mapping, fh, ensure_ascii=False, sort_keys=True)
    print("client dictionary  : %d ids" % n_client)
    print("server pack dict  : %d ids" % n_pack)
    print("translations      : %d" % len(mapping))
    print("written           : %s (%.1f KB)"
          % (OUT, os.path.getsize(OUT) / 1024.0))
    print()
    print("This file is PERMANENT and in git.  The server only looks ids up in")
    print("it - it never works them out, and it needs no second dictionary.")


if __name__ == "__main__":
    main()
