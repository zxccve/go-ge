"""emit_item_props - item ClassID -> EVERY property the status script reads.

WHY THIS EXISTS
---------------
`out\\lua_corpus\\pc\\get_status_pc.lua` (5,790 lines, 98 `SCR_*` functions) is
the game's own player-status formula. Every term in it is a property read off
the object `GetEquipItem(self, 'SLOT')` returns:

    value = value + armor.IncSTR + rweapon.IncSTR + neck.IncSTR_Opt + ...

`data\\item_stats.json` carries only 18 of the properties those functions read
(AR ASPD ATK ClassType DEF HR IncMHP MHP MSPD RFIRE RICE RLGHT RPSY RSTAT
RedMAG ShldDR WLv Weight) - it was emitted for the Go port's Character Info
sheet, not for the script. The other 57 properties ARE in the client's own
`ge\\xml\\datatable_item_*.xml`; nothing under `data\\` carried them, so a server
that wanted to run the script had no input for them.

This file is a COPY of those columns, emitted once. A zone restart must not
parse 82 MB of XML, and the script must not guess.

THE 75 NAMES ARE MEASURED, NOT CHOSEN
-------------------------------------
The list below was produced by pairing every `local x = GetEquipItem(self,'S')`
with every `x.Field` in `get_status_pc.lua` and taking the union: 75 distinct
property names across 19 slot names (RH LH RCOS LCOS BODY EAR GLOVE BOOTS NECK
BELT RING1 RING2 BACK COSTUME FACE HEAD ACHIEVE ARTIFACT ASSIST). A mechanical
pass finds exactly these 75 and no others, so the list and the script agree.
Keep the ORDER: the emitted row is positional, so index i is COLUMNS[i].

WHY REUSE emit_item_stats AND NOT WRITE A NEW READER
----------------------------------------------------
`emit_item_stats.py` already knows how to open the client's item tables, how to
pull one attribute off one `<Class ... />` tag, and how to turn an empty or
"None" cell into 0. That half is imported here, not copied, so the two files can
never disagree about how a table cell is spelled. Only the COLUMN LIST differs:
18 there, 75 here. The output format differs too (row-oriented there, because
the Go sheet reads one class at a time; column-oriented here, because a Lua
object is bound one property at a time and the next lane will index by name).
`item_stats.json` is NOT touched - the deployed zone parses it, and widening its
shape would break a working feature.

WHICH TABLE WINS WHEN A CLASS APPEARS TWICE
-------------------------------------------
`IncSTR` occurs 25,533 times against 24,975 distinct ClassIDs, so 59 ids are in
two tables. Measured 2026-11-03: every one of the 59 is `datatable_item_etc`
(which carries the whole stat row) together with `datatable_item_lumin` (which
carries ClassID, ClassName, Grade and nothing else). The rule here is:

    FIRST TABLE WINS. `ge_item_index.ITEM_TABLES` order is the precedence,
    and a class already seen is never overwritten by a later table.

So the complete `datatable_item_etc` row wins and the thin `datatable_item_lumin`
row cannot blank it. (`emit_item_stats.py` uses last-wins; for these 59 ids it
therefore drops the stat columns. `item_stats.json` is left alone - the 59 are
lumin gems, not gear.)

GROWING ITEMS - the one place a number is not on the item's own row
-------------------------------------------------------------------
A "Crecemento" item leaves ATK / DEF / WLv 0 on its own row and takes them from
the OWNER'S LEVEL instead. Class 7207 (CrecementoPolearm) is one: its own row
says ATK 0 and `ge\\xml\\datatable_growing_item.xml` says `Value="Lv*2.3 + 80"`.
This is why the owner's Character Info ATK cannot be read straight off the table.

The curve is carried in the output (the `growing` block, the same shape
`item_stats.json` already ships, read by the same parser), so the next lane can
answer at the owner's real level. Because a flat row of integers cannot hold a
formula, the flat value in `classes` is the curve evaluated at the project's
level cap:

    GROW_LEVEL = 100, the cap the client itself applies. `goserver\\zone\\itemstats.go`
    (growLevelCap) records the evidence: ge.exe `FUN_00637151` at 0x637151 does
    `if (100 < lv) lv = 100` before it evaluates Value and Rank. A character
    above 100 therefore still reads the level-100 number, which is what the
    client shows.

Only a column that is 0 on the item's own row is filled, exactly as
`goserver\\zone\\itemstats.go` grow() does: 7217 Leguard's own DEF 2 survives. The
grown Rank (floored, because the table wraps it in math.floor) fills WLv when the
row has none. A reader that wants the owner's REAL level must use the `growing`
block, not the flat number - the flat number is the cap, and for a level-20
character it is too high.

A column the XML does not carry is 0. Never a plausible default.

OUTPUT  `data\\item_props.json`
    {"columns": ["AR", "ASPD", ... 75 names in order],
     "classes": {"<classid>": [v0, v1, ... v74], ...},
     "growing": {"count": N, "items": {"7207": {"column": "ATK", "value_mul": 2.3,
                                                "value_add": 80, "rank_mul": 0.25,
                                                "rank_add": 5}}},
     "source": "...", "note": "..."}

A class whose every value is 0/empty is skipped: it would be 75 zeroes the
script can already treat as "no item here".

Run:  cd docker-port\\src && python emit_item_props.py
"""

