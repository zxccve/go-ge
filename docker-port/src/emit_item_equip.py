"""emit_item_equip - class id -> the equipment slot(s) that class may be worn in.

WHY THIS EXISTS
---------------
The zone's equip path (R59) has to refuse "this item cannot go in that slot"
WITHOUT touching the database. The answer is already in the CLIENT's own item
tables - every `<Class>` carries `DefaultEqpSlot`, the slot the client itself
puts the item in - and the shipped server reads the same field:

    geServer\\ZServerLib\\EquipSlotControl.cpp:276
        if (false == IsValidSpot(spot, equipType))
            spot = GetDefaultSpot(item);

so this file is a COPY of the client's answer, not a guess. It is emitted once
because the source is 82 MB of XML across 27 `datatable_item*.xml` files, and a
zone restart must not read that.

WHY A NEW SCRIPT (the project rule says reuse first)
-----------------------------------------------------
`ge_item_index.py` reads the same 27 files but keeps only ClassName -> ClassID;
`emit_item_catalog.py` writes the NAME catalog (class -> [id, mergeable, max
stack]) and no slot column. Neither carries a slot, and widening the catalog
file would change a shipped format for a second reader. This one file is the
smallest thing that answers the question, and it reuses `ITEM_TABLES` from
`ge_item_index` so the two cannot disagree about WHICH tables are the Item
idspace.

THE ENCODING (see `zone\\equip.go` and `goserver\\item\\equip.go`)
---------------------------------------------------------------
EQP_HANDS  = 100 : a hand item - either LHAND (4) or RHAND (5) is accepted.
EQP_RINGS  = 101 : a ring      - either RING1 (6) or RING2 (7) is accepted.
otherwise        : the ONE slot index (0..20), the same numbering as
                   EQUIP_SPOT in the client's `geSharedTypes.h:7-30` and as
                   `wear_item.spot` in PostgreSQL.

A class with NO entry is NOT wearable at all: `DefaultEqpSlot` is empty or
"None" for 11,000+ rows (money, potions, stones). That is the answer the equip
path needs for "you cannot wear this".

Output: `data\\item_equip.json`
    {"count": N, "slots": {"7201": 100, "15001": 1, ...},
     "source": "...", "note": "..."}

Run:  cd docker-port\\src && python emit_item_equip.py
"""

import json
import os
import re

import ge_item_index
from ge_paths import DATA, GE

OUT_FILE = os.path.join(DATA, "item_equip.json")

EQP_HANDS = 100
EQP_RINGS = 101

# DefaultEqpSlot NAME -> EQUIP_SPOT index. The names are the client's own
# (`ge\\xml\\datatable_item*.xml`), the numbers are `geSharedTypes.h:7-30`.
# RING, LH and RH are special and are handled before this table is consulted.
SLOT_BY_NAME = {
    "HEAD": 0, "BODY": 1, "FACE": 2, "NECK": 3,
    "GLOVE": 8, "BOOTS": 9, "BACK": 10, "COSTUME": 11, "EAR": 12,
    "BELT": 13, "BREAST": 14, "SHOULDER": 15, "ACHIEVE": 16,
    "ASSIST": 17, "LCOS": 18, "RCOS": 19, "ARTIFACT": 20,
}

TAG_RE = re.compile(r"<Class\s[^>]*?/?>")


def _attr(tag, name):
    m = re.search(r'\b%s="([^"]*)"' % name, tag)
    return m.group(1) if m else ""


def slot_code(default_slot):
    """The one number this project stores for a DefaultEqpSlot name, or None.

    None means "not wearable": an empty name, the literal "None", or a name the
    client has that this table does not know yet. A silent 0 would put an
    unknown item in the HEAD slot, so an unknown name is reported by the caller
    instead.
    """
    name = (default_slot or "").strip()
    if name in ("", "None"):
        return None
    if name in ("LH", "RH"):
        return EQP_HANDS
    if name == "RING":
        return EQP_RINGS
    return SLOT_BY_NAME.get(name)


def build(root=None):
    """Read the client's item tables and return (slots, unknown_names)."""
    root = root or GE
    slots = {}
    unknown = {}
    for table in ge_item_index.ITEM_TABLES:
        path = os.path.join(root, "xml", table + ".xml")
        if not os.path.exists(path):
            # ge_item_index raises for a missing table; here the table list is
            # shared, so a missing file is a fact worth reporting, not a crash.
            unknown["<missing file: %s>" % table] = unknown.get(
                "<missing file: %s>" % table, 0) + 1
            continue
        with open(path, encoding="utf-8", errors="replace") as fh:
            blob = fh.read()
        for tag in TAG_RE.findall(blob):
            raw = _attr(tag, "DefaultEqpSlot")
            code = slot_code(raw)
            if code is None:
                if raw not in ("", "None"):
                    unknown[raw] = unknown.get(raw, 0) + 1
                continue
            try:
                class_id = int(_attr(tag, "ClassID"))
            except ValueError:
                continue
            slots[class_id] = code
    return slots, unknown


def main():
    slots, unknown = build()
    doc = {
        "count": len(slots),
        "slots": {str(k): v for k, v in sorted(slots.items())},
        "source": "client xml datatable_item*.xml DefaultEqpSlot via ge_item_index.ITEM_TABLES",
        "note": "100 = either hand (4 or 5), 101 = either ring (6 or 7), "
                "else the EQUIP_SPOT index 0..20 = wear_item.spot",
    }
    with open(OUT_FILE, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"))
    print("wrote %s: %d wearable class(es)" % (OUT_FILE, len(slots)))
    if unknown:
        shown = sorted(unknown.items(), key=lambda kv: -kv[1])[:5]
        print("not wearable and not mapped (%d distinct name(s)): %s"
              % (len(unknown), ", ".join("%s x%d" % kv for kv in shown)))


if __name__ == "__main__":
    main()
