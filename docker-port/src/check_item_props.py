"""check_item_props - is `data\\item_props.json` usable by the status script?

Why this exists
---------------
`emit_item_props.py` writes 75 properties per item class. A wrong emitter does
not crash: it writes 75 zeroes, and the status script then happily computes
ATK 0 for a valid weapon. So the file is checked the way it will be USED:

  (a) per property, how many classes carry a NON-ZERO value. A property that is
      0 on every class is a column this emitter never really read - the exact
      failure `emit_item_stats.py` was written to avoid for its 18 columns.
  (b) the WHOLE row of the three worn classes 15001 / 7207 / 19533, because a
      spot check cannot spot a shifted index and a row can. 7207 is the worn
      weapon whose ATK the Character Info sheet depends on; 15001 is growing
      armour (DEF); 19533 is ordinary boots (WLv).
  (c) the WITNESS rule: every one of the 18 columns that the OLDER, independent
      emitter `data\\item_stats.json` also carries must AGREE with it, class by
      class. Two emitters reading the same XML with different authors is the only
      cross-check this project has that a column is neither missing nor shifted:
      one wrong index moves 4.7 MB of numbers and no spot check sees it.
  (d) the emitted file's size, so "it exists" is a number and not a claim.

WHY THE WITNESS RULE REPLACED "the ten CORE properties must be non-zero on one
of the three worn classes": that rule was FALSIFIED BY THE DATA. AR, HR, ASPD,
ShldDR and MHP are honestly 0 on all three of those items - the client's own
table says so, and item_stats.json, an independent reading of the same XML,
agrees on all five. A rule that exits 1 on true data trains a reader to ignore
the checker.

THE ONE ALLOWED DIFFERENCE is class 500001, and it is named in ALLOWED below with
its reason: the two emitters rank a DUPLICATE class id differently, so that one
row is a different item in each file.

Exit code is 1 when:
  * fewer than 60 of the 75 names carry a non-zero value anywhere - the emitter
    has stopped reading the tables, or
  * any of the 18 witness columns DISAGREES with item_stats.json on any class,
    other than the named allowance - the two readings of the same XML differ, so
    at least one of them is wrong.
It is also 1 when the file's shape is wrong (not 75 columns, a row of the wrong
length), because a shape error makes the index meaningless, and when the witness
file itself is missing - the cross-check is the point.

Usage:  cd docker-port\\src && python check_item_props.py
"""

import json
import os
import sys

from ge_paths import DATA

# The same list the emitter writes, spelled the same way. Kept as a literal on
# purpose: a checker that reads its expectations from the file it checks can
# never disagree with it.
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

STRING_COLUMNS = frozenset(("ClassName", "ClassType"))

# The properties the owner's Character Info sheet cannot go without. Kept as a
# list to PRINT (the row dump below shows them with their values); it is no longer
# a pass/fail rule - see the module docstring for why.
CORE = ("ATK", "DEF", "AR", "HR", "ASPD", "WLv", "ShldDR", "ClassType",
        "Weight", "MHP")

# The 18 columns `item_stats.json` also carries. They are the witness: two
# independent readings of `datatable_item*.xml`, compared class by class.
WITNESS_COLUMNS = ("ATK", "DEF", "AR", "ShldDR", "WLv", "MHP", "IncMHP",
                   "RFIRE", "RICE", "RLGHT", "RSTAT", "RPSY", "RedMAG",
                   "Weight", "HR", "ASPD", "MSPD", "ClassType")

# The ONE class the two emitters are allowed to disagree about, with the reason
# written down: both files rank a DUPLICATE class id differently, so this row is
# a different item in each of them (the props emitter takes the first table in
# ITEM_TABLES order, the stats emitter the last). Measured 2026-11-03: ATK 0 vs 5
# and ClassType "Ear" vs "Achieve".
ALLOWED = {
    "500001": "duplicate class id ranked differently by the two emitters "
              "(ATK 0 vs 5, ClassType Ear vs Achieve)",
}

