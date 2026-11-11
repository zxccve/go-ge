r"""Prove that a script's TxGiveItem / TxTakeItem really moves items in the DB.

What this checks, end to end, through the same code the click path uses:
  1. a give writes an ITEM row + a STORAGE row for the right commander, and the
     id it used is inside the mint rule (base issued by INSTANCEHISTORY);
  2. a take of exactly what was given removes BOTH rows again, so the test
     leaves the database as it found it;
  3. an unknown item name fails the builtin AND the transaction, writing
     nothing - the shipped server rolls back the whole tx, not just the call;
  4. taking more than is held fails the same way.

The item class is chosen so the commander does NOT already hold it: a take
targets the OLDEST stack of that class, so using a class already in the bag
would quietly take the player's real stack instead of the test one.

Usage
-----
    python check_item_tx.py            # commander 24
    python check_item_tx.py 24
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

PREFERRED = ("FireStone", "IceStone", "WindStone", "GrowthStone")


def _db():
    import ge_db
    return ge_db


def held_classes(commander):
    """{CLASS_ID: total count} in the commander's bag, trash excluded."""
    rows = _db()._run(
        "SELECT i.CLASS_ID, SUM(i.COUNT) FROM STORAGE s "
        "JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
        "WHERE s.COMMANDER_ID = %d AND s.TRASH = 0 GROUP BY i.CLASS_ID" % int(commander))
    return {int(r[0]): int(r[1]) for r in rows}


def pick_item(commander):
    """(name, class id) of an item the commander does not hold, so the test is clean."""
    import ge_item_index
    held = held_classes(commander)
    for name in PREFERRED:
        cid = ge_item_index.class_id(name)
        if cid is not None and cid not in held:
            return name, cid
    for name, cid in ge_item_index.classes().items():
        if cid not in held:
            return name, cid
    raise RuntimeError("every item class is already in the bag")


def rows_for(commander, class_id):
    """[(ITEM_ID, COUNT)] the commander holds of one class."""
    rows = _db()._run(
        "SELECT s.ITEM_ID, i.COUNT FROM STORAGE s JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
        "WHERE s.COMMANDER_ID = %d AND i.CLASS_ID = %d AND s.TRASH = 0 "
        "ORDER BY s.INSERT_DATE" % (int(commander), int(class_id)))
    return [(int(r[0]), int(r[1])) for r in rows]


def run(commander):
    import check_item_ids
    import script_host

    host = script_host.ScriptHost(zone_id=10)
    host.commander_id = commander
    name, class_id = pick_item(commander)
    before = rows_for(commander, class_id)
    problems = []

    def expect(label, got, want):
        ok = got == want
        print("  %s %s: got %r, want %r" % ("OK  " if ok else "FAIL", label, got, want))
        if not ok:
            problems.append(label)

    print("commander %d, item %r (class %d), held before: %s"
          % (commander, name, class_id, before or "none"))

    # 1. give
    tx = host.tx_begin()
    expect("TxGiveItem returns 1", host.tx_item(tx, name, 3, "give"), 1)
    expect("TxEnd returns 1", host.tx_end(tx), 1)
    after_give = rows_for(commander, class_id)
    new = [r for r in after_give if r not in before]
    expect("exactly one new stack", len(new), 1)
    if new:
        item_id, count = new[0]
        expect("new stack count", count, 3)
        verdict = check_item_ids.follows_rule(item_id)
        expect("id is inside the mint rule (%s)" % verdict[1], verdict[0], True)
        print("  ..   new ITEM_ID %d" % item_id)

    # 2. take it straight back
    tx = host.tx_begin()
    expect("TxTakeItem returns 1", host.tx_item(tx, name, 3, "take"), 1)
    expect("TxEnd returns 1", host.tx_end(tx), 1)
    expect("bag is back to where it started", rows_for(commander, class_id), before)

    # 3. unknown name: the builtin and the transaction both fail, nothing written
    tx = host.tx_begin()
    expect("unknown name returns 0", host.tx_item(tx, "NoSuchItem_ZZZ", 1, "give"), 0)
    expect("TxEnd of a failed tx returns 0", host.tx_end(tx), 0)

    # 4. take more than is held
    tx = host.tx_begin()
    expect("over-take returns 0", host.tx_item(tx, name, 999999999, "take"), 0)
    expect("TxEnd of that tx returns 0", host.tx_end(tx), 0)
    expect("nothing changed by the failed takes", rows_for(commander, class_id), before)

    ok = not problems
    print("%s (%d problem%s)" % ("PASS" if ok else "FAIL", len(problems),
                                 "" if len(problems) == 1 else "s"))
    return 0 if ok else 1


def main(argv):
    commander = int(argv[0]) if argv and argv[0].isdigit() else 24
    return run(commander)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