import json
import os

import ge_item_index
import emit_item_stats
from ge_paths import DATA, GE

OUT_FILE = os.path.join(DATA, "item_props.json")

# The 75 property names `get_status_pc.lua` reads off a GetEquipItem result, in
# the order measured from the script. The emitted row is positional: index i of
# a row is COLUMNS[i]. Do not re-sort.
COLUMNS = (
    "AR", "ASPD", "ATK", "AddATK", "BLK", "CASTINC", "CRT", "CRTATK", "CRTDEF",
    "ClassID", "ClassName", "ClassType", "DEF", "DefIP", "EvationSkill_BM",
    "FireATK", "FireIP", "HPDrain", "HR", "IGN_DEF", "IGN_DEFCOUNT", "IGN_REG",
    "IGN_REGDEF", "IGN_RSTAT", "IMP", "IceATK", "IceIP", "IgnoreDEFPer_BM",
    "IncAGI", "IncAGI_Opt", "IncCHA", "IncCHA_Opt", "IncCON", "IncCON_Opt",
    "IncDEX", "IncDEX_Opt", "IncINT", "IncINT_Opt", "IncMHP", "IncMSP",
    "IncSTR", "IncSTR_Opt", "IncSklATK", "KDRank", "LghtATK", "LgtIP", "MHP",
    "MKP", "MSATK", "MSAtk", "MSP", "MSPD", "MagicATK_BM", "MeleeATK_BM",
    "PCAtk", "PCDef", "PR", "PsyATK", "PsyIP", "RFIRE", "RHP", "RICE",
    "RLGHT", "RPSY", "RSP", "RSTAT", "RedMAG", "SKL_COOL", "SKL_HEAL",
    "SOCKETWEIGHT", "SetItemGroup", "ShldDR", "ShootATK_BM", "WLv", "Weight",
)

# The two properties the client spells as text. Everything else is a number.
STRING_COLUMNS = frozenset(("ClassName", "ClassType"))

# The level a growing item's curve is asked at, for the flat row. See the module
# docstring: 100 is the client's own cap (ge.exe FUN_00637151 at 0x637151).
GROW_LEVEL = 100


def _text(raw):
    """One text cell: "" for an empty or "None" cell, else the string."""
    value = (raw or "").strip()
    return "" if value == "None" else value


def _read_rows(root=None):
    """Read every item table into (rows, armour, missing).

    rows   {classid: {column: value}} - only the columns the table carries.
    armour the ClassIDs whose Category1 is "Armor", which decides whether a
           growing item fills ATK or DEF (emit_item_stats.build_growing needs it).
    """
    root = root or GE
    rows = {}
    armour = set()
    missing = []
    for table in ge_item_index.ITEM_TABLES:
        path = os.path.join(root, "xml", table + ".xml")
        if not os.path.exists(path):
            # A table list shared with ge_item_index: a missing file is a fact
            # worth printing, not a crash.
            missing.append(table)
            continue
        with open(path, encoding="utf-8", errors="replace") as fh:
            blob = fh.read()
        for tag in emit_item_stats.TAG_RE.findall(blob):
            try:
                class_id = int(emit_item_stats._attr(tag, "ClassID"))
            except ValueError:
                continue
            if emit_item_stats._attr(tag, "Category1").strip() == "Armor":
                armour.add(class_id)
            # FIRST TABLE WINS: see "WHICH TABLE WINS" in the docstring.
            if class_id in rows:
                continue
            row = {}
            for col in COLUMNS:
                raw = emit_item_stats._attr(tag, col)
                if col in STRING_COLUMNS:
                    text = _text(raw)
                    if text:
                        row[col] = text
                else:
                    value = emit_item_stats._num(raw)
                    if value:
                        row[col] = value
            # ClassID is the key itself; the client's own ClassID attribute is
            # the same number, so read it from the key and never from the tag
            # (a tag without a readable ClassID is skipped above).
            row["ClassID"] = class_id
            rows[class_id] = row
    return rows, armour, missing


