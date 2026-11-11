"""NPC click lines, read from the game's own NPC scripts.

The real server runs each NPC's script when it is clicked: `CTriggerObject`'s
script name comes from the npcgen row's `Script` column, and the function it runs
is `SCR_CLICK_<Script>(self, pc)` in `ge/script/**/*.scp` (see
source2/geDataCheck/source/geTestClass.cpp:377 for the name and the check).
Those scripts are Lua; running them needs the whole script API (quests, shops,
transactions), which this port does not have.

What this module does instead is read the LINE the script would say for the
simple cases, without running anything: it indexes every `SCR_CLICK_<name>` body
and keeps the dialog call it makes.

    ShowOkDlg(pc, "<$>14047</>")                     -> say "<$>14047</>"
    ShowSelDlg(pc, "<$>8602</>", "<$>272</>", ...)   -> say "<$>8602</>" (the
                                                        prompt; the OPTIONS are
                                                        part of the script flow
                                                        and are not run)
    ShowNPCTradeDlg(...)                             -> a shop, not a line

The text is a locale token, not English: the client resolves `<$>NNNN</>` from
its own dictionary when it draws the text, the same way it already resolves the
caption above the NPC. So the right text to send is the TOKEN. That is why the
Terra script set is used and not `ge/script` - the latter is the Korean original
(literal CP949 text), while `ge server folder meriel update/script_export` is the
same tree written with tokens.

The index is cached next to this file; parsing ~4400 files takes seconds, and the
cache is only rebuilt when it is missing (delete it to pick up edited scripts).
"""
import json
import os
import re

from ge_paths import CACHE as CACHE_DIR, GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))
CACHE = os.path.join(CACHE_DIR, "npc_script_cache.json")

# The script tree. `ge\script` is the SERVER's own tree and now holds the
# merged server-side scripts (the meriel overlay was copied into it on
# 2026-10-22, R36), so it is the only tree we need. The old meriel folder is
# still listed second as a fallback for scripts we have not merged yet, but
# `ge\script` wins because the first hit wins.
SCRIPT_TREES = [
    os.path.join(GAME_ROOT, "ge", "script"),
    os.path.join(GAME_ROOT, "ge server folder meriel update", "script_export"),
]

# Bump when the trees change, so the index is rebuilt instead of reused. The
# cache has no other freshness test: it was written once and then kept, so a
# script refresh left the OLD index in place and the server answered clicks
# from a script that no longer exists.
CACHE_VERSION = 3

# Calls that answer with a line of text (what this module can reproduce) ...
TEXT_CALLS = ("ShowOkDlg", "ShowSelDlg", "ShowScnDlg", "ShowScnSelDlg",
              "ShowCommonDlg", "ShowMsgDlg")
# ... and calls that open a window instead (shops and other UIs). Recorded so a
# click can say WHAT the NPC is, instead of pretending it talks.
WINDOW_CALLS = ("ShowNPCTradeDlg", "ShowCustomNPCTradeDlg", "ShowRecipeItemDlg",
                "ShowRefineRingDlg", "ShowSocketItemDlg",
                "ShowExtentionSocketItemDlg", "ShowLuminItemDlg",
                "ShowChangeHeadDlg", "ShowWayPointList", "ShowGigGameDlg",
                "ShowGigGame", "ShowSelectItemDlg",
                # The mission lobby: a click that asks for it opens a WINDOW and
                # says nothing, exactly like a shop (ShowLobby -> ZC_LOBBY_ROOM_LIST).
                "ShowLobby")

_CALL_RE = re.compile(
    r"\b(" + "|".join(TEXT_CALLS + WINDOW_CALLS) + r")\s*\((.*?)\)\s*;", re.S)
_STR_RE = re.compile(r'"(?:[^"\\]|\\.)*"|\'(?:[^\'\\]|\\.)*\'')
_FUNC_RE = re.compile(r"func\s+([A-Za-z0-9_]+)\s*\(.*?\)\s*\{", re.S)
_LOCAL_CALL_RE = re.compile(r"\b(SCR_[A-Za-z0-9_]+)\s*\(")