# The three worn classes the row dump prints: a weapon, growing armour, boots.
WORN = ("15001", "7207", "19533")

# How many of the 75 properties must be non-zero SOMEWHERE. Measured on the
# shipped tables every single one is, so this is a floor with room, not a target.
MIN_LIVE_COLUMNS = 60

FILE = os.path.join(DATA, "item_props.json")

# The older, independent emitter: 18 of these 75 columns, same XML, a different
# script. Used only as a SECOND WITNESS - if both emitters say 0 for a worn
# class, the client's own row really is 0 and the gap is not in the reading.
WITNESS = os.path.join(DATA, "item_stats.json")


def _blank(value):
    if isinstance(value, str):
        return value == ""
    return not value


def _witness():
    """{classid: {column: value}} from item_stats.json, or None if it is gone."""
    if not os.path.exists(WITNESS):
        return None
    try:
        with open(WITNESS, encoding="utf-8") as fh:
            return json.load(fh).get("items", {})
    except (ValueError, AttributeError):
        return None


def _witness_compare(items, classes, columns):
    """Compare the two emitters on the 18 columns they share.

    Answers (comparisons, differences), where a difference is a tuple
    (class_key, column, the value in item_props.json, the value in item_stats.json).

    ONLY the pairs the witness STATES are compared. `item_stats.json` writes a
    column only when the client's row carries one, so an absent column there is
    the same fact as a 0 - and comparing against it would report every GROWING
    item (whose flat ATK/DEF the props file fills in from its curve, while the
    stats file leaves the column out and keeps the curve in its own `growing`
    block) as a disagreement it is not. Measured 2026-11-03: 78,822 comparisons,
    2 differences, both class 500001.
    """
    compared = 0
    diffs = []
    for key in sorted(classes, key=lambda k: int(k)):
        row = classes[key]
        other = items.get(key)
        if not isinstance(other, dict):
            continue
        for name in WITNESS_COLUMNS:
            if name not in other:
                continue
            compared += 1
            mine = row[columns.index(name)] if name in columns else 0
            theirs = other[name]
            if isinstance(theirs, str) or name in STRING_COLUMNS:
                agree = (mine or "") == (theirs or "")
            else:
                try:
                    agree = abs(float(mine) - float(theirs)) < 1e-9
                except (TypeError, ValueError):
                    agree = False
            if not agree:
                diffs.append((key, name, mine, theirs))
    return compared, diffs


def _witness_says(items, class_key, name):
    """What the other emitter holds for one class and column: a value, or None.

    `item_stats.json` writes only the columns a row carries, so an absent column
    means the same as 0 - both are "the client's own row is silent here".
    Returns a string for the report either way.
    """
    row = items.get(class_key)
    raw = "" if row is None else row.get(name, "")
    if raw is None or raw == "" or raw == "None":
        return None
    if isinstance(raw, str):
        return raw
    return str(raw)


