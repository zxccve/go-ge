r"""Write the item catalog as one JSON file: script item NAME -> class id and stack rule.

WHY A DATA FILE: a script names items ("Vis", "FireStone", "CrecementoSword") and only
the client's item tables know the number behind that name. `ge_item_index` already reads
them, so the translation is a BUILD step here and the Go server replays the result -
the same arrangement as `emit_zone_npcs.py` and `emit_zone_nav.py`. Writing a second
`.xml` reader in Go would be a second implementation of work already proved.

The three fields are exactly what a give/take needs (`CInventoryControl::Add`):

- `class_id` - the number the inventory row carries.
- `mergeable` - False means the shipped server never adds to an existing row for this
  class (equipment, unstackable), so a give mints a NEW id instead.
- `max_stack` - the client's per-slot cap; a give that would pass it is a FULL
  INVENTORY and fails the transaction.

The file is DERIVED data: regenerate it, never hand-edit it.

Usage
-----
    python emit_item_catalog.py            # -> data\item_catalog.json
    python emit_item_catalog.py --print    # also show a few known names
"""

import argparse
import json
import os
import time

import ge_item_index
from ge_paths import DATA, ensure

OUT_FILE = os.path.join(DATA, "item_catalog.json")


def catalog():
    """{name: [class_id, mergeable, max_stack]} over every Item-idspace table."""
    names = ge_item_index.classes()
    out = {}
    for name, class_id in names.items():
        mergeable, cap = ge_item_index.stack_rule(class_id)
        if mergeable and cap <= 1:
            # mergeable with a cap of 1 is contradictory: treat it as unstackable,
            # because a "merge" into a slot that holds one would be a no-op the
            # client cannot draw.
            mergeable = False
        out[name] = [int(class_id), 1 if mergeable else 0, int(cap)]
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default=OUT_FILE)
    ap.add_argument("--print", action="store_true", dest="show")
    args = ap.parse_args()

    started = time.time()
    items = catalog()
    ensure(os.path.dirname(args.out))
    doc = {
        "source": "client xml datatable_item*.xml via ge_item_index",
        "count": len(items),
        "items": items,
    }
    tmp = args.out + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"), sort_keys=True)
    os.replace(tmp, args.out)
    size = os.path.getsize(args.out)
    print("item catalog: %d name(s), %d bytes in %.1f s -> %s"
          % (len(items), size, time.time() - started, args.out))

    mergeable = sum(1 for v in items.values() if v[1])
    print("  mergeable: %d, one-per-slot: %d" % (mergeable, len(items) - mergeable))
    if args.show:
        for name in ("Vis", "Feso", "FireStone", "IceStone", "WindStone"):
            print("  %-12s -> %s" % (name, items.get(name)))


if __name__ == "__main__":
    main()
