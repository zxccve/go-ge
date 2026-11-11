"""emit_item_stats - item ClassID -> the columns the status RULES read.

WHY THIS EXISTS
---------------
The Character Info sheet's derived numbers (ATK, DEF, A.R., D.R., HR, CSPD, the
five resistances) are NOT computed by the client: it prints whatever
ZC_UPDATE_ALL_STATUS 3684 put in its record (proved in `history\\zone-entry.md`).
The game's own scripts compute them, and every one of those scripts reads ITEM
columns - `SCR_Get_CATK` (`ge\\script\\pc\\get_status_pc.scp:944`) starts from
`rweapon.ATK`, `SCR_Get_DEF` (`:1126`) from the worn `DEF`, `SCR_Get_AR`
(`:565`) from `WLv`/`AR`, the resistances (`:2105`..`:2292`) from `RFIRE` ..
`RPSY`. Those columns live in the client's own `ge\\xml\\datatable_item_*.xml`
and nothing under `data\\` carried them, so the zone had no input at all.

This file is a COPY of the client's columns, emitted once. A zone restart must
not parse 82 MB of XML.

WHY A NEW SCRIPT (the project rule says reuse first)
----------------------------------------------------
`emit_item_equip.py` reads the same 27 tables and keeps ONE column
(DefaultEqpSlot); `ge_item_index.py` keeps ClassName -> ClassID;
`emit_item_catalog.py` keeps the name catalog and the stack rule. None of them
carries a stat column, and widening a shipped format for a second reader would
change it under the first one. This reuses `ge_item_index.ITEM_TABLES` so the
four files cannot disagree about WHICH tables are the Item idspace.

COLUMNS, and the rules that read them
-------------------------------------
  ATK DEF AR ShldDR WLv   weapon/armour numbers (SCR_Get_CATK/AR/DR)
  MHP IncMHP              the max-HP gear terms (SCR_Get_MHP)
  RFIRE RICE RLGHT RSTAT RPSY RedMAG   the resistance rules
  Weight                  the Burlon-point rule reads it
  MSPD                    the walk speed: a MULTIPLIER (1 neutral, 0.95 slower,
                          1.05 faster) on the eight worn item categories
                          SCR_Get_MSPD multiplies into its 469 base
  ClassType               "Sword", "ArmShield", "Rosario", "Shield" ... - the
                          stance/weapon branches and SCR_GET_STATANDWEAPON:3760
                          switch on this NAME, so it is kept as a string.

A column the client spells as an empty string or "None" is written as 0 (or ""
for ClassType). Nothing is defaulted to a plausible number: a value that is not
in the table stays absent, and the Go side then sends 0 and says so.

GROWING ITEMS (the second half of the file)
-------------------------------------------
A growing item ("Crecemento") leaves ATK / DEF / WLv EMPTY on its own row and
takes them from the owner's LEVEL instead. Its numbers are in
`ge\\xml\\datatable_growing_item.xml`, 45 rows, one per item ClassID, and each
row is two small expressions over `Lv`:

    <Class ClassID="1" ItemID="7201" Rank="math.floor(5 + Lv/4)"
           Value="Lv*1.7 + 55" />

  Value  the item's own grown number - the ATK of a weapon, the DEF of armour
         or a shield (see GROWING_COLUMN below);
  Rank   the grown WLv, which is what SCR_Get_AR (`get_status_pc.scp:565`) and
         SCR_Get_DR (`:632`) add on top of floor(Lv/4).

Both are printed as a LINE (a*Lv + b) rather than one level's value, so the zone
can answer at the owner's real level instead of a fixed 100. The game's own item
text says the same thing in words: item 7201's description (dictionary ClassID
10047079) is "Sword that is decided the A.R. and ATK Power according to the
player's level", and 15001's (10017601) is "Leather Armor that is decided the
D.R. and DEF Power according to the player's level".

Output: `data\\item_stats.json`
    {"count": N, "items": {"101": {"ATK": 89, "DEF": 0, ..., "ClassType": "Sword"},
                           ...},
     "growing": {"count": M, "items": {"7201": {"column": "ATK",
                                                "value_mul": 1.7, "value_add": 55,
                                                "rank_mul": 0.25, "rank_add": 5}}},
     "source": "...", "note": "..."}

Run:  cd docker-port\\src && python emit_item_stats.py
"""

import json
import math
import os
import re

import ge_item_index
from ge_paths import DATA, GE

OUT_FILE = os.path.join(DATA, "item_stats.json")

# The growing-item table: one row per growing item ClassID, each a Rank and a
# Value expression over Lv. See the module docstring.
GROWING_FILE = os.path.join(GE, "xml", "datatable_growing_item.xml")