def _grown(value, rank_mul, rank_add):
    """The flat number a growing item's row carries at GROW_LEVEL.

    Value is NOT floored (the rules that consume it floor their own total); a
    fractional result is kept as a float rather than rounded into a lie. The
    round() is only there to undo binary-float noise: 2.3 * 100 is
    229.99999999999997, and int() on that would answer 309 for the 310 the
    table's own line says. Rank IS floored, because the table writes
    math.floor(...) around it.
    """
    number = round(value, 6)
    if number == int(number):
        return int(number)
    return number


def build(root=None):
    """Read the item tables and the growing curves: (rows, growing, missing)."""
    rows, armour, missing = _read_rows(root)
    # The growing parser is emit_item_stats' own: it reads Value and Rank as a
    # line, then re-checks the parse against the file's own text at Lv 1..200.
    growing = emit_item_stats.build_growing(rows, armour, root)
    for class_id, curve in growing.items():
        row = rows.setdefault(class_id, {"ClassID": class_id})
        level = GROW_LEVEL
        value = _grown(curve["value_mul"] * level + curve["value_add"],
                       curve["rank_mul"], curve["rank_add"])
        # Only a column that is EMPTY is filled. 7217 Leguard's own DEF 2
        # survives; so does any row that really carries its number.
        if not row.get(curve["column"]):
            row[curve["column"]] = value
        if not row.get("WLv"):
            row["WLv"] = int(curve["rank_mul"] * level + curve["rank_add"])
    return rows, growing, missing


def _is_blank(value):
    if isinstance(value, str):
        return value == ""
    return not value


def main():
    rows, growing, missing = build()
    classes = {}
    for class_id in sorted(rows):
        row = rows[class_id]
        values = [row.get(col, "" if col in STRING_COLUMNS else 0)
                  for col in COLUMNS]
        if all(_is_blank(v) for v in values):
            continue
        classes[str(class_id)] = values
    doc = {
        "columns": list(COLUMNS),
        "classes": classes,
        "growing": {
            "count": len(growing),
            "items": {str(k): growing[k] for k in sorted(growing)},
            "note": "a growing item's flat value in 'classes' is its curve at the "
                    "level cap %d; a reader that knows the owner's level should "
                    "prefer this block" % GROW_LEVEL,
        },
        "source": "client xml datatable_item*.xml via ge_item_index.ITEM_TABLES",
        "note": "column-oriented: classes['<id>'][i] is columns[i]. A column the "
                "client leaves empty is 0 ('' for ClassName/ClassType), never "
                "defaulted. First table in ITEM_TABLES order wins for a class "
                "that appears twice.",
    }
    with open(OUT_FILE, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"))
    size = os.path.getsize(OUT_FILE)
    print("wrote %s: %d column(s), %d class(es), %d growing curve(s), %s"
          % (OUT_FILE, len(COLUMNS), len(classes), len(growing), _human(size)))
    blank = len(rows) - len(classes)
    if blank:
        print("skipped %d class(es) whose every value is 0/empty" % blank)
    if missing:
        print("tables not found (%d): %s" % (len(missing), ", ".join(missing)))


def _human(size):
    if size >= 1024 * 1024:
        return "%.2f MB (%d bytes)" % (size / (1024.0 * 1024.0), size)
    if size >= 1024:
        return "%.1f KB (%d bytes)" % (size / 1024.0, size)
    return "%d bytes" % size


if __name__ == "__main__":
    main()
