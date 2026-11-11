"""ge_locale - send the client the locale ids it can actually draw.

Dialogue text in this port is a locale token such as `<$>8026</>`. The CLIENT
resolves it from its own dictionary when it draws the string, so the server
sends the TOKEN - not the text.

THE PROBLEM THIS SOLVES
    Our scripts are an older server pack, so they quote the OLD text ids.
    The client we run renumbered the whole id space, so it knows only the NEW
    ones - measured: the two dictionaries share 136 ids out of ~190,000 and
    ~210,000, while sharing 83.6% of their TEXTS. An id the client does not
    know is drawn as a bare number.

THE ONE TABLE
    `data\\locale_id_map.json` is the permanent translation table: our old id
    -> the client's id. It is built ONCE by `make_locale_map.py` (which reads
    the server pack's dictionary and the live server) and kept in git, so the
    server NEVER works it out again - it only looks ids up.

    The server's dictionary is the CLIENT's dictionary. The server pack's old
    dictionary was the TOOL's input, not the server's, and the runtime does
    not read it.

AN ID WITH NO TRANSLATION
    Left as the token, so the player sees the number - and the id is written
    to `data\\unresolved_ids.txt`. That file is the worklist of ids that still
    need the live server's answer: a number is a precise error message, not
    noise. (A fallback that hid it was worse than the bug it hid.)

Env:
  GE_LOCALE_DICT        the client's dictionary to read
  GE_LOCALE_ID_MAP      the permanent translation table
  GE_LOCALE_UNRESOLVED  where the worklist of missing ids is written
"""
import json
import os
import re
import sys

from ge_paths import DATA as DATA_DIR, GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))

# The CLIENT's dictionary - the ids the client can actually draw.
DICT = os.environ.get(
    "GE_LOCALE_DICT",
    os.path.join(GAME_ROOT, "ge", "dictionary", "dictionary_local.xml"))

# The permanent translation table: our old id -> the client's id. Written by
# `make_locale_map.py` and kept in git, so the server never derives it.
ID_MAP = os.environ.get(
    "GE_LOCALE_ID_MAP", os.path.join(DATA_DIR, "locale_id_map.json"))

# Where the ids we could not translate are written - the worklist.
UNRESOLVED_FILE = os.environ.get(
    "GE_LOCALE_UNRESOLVED", os.path.join(DATA_DIR, "unresolved_ids.txt"))

# `<$>8026</>` - the client's own token form. Only digits; anything else is
# left alone (a format with arguments is not a plain dictionary lookup).
TOKEN = re.compile(r"<\$>(\d+)</>")

# A text the CLIENT would have to fill in - the client cannot do that, so a
# token whose text carries one must NOT be sent to the client as a token.
_PLACEHOLDER = re.compile(r"%[0-9]*[dsf]")

# One flat <Text .../> entry. Attribute order is ClassID then Text in this file.
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


_table = None
_id_map = None
_reported = set()


def table():
    """The CLIENT's id -> text map. Parsed once per process."""
    global _table
    if _table is None:
        _table = _parse(DICT) if os.path.exists(DICT) else {}
    return _table


def client_table():
    """The client's id -> text map - the same thing under a clearer name.

    There is only ONE dictionary at runtime, so this is `table()`. It exists
    because the question "can the CLIENT draw this id" has to be asked
    against the client's dictionary and nothing else.
    """
    return table()


def id_map():
    """The permanent translation table: our old id -> the client's id."""
    global _id_map
    if _id_map is None:
        _id_map = {}
        if os.path.exists(ID_MAP):
            try:
                with open(ID_MAP, "r", encoding="utf-8") as fh:
                    _id_map = json.load(fh)
            except Exception as exc:
                print("ge_locale: could not read the id map %s: %s"
                      % (ID_MAP, exc), file=sys.stderr)
                _id_map = {}
    return _id_map


def report_unresolved(cid):
    """Record an id the client has no text for, once each."""
    if cid in _reported:
        return
    _reported.add(cid)
    print("ge_locale: id %s is not in the translation table and the client "
          "cannot draw it - it will show as a bare number.  It needs the live "
          "server's id." % cid, file=sys.stderr)
    try:
        os.makedirs(os.path.dirname(UNRESOLVED_FILE), exist_ok=True)
        with open(UNRESOLVED_FILE, "a", encoding="utf-8") as fh:
            fh.write(cid + "\n")
    except Exception as exc:
        print("ge_locale: could not write the worklist %s: %s"
              % (UNRESOLVED_FILE, exc), file=sys.stderr)


def unresolved():
    """The ids we could not translate, read back from the worklist."""
    out = set(_reported)
    if os.path.exists(UNRESOLVED_FILE):
        try:
            with open(UNRESOLVED_FILE, "r", encoding="utf-8") as fh:
                out |= set(l.strip() for l in fh if l.strip())
            _reported |= out
        except Exception:
            pass
    return out


def resolve_for_client(text):
    """Turn a line into something the client can draw.

    The original server sends a LOCALE TOKEN and the client resolves it, so
    that is what this does:

      * the token's id is in the translation table -> send the CLIENT's id,
        so the client resolves it and the player reads the client's own
        wording, exactly like the original server;
      * the id is already the client's own (a caption from the table) ->
        send it unchanged;
      * the client's text for it needs a `%` the client cannot fill in ->
        send that text literally;
      * otherwise -> the id is not translatable, so leave the token (the
        player sees the number) and write the id to the worklist.
    """
    if not text:
        return text
    tab = table()
    imap = id_map()

    def sub(m):
        old = m.group(1)
        new = imap.get(old)
        if new:
            client_text = tab.get(new)
            if client_text and not _PLACEHOLDER.search(client_text):
                return "<$>%s</>" % new
            # the client's text for it needs a % the client cannot fill in
            return client_text
        # already the client's own id? (a caption from the table)
        client_text = tab.get(old)
        if client_text and not _PLACEHOLDER.search(client_text):
            return "<$>%s</>" % old
        # not translatable: report it and leave the token, so the player sees
        # the number and we know exactly which id to fix.
        report_unresolved(old)
        return m.group(0)

    return TOKEN.sub(sub, text)


def resolve(text):
    """Replace every `<$>N</>` in <text> with the client's English line.

    Used where the SERVER must send literal text (a caption the client
    cannot look up, for instance). Most dialogs go through
    `resolve_for_client`, which keeps the token so the client draws it.
    """
    if not text:
        return text
    tab = table()

    def sub(m):
        old = m.group(1)
        new = id_map().get(old, old)
        return tab.get(new, m.group(0))

    return TOKEN.sub(sub, text)


def main():
    """Self-test: report the coverage over every token the NPC index produced."""
    tab = table()
    imap = id_map()
    print("dictionary : %s" % DICT)
    print("entries    : %d" % len(tab))
    print("id map     : %s (%d translations)" % (ID_MAP, len(imap)))
    path = os.path.join(HERE, "npc_script_cache.json")
    if not os.path.exists(path):
        from ge_paths import CACHE as _CACHE
        path = os.path.join(_CACHE, "npc_script_cache.json")
    if not os.path.exists(path):
        print("no script cache at %s - run the zone once first" % path)
        return 0
    with open(path, encoding="utf-8") as fh:
        blob = fh.read()
    toks = set(TOKEN.findall(blob))
    if not toks:
        print("no tokens in %s" % path)
        return 0
    ok = sum(1 for t in toks if t in imap or t in tab)
    print("script tokens: %d   translatable: %d  (%.1f%%)"
          % (len(toks), ok, 100.0 * ok / len(toks)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
