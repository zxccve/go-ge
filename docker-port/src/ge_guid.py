r"""Item ids, minted exactly the way the shipped server mints them.

Why this exists
---------------
`ITEM.ITEM_ID` is not an identity column: the caller supplies it. The shipped
server (`geServerShared\source\GUIDGen.cpp`) does:

    m_guid[type]++;
    guid = (GE_GUID)base << 32;
    guid += m_guid[type];

and `base` is `geWorldTable::GetInstanceID()` (`ZServerLib\Server_Init.cpp:399`),
which calls `RESOURCE.dbo.NS_GET_INSTANCEHISTORY`. That proc inserts a row into
`INSTANCEHISTORY` and returns its IDENTITY, so **every server start gets the next
base** and two processes can never collide.

So there is exactly one rule to obey: one base per process, one counter per
process, and never reuse a base that any existing row already carries.
`check_item_ids.py` proves a minted id stays inside the rule.

Nothing happens at import time: the base is claimed on the first `next_item_id()`.
"""

import threading

_lock = threading.Lock()
_base = None
_counter = 0


def claim_base():
    """Ask the RESOURCE database for the next unused base. WRITES one row.

    This is the original `NS_GET_INSTANCEHISTORY`: the base is the IDENTITY of a
    row inserted now, so the answer is monotonic and unique across restarts.
    """
    global _base
    if _base is not None:
        return _base
    with _lock:
        if _base is not None:
            return _base
        import ge_db
        rows = ge_db._run(
            "INSERT INTO RESOURCE.dbo.INSTANCEHISTORY ([DATE]) VALUES (GETDATE()); "
            "SELECT CAST(SCOPE_IDENTITY() AS bigint)")
        if not rows:
            raise RuntimeError("INSTANCEHISTORY did not return an id")
        base = int(rows[0][0])
        if base <= 0:
            raise RuntimeError("INSTANCEHISTORY returned a nonsense base %r" % (base,))
        _base = base
        return _base


def next_item_id():
    """The next ITEM_ID this process may use: (base << 32) + counter."""
    global _counter
    base = claim_base()
    with _lock:
        _counter += 1
        return (base << 32) | _counter


def base():
    """The base this process was given (claiming one if needed)."""
    return claim_base()


def main():
    import check_item_ids
    here = base()
    print("base %d (this process)" % here)
    ids = [next_item_id() for _ in range(3)]
    print("next ids: %s" % ", ".join(str(i) for i in ids))
    bad = 0
    for guid, gbase, ok, why in check_item_ids.check(ids):
        print("  %s %d -> %s" % ("OK  " if ok else "FAIL", guid, why))
        bad += 0 if ok else 1
    return 1 if bad else 0


if __name__ == "__main__":
    raise SystemExit(main())