def _body(text, start):
    """The function body from the brace at `start`, by brace counting."""
    depth = 0
    i = start
    while i < len(text):
        c = text[i]
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return text[start:i]
        i += 1
    return text[start:]


def _first_string(args):
    """The first string literal in a call's arguments, unquoted.

    `IMCFormat("<$>158648</>", reward)` gives back the token; anything the format
    would substitute is dropped, which is fine because those parts are numbers
    and amounts the script computes at runtime.
    """
    m = _STR_RE.search(args)
    if not m:
        return None
    s = m.group(0)[1:-1]
    return s.replace("\\n", "{br}")


def _answer(body, funcs, depth=2):
    """(kind, text, group) for the first dialog this body leads to, or None.

    The FIRST dialog call wins, in the order the script would reach them, so a
    shop that says a line and then opens its window reads as the line (which is
    what the player sees first); a click that only opens a window reads as that
    window. Many click functions are one-line wrappers around another local
    SCR_* function, so those are followed a couple of levels deep.

    `group` is the window call's own first string argument - for a shop that is
    the shop GROUP the item list is looked up by (e.g. ShowNPCTradeDlg(self, pc,
    "REB_MAGIC")), which is what a port needs to build the trade window.
    """
    for call in _CALL_RE.finditer(body):
        kind = call.group(1)
        if kind in TEXT_CALLS:
            s = _first_string(call.group(2))
            if s:
                return ("line", s, None)
        else:
            return ("window", kind, _first_string(call.group(2)))
    if depth <= 0:
        return None
    for m in _LOCAL_CALL_RE.finditer(body):
        sub = funcs.get(m.group(1))
        if sub is not None:
            got = _answer(sub, funcs, depth - 1)
            if got:
                return got
    return None


def _parse(text, out):
    funcs = {}
    for m in _FUNC_RE.finditer(text):
        funcs.setdefault(m.group(1), _body(text, text.index("{", m.end() - 1)))
    for name, body in funcs.items():
        if not name.startswith("SCR_CLICK_") or name[10:] in out:
            continue
        got = _answer(body, funcs)
        if got is None:
            out[name[10:]] = {"kind": "none", "text": "", "group": None}
        else:
            out[name[10:]] = {"kind": got[0], "text": got[1], "group": got[2]}


def build_index():
    """Read every SCR_CLICK_ function in the script trees -> {script: entry}."""
    out = {}
    for tree in SCRIPT_TREES:
        if not os.path.isdir(tree):
            continue
        for root, _dirs, files in os.walk(tree):
            for fn in files:
                if not fn.lower().endswith(".scp"):
                    continue
                try:
                    with open(os.path.join(root, fn), "r",
                              encoding="utf-8", errors="replace") as fh:
                        _parse(fh.read(), out)
                except OSError:
                    continue
    return out


_cache = None


def _trees_stamp():
    """`[scp count, newest mtime]` over the script trees - the freshness test.

    A version CONSTANT alone cannot see a NEWLY ADDED `.scp`: it stayed 4281
    files after a test script was dropped in, so the click answered "nothing to
    say" while the file sat on disk. Stat-ing ~4,300 files costs milliseconds and
    the startup already walks them to build the index.
    """
    count = 0
    newest = 0.0
    for tree in SCRIPT_TREES:
        if not os.path.isdir(tree):
            continue
        for root, _dirs, files in os.walk(tree):
            for fn in files:
                if not fn.lower().endswith(".scp"):
                    continue
                try:
                    newest = max(newest, os.path.getmtime(os.path.join(root, fn)))
                except OSError:
                    continue
                count += 1
    return [count, round(newest, 3)]


