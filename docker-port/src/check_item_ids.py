"""Prove that an item id follows the shipped server's own mint rule.

Why this exists
---------------
`ITEM.ITEM_ID` is NOT an identity column: the caller supplies it. The shipped
server mints it in `geGUIDGen` (source2/geServerShared/source/GUIDGen.cpp):

    guid = (GE_GUID)base << 32; guid += ++counter;

with one counter per GUID type, starting at 1, and `base` fixed for the life of
the process. `base` is `geWorldTable::GetInstanceID()`, which calls
`RESOURCE.dbo.NS_GET_INSTANCEHISTORY` - that proc inserts a row into
`INSTANCEHISTORY` and returns its IDENTITY. So every server start gets the next
base, and two processes can never collide.

A wrong id does not fail loudly: it writes an item nobody can see, or overwrites
a real one. So before any item write, prove the id. That is what this does.

Usage
-----
    python check_item_ids.py                # audit the ids already in ITEM
    python check_item_ids.py 1234567890123  # prove specific ids are safe
    python check_item_ids.py --next         # print the next free base
    python check_item_ids.py --alloc        # claim the next base (WRITES)

`--alloc` is the only mode that writes, and it only touches RESOURCE, never the
item tables. Exit code is 1 if any id fails, so it can gate a commit.
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

WORD = 4294967296  # 2**32; the split point between base and counter


def split(guid):
    """(base, counter) of an item id, the way geGUIDGen builds it."""
    guid = int(guid)
    return guid >> 32, guid & (WORD - 1)


def known_bases():
    """The set of bases the RESOURCE instance history has actually issued."""
    rows = _db()._run("SELECT INSTANCE_ID FROM RESOURCE.dbo.INSTANCEHISTORY")
    return set(int(r[0]) for r in rows)


def next_base():
    """The base the next server start would get (max issued + 1)."""
    rows = _db()._run("SELECT MAX(INSTANCE_ID) FROM RESOURCE.dbo.INSTANCEHISTORY")
    return int(rows[0][0]) + 1


def alloc_base():
    """Claim a base exactly as the original server does. WRITES to RESOURCE."""
    rows = _db()._run("INSERT INTO RESOURCE.dbo.INSTANCEHISTORY ([DATE]) "
                      "VALUES (GETDATE()); SELECT SCOPE_IDENTITY();")
    return int(rows[0][0])


def existing_ids():
    """Every ITEM_ID already used, with the base it belongs to."""
    rows = _db()._run("SELECT ITEM_ID FROM GE_DATA.dbo.ITEM")
    return set(int(r[0]) for r in rows)


def check(guids):
    """[(id, base, ok, why)] - does this id follow the mint rule and stay free?"""
    bases = known_bases()
    taken = existing_ids()
    out = []
    for guid in guids:
        base, counter = split(guid)
        if guid in taken:
            out.append((guid, base, False, "COLLISION: already a row in ITEM"))
        elif base not in bases:
            out.append((guid, base, False,
                        "base %d was never issued (INSTANCEHISTORY has no such row)"
                        % base))
        elif counter == 0:
            out.append((guid, base, False, "counter 0: geGUIDGen starts at 1"))
        else:
            out.append((guid, base, True,
                        "base %d issued, counter %d, id free" % (base, counter)))
    return out


def follows_rule(guid):
    """(ok, why) - does this id obey the mint rule? Ignored whether it is taken.

    `check()` answers "is this id safe to use now", so an id that is already a row
    is a COLLISION. That makes it useless for validating an id the server just
    minted and used, which is exactly what a post-commit test wants to prove.
    """
    base, counter = split(guid)
    if base not in known_bases():
        return False, "base %d was never issued by INSTANCEHISTORY" % base
    if counter == 0:
        return False, "counter 0: geGUIDGen starts at 1"
    return True, "base %d issued, counter %d" % (base, counter)


def audit():
    """Summary of every base in ITEM: how many ids, is it a real instance."""
    bases = known_bases()
    rows = _db()._run(
        "SELECT CAST(ITEM_ID / CAST(%d AS bigint) AS bigint) AS base, "
        "COUNT(*), MIN(ITEM_ID), MAX(ITEM_ID) FROM GE_DATA.dbo.ITEM GROUP BY "
        "CAST(ITEM_ID / CAST(%d AS bigint) AS bigint) ORDER BY base" % (WORD, WORD))
    bad = 0
    for base, n, lo, hi in rows:
        base = int(base)
        if base in bases:
            continue
        bad += 1
        print("  UNKNOWN BASE %d (%s ids, %s..%s)" % (base, n, lo, hi))
    print("%d distinct bases in ITEM, %d not issued by INSTANCEHISTORY"
          % (len(rows), bad))
    return bad == 0


def _db():
    import ge_db
    return ge_db


def main(argv):
    if "--alloc" in argv:
        base = alloc_base()
        print("claimed base %d (counter starts at 1)" % base)
        print("first item id would be %d" % (base << 32 | 1))
        return 0
    if "--next" in argv:
        print(next_base())
        return 0
    ids = [int(a) for a in argv if a.isdigit()]
    if not ids:
        return 0 if audit() else 1
    ok = True
    for guid, base, good, why in check(ids):
        print("%s %d -> base %d: %s" % ("OK  " if good else "FAIL", guid, base, why))
        ok = ok and good
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