# Which item column a growing Value fills, when the item's own row is silent.
# The evidence is the shipped item text plus the item's own Category1:
#   Category1 "Armor"  -> DEF   (15001..15004: "the D.R. and DEF Power")
#   ClassType "Shield" -> DEF   (7215: "a Shield that increases DEF according to
#                                the level of character"; it is GroupName Weapon,
#                                so Category1 alone does NOT separate it)
#   anything else      -> ATK   (7201 "the A.R. and ATK Power", 7235 ArmShield
#                                "increases A.R. and ATK Power", 7217 Leguard
#                                "the A.R. and ATK Power [DEF +2]" - and note
#                                7217 KEEPS its own DEF 2, which is why the Go
#                                side only fills a column that is empty)
# The two rows whose Value is `Lv*0` (7230 Tool, 7231 Controller, 7232 Pendant)
# answer 0 whichever column this picks, and their own text says "No difference
# according to the level of character".
SHIELD_TYPE = "Shield"

# The numeric columns, in the order the game's scripts read them. ClassType is
# the one string and is handled separately.
NUM_COLUMNS = (
    "ATK", "DEF", "AR", "ShldDR", "WLv",
    "MHP", "IncMHP",
    "RFIRE", "RICE", "RLGHT", "RSTAT", "RPSY", "RedMAG",
    "Weight",
    # HR and ASPD are NOT in ROADMAP R61b's column list but its rules read them:
    # SCR_Get_CHR (:1277) adds rweapon.HR, glove.HR, ring1.HR ... and
    # SCR_Get_CSPD (:1613) builds itemfix from stance/item ASPD. Without the two
    # columns HR and CSPD could only be sent as 0, so they are emitted too.
    "HR", "ASPD",
    # MSPD is the WALK-SPEED MULTIPLIER, and it is a MULTIPLIER, not a
    # percentage: 1 is neutral, 0.95 is slower, 1.05 is faster.
    # SCR_Get_MSPD (get_status_pc.scp:1808) multiplies the eight worn
    # categories' MSPD columns into its 469 base:
    #   469 * stance.StnMSPD * armor * boots * costume * face * back
    #       * belt * Achieve * artifact
    # Only those eight families are read from this column, but it is emitted
    # for every item the tables carry it on - the reader picks the slots.
    # Measured on the shipped tables: every row of all eight families has the
    # column (0 of 83 boots rows lack it), so a row without it is OUR gap and
    # the Go side reads that as the neutral 1, never as 0.
    "MSPD",
)

TAG_RE = re.compile(r"<Class\s[^>]*?/?>")


def _attr(tag, name):
    m = re.search(r'\b%s="([^"]*)"' % name, tag)
    return m.group(1) if m else ""


def _num(raw):
    """One numeric cell: int when it is a whole number, else a float, else 0.

    The tables write integers ("89"), but a few columns are fractional ("1.10
    " with a trailing space, which int() rejects), so both are parsed and an
    unreadable cell is 0 - never a guessed value.
    """
    text = (raw or "").strip()
    if not text or text == "None":
        return 0
    try:
        return int(text)
    except ValueError:
        pass
    try:
        value = float(text)
    except ValueError:
        return 0
    if value == int(value):
        return int(value)
    return value


def build(root=None):
    """Read the client's item tables and return (rows, missing_tables, armour)."""
    root = root or GE
    rows = {}
    missing = []
    armour = set()
    for table in ge_item_index.ITEM_TABLES:
        path = os.path.join(root, "xml", table + ".xml")
        if not os.path.exists(path):
            # A table list shared with ge_item_index: a missing file is a fact
            # worth printing, not a crash.
            missing.append(table)
            continue
        with open(path, encoding="utf-8", errors="replace") as fh:
            blob = fh.read()
        for tag in TAG_RE.findall(blob):
            try:
                class_id = int(_attr(tag, "ClassID"))
            except ValueError:
                continue
            row = {}
            for col in NUM_COLUMNS:
                value = _num(_attr(tag, col))
                if value:
                    row[col] = value
            class_type = _attr(tag, "ClassType").strip()
            if class_type and class_type != "None":
                row["ClassType"] = class_type
            rows[class_id] = row
            if _attr(tag, "Category1").strip() == "Armor":
                armour.add(class_id)
    return rows, missing, armour


# One term of a growing expression. Every row of the table is a sum of these
# shapes, in either order, and nothing else - anything unexpected RAISES, so a
# row that this parser cannot read can never be written as a silent 0.
_TERM_RE = (
    (re.compile(r"^Lv$"), "lv1"),
    (re.compile(r"^Lv\s*\*\s*([0-9.]+)$"), "lv_mul"),
    (re.compile(r"^([0-9.]+)\s*\*\s*Lv$"), "lv_mul"),
    (re.compile(r"^([0-9.]+)\s*\*\s*Lv$"), "lv_mul"),
    (re.compile(r"^Lv\s*/\s*([0-9.]+)$"), "lv_div"),
    (re.compile(r"^([0-9.]+)$"), "const"),
)