def main():
    if not os.path.exists(FILE):
        print("FAIL %s does not exist - run emit_item_props.py first" % FILE)
        return 1
    size = os.path.getsize(FILE)
    with open(FILE, encoding="utf-8") as fh:
        doc = json.load(fh)

    columns = doc.get("columns")
    classes = doc.get("classes")
    growing = doc.get("growing", {}).get("items", {})
    problems = []

    if not isinstance(columns, list) or len(columns) != len(COLUMNS):
        print("FAIL columns is %r, expected the %d names"
              % (type(columns).__name__ if not isinstance(columns, list)
                 else len(columns), len(COLUMNS)))
        return 1
    if not isinstance(classes, dict) or not classes:
        print("FAIL no classes object in %s" % FILE)
        return 1
    if list(columns) != list(COLUMNS):
        bad = [i for i, (a, b) in enumerate(zip(columns, COLUMNS)) if a != b]
        problems.append("columns differ from this checker's list at index %s "
                        "(%s vs %s)" % (bad[:5], [columns[i] for i in bad[:5]],
                                        [COLUMNS[i] for i in bad[:5]]))

    # (a) non-zero count per property, and the length check the count depends on.
    width = {}
    nonblank = [0] * len(COLUMNS)
    for key, row in classes.items():
        width[len(row)] = width.get(len(row), 0) + 1
        for i, value in enumerate(row):
            if not _blank(value):
                nonblank[i] += 1
    if len(width) != 1 or len(COLUMNS) not in width:
        problems.append("rows are not all %d long: lengths seen %s"
                        % (len(COLUMNS), dict(sorted(width.items()))))

    print("%s  %d bytes (%.2f MB)" % (FILE, size, size / (1024.0 * 1024.0)))
    print("%d class(es), %d column(s), %d growing curve(s)"
          % (len(classes), len(columns), len(growing)))
    print()
    print("NON-ZERO value on how many of the %d classes:" % len(classes))
    for i, name in enumerate(COLUMNS):
        print("  %-18s %6d%s" % (name, nonblank[i],
                                 "" if nonblank[i] else "   <- EMPTY ON EVERY CLASS"))

    # (b) the worn rows, whole.
    print()
    print("WORN CLASS ROWS (every column, so a shifted index shows):")
    missing_worn = []
    for key in WORN:
        row = classes.get(key)
        if row is None:
            print("  class %s: NOT IN THE FILE" % key)
            missing_worn.append(key)
            continue
        curve = growing.get(key)
        print("  class %s%s:" % (key, "" if not curve else
                                 "   [growing %s = %sxLv + %s]" %
                                 (curve.get("column"), curve.get("value_mul"),
                                  curve.get("value_add"))))
        print("    " + "  ".join("%s=%s" % (c, row[i])
                                 for i, c in enumerate(COLUMNS)))

    # (c) THE WITNESS RULE: the 18 columns the older, independent emitter also
    # carries must agree with it, class by class. Only the pairs the witness file
    # actually states are compared - a column `item_stats.json` leaves out is the
    # client's own silence there, not a disagreement.
    print()
    items = _witness()
    if items is None:
        problems.append("the witness file %s is missing or unreadable - the "
                        "cross-check between the two emitters cannot run"
                        % WITNESS)
    else:
        compared, diffs = _witness_compare(items, classes, columns)
        print("WITNESS (the %d columns item_stats.json also carries): %s comparison(s), "
              "%d difference(s)" % (len(WITNESS_COLUMNS),
                                    "{:,}".format(compared), len(diffs)))
        for key, name, mine, theirs in diffs:
            allowed = ALLOWED.get(key)
            print("  class %s  %-10s item_props=%s  item_stats=%s%s"
                  % (key, name, mine, theirs,
                     "   <- ALLOWED: " + allowed if allowed else "   <- DISAGREE"))
            if not allowed:
                problems.append(
                    "class %s %s: item_props.json says %s, item_stats.json says %s - "
                    "the two readings of the same XML differ"
                    % (key, name, mine, theirs))

    dead = [n for i, n in enumerate(COLUMNS) if not nonblank[i]]
    if len(COLUMNS) - len(dead) < MIN_LIVE_COLUMNS:
        problems.append("only %d of %d properties carry a value anywhere "
                        "(floor is %d)" % (len(COLUMNS) - len(dead),
                                           len(COLUMNS), MIN_LIVE_COLUMNS))
    if missing_worn:
        problems.append("a worn fixture class is not in the file at all: %s"
                        % ", ".join(missing_worn))

    print()
    if problems:
        print("FAIL")
        for text in problems:
            print("  " + text)
        return 1
    print("PASS  %d/%d properties carry a value, and all %d witness columns agree "
          "with item_stats.json class by class (%d allowed difference(s))"
          % (len(COLUMNS) - len(dead), len(COLUMNS), len(WITNESS_COLUMNS),
             len(ALLOWED)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