def index():
    """The script index, from the JSON cache when it exists AND is current."""
    global _cache
    if _cache is not None:
        return _cache
    stamp = _trees_stamp()
    try:
        with open(CACHE, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        # A cache written before the scripts were refreshed still holds the
        # OLD index, and the server then answers a click from a script that
        # no longer exists. The version is the freshness test.
        if (isinstance(data, dict) and data.get("_version") == CACHE_VERSION
                and data.get("_stamp") == stamp):
            _cache = data
            return _cache
    except (OSError, ValueError):
        pass
    _cache = build_index()
    _cache["_version"] = CACHE_VERSION
    _cache["_stamp"] = stamp
    try:
        with open(CACHE, "w", encoding="utf-8") as fh:
            json.dump(_cache, fh, ensure_ascii=False, sort_keys=True)
    except OSError:
        pass
    return _cache


def entry(script):
    """The script's click entry, or None."""
    if not script:
        return None
    return index().get(script)


def line_for(script):
    """The line `<script>`'s click function says, or None when it does not talk.

    None means either there is no click script at all or the click opens a window
    (shop, socket, waypoint, face change); `window_for` tells those apart.
    """
    e = entry(script)
    return e["text"] if e and e["kind"] == "line" else None


def window_for(script):
    """The window the click opens (e.g. 'ShowNPCTradeDlg'), or None."""
    e = entry(script)
    return e["text"] if e and e["kind"] == "window" else None


def group_for(script):
    """A window call's first string argument (a shop GROUP), or None."""
    e = entry(script)
    return e.get("group") if e else None


_cache_funcs = None


def _funcs_of_script(script):
    """All functions of the file that defines SCR_CLICK_<script>, or None."""
    global _cache_funcs
    if _cache_funcs is None:
        _cache_funcs = {}
    if script in _cache_funcs:
        return _cache_funcs[script]
    for tree in SCRIPT_TREES:
        if not os.path.isdir(tree):
            continue
        for root, _dirs, files in os.walk(tree):
            for fn in files:
                if not fn.lower().endswith(".scp"):
                    continue
                try:
                    with open(os.path.join(root, fn), "r", encoding="utf-8",
                              errors="replace") as fh:
                        text = fh.read()
                except OSError:
                    continue
                if ("func SCR_CLICK_" + script + "(") not in text:
                    continue
                funcs = {}
                for m in _FUNC_RE.finditer(text):
                    funcs.setdefault(m.group(1),
                                     _body(text, text.index("{", m.end() - 1)))
                _cache_funcs[script] = funcs
                return funcs
    _cache_funcs[script] = None
    return None


def _find_call(body, funcs, names, depth=2):
    """The first call to any of `names` in this body (or a local SCR_* helper).

    Separate from `_answer` on purpose: a shop NPC's SCR_CLICK_ usually starts
    with a `ShowSelDlg` prompt ("would you like to buy?") and only then calls
    `ShowNPCTradeDlg`, so looking for the FIRST dialog call finds the prompt, not
    the shop.
    """
    for m in re.finditer(r"\b(" + "|".join(names) + r")\s*\((.*?)\)\s*;", body, re.S):
        return (m.group(1), m.group(2))
    if depth <= 0:
        return None
    for m in _LOCAL_CALL_RE.finditer(body):
        sub = funcs.get(m.group(1))
        if sub is not None:
            got = _find_call(sub, funcs, names, depth - 1)
            if got:
                return got
    return None


def trade_for(script):
    """(call, shop_group) when this click leads to a SHOP, else None.

    The shop GROUP is what the item list is looked up by, in the game's own
    `datatable_Shop` table (GroupName -> items) - see the server's
    ShowNPCTradeDlg, which builds the trade packet from exactly that.
    """
    if not script:
        return None
    funcs = _funcs_of_script(script)
    if not funcs:
        return None
    body = funcs.get("SCR_CLICK_" + script)
    if body is None:
        return None
    got = _find_call(body, funcs, ("ShowNPCTradeDlg", "ShowCustomNPCTradeDlg",
                                   "ShowRecipeItemDlg"))
    if not got:
        return None
    return (got[0], _first_string(got[1]))


if __name__ == "__main__":
    import sys
    idx = index()
    print("%d click scripts indexed (%s)" % (len(idx), "cache" if os.path.exists(CACHE) else "fresh"))
    for s in sys.argv[1:]:
        print("  %-28s %s" % (s, idx.get(s)))