def parse_linear(expr, where):
    """Turn "Lv*1.7 + 55" / "math.floor(5 + Lv/4)" into (mul, add).

    The table prints a formula, not a value, so the zone needs its two
    coefficients. `where` names the row in the error message.
    """
    text = (expr or "").strip()
    if not text:
        raise ValueError("%s: empty expression" % where)
    inner = re.fullmatch(r"math\.floor\((.*)\)", text)
    floor = bool(inner)
    if floor:
        text = inner.group(1).strip()
    mul, add = 0.0, 0.0
    for raw in text.split("+"):
        term = raw.strip()
        for pattern, kind in _TERM_RE:
            m = pattern.match(term)
            if not m:
                continue
            if kind == "lv1":
                mul += 1.0
            elif kind == "lv_mul":
                mul += float(m.group(1))
            elif kind == "lv_div":
                divisor = float(m.group(1))
                if divisor == 0:
                    raise ValueError("%s: division by zero in %r" % (where, expr))
                mul += 1.0 / divisor
            else:
                add += float(m.group(1))
            break
        else:
            raise ValueError("%s: cannot read the term %r of %r" % (where, term, expr))
    return mul, add, floor


def check_linear(expr, mul, add, floor, where):
    """Re-evaluate the ORIGINAL string at Lv 1..200 and compare with (mul, add).

    A restricted eval of the shipped file's own text: `math` and `Lv` are the
    only names it can see, and a mismatch means the parser above is wrong.
    """
    scope = {"math": math, "Lv": 0}
    for level in range(1, 201):
        scope["Lv"] = level
        want = eval(expr, {"__builtins__": {}}, scope)  # noqa: S307 - shipped file
        got = mul * level + add
        if floor:
            got = math.floor(got)
        if abs(want - got) > 1e-9:
            raise ValueError("%s: at Lv %d the expression %r is %r but the stored "
                             "line gives %r" % (where, level, expr, want, got))


def build_growing(rows, armour, root=None):
    """Read datatable_growing_item.xml into {item ClassID: curve}."""
    root = root or GE
    path = os.path.join(root, "xml", "datatable_growing_item.xml")
    if not os.path.exists(path):
        raise SystemExit("%s is missing - the growing items would silently stay 0"
                         % path)
    with open(path, encoding="utf-8", errors="replace") as fh:
        blob = fh.read()
    out = {}
    for tag in TAG_RE.findall(blob):
        item_id = _attr(tag, "ItemID").strip()
        if not item_id:
            continue
        where = "%s ClassID=%s ItemID=%s" % (
            os.path.basename(path), _attr(tag, "ClassID"), item_id)
        class_id = int(item_id)
        value_mul, value_add, _ = parse_linear(_attr(tag, "Value"), where + " Value")
        rank_mul, rank_add, rank_floor = parse_linear(
            _attr(tag, "Rank"), where + " Rank")
        check_linear(_attr(tag, "Value"), value_mul, value_add, False, where + " Value")
        check_linear(_attr(tag, "Rank"), rank_mul, rank_add, rank_floor, where + " Rank")
        class_type = rows.get(class_id, {}).get("ClassType", "")
        column = "ATK"
        if class_id in armour or class_type == SHIELD_TYPE:
            column = "DEF"
        out[class_id] = {
            "column": column,
            "value_mul": value_mul,
            "value_add": value_add,
            "rank_mul": rank_mul,
            "rank_add": rank_add,
        }
    return out


def main():
    rows, missing, armour = build()
    growing = build_growing(rows, armour)
    doc = {
        "count": len(rows),
        "items": {str(k): rows[k] for k in sorted(rows)},
        "growing": {
            "count": len(growing),
            "items": {str(k): growing[k] for k in sorted(growing)},
            "source": "ge\\xml\\datatable_growing_item.xml (Value and Rank read as "
                      "a*Lv + b, then re-checked against the file's own string at "
                      "Lv 1..200)",
            "note": "'column' is the item column a growing item FILLS at the "
                    "owner's level: ATK for a weapon, DEF for armour or a shield. "
                    "'rank_*' is the grown WLv (A.R./D.R. term). A column the "
                    "item's own row already carries is never overwritten",
        },
        "source": "client xml datatable_item*.xml via ge_item_index.ITEM_TABLES",
        "note": "one row per item ClassID, only the non-zero stat columns; "
                "a column the client leaves empty is absent, never defaulted",
    }
    with open(OUT_FILE, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"))
    print("wrote %s: %d plain item class(es)" % (OUT_FILE, len(rows)))
    print("wrote %s: %d growing item(s)" % (OUT_FILE, len(growing)))
    # The walk speed's item term. A class whose MSPD is 1 is NEUTRAL (the
    # client's default), so only the ones that move the number matter to a
    # reader - both numbers are printed, because "0 non-neutral" would
    # otherwise be indistinguishable from "the column was never read".
    mspd_known = [c for c, row in rows.items() if "MSPD" in row]
    mspd_off = [c for c in mspd_known if rows[c]["MSPD"] != 1]
    print("MSPD: %d class(es) carry the column, %d of them are NOT neutral (1.0)"
          % (len(mspd_known), len(mspd_off)))
    if missing:
        print("tables not found (%d): %s" % (len(missing), ", ".join(missing)))


if __name__ == "__main__":
    main()
