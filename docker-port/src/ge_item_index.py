r"""Item ClassName -> ClassID, from the CLIENT's own tables.

Why this exists
---------------
A quest script names an item by its ClassName (`TxGiveItem(tx, 'FireStone', 1)`).
The shipped server turns that name into a class id with
`geItemTable::Get(name.c_str())` (`ZServerLib\BindFunction\TransactionFunc.cpp:294`),
and an UNKNOWN name is not a warning: it rolls the whole transaction back.

The client's item idspace is split over 47 `datatable_item*.xml` files in
`ge\\xml` and only SOME of them extend the `Item` idspace. Those are the ones
listed in `ITEM_TABLES`; the other `datatable_item*` files (opt, group, socket,
reinforce, set, make, charge, bless) are a DIFFERENT idspace whose numbers
collide, so including them would map a name to the wrong class.

The parse is cached in `cache\\item_classes.json` because it reads ~82 MB.
Nothing works at import time: call `class_id()`.
"""

import json
import os
import re
import threading

from ge_paths import CACHE, GE

XML_DIR = os.path.join(GE, "xml")
CACHE_FILE = os.path.join(CACHE, "item_classes.json")

# The tables that extend the `Item` idspace, in the client's own load order.
# `datatable_item` (base) first, then the category tables.
ITEM_TABLES = (
    "datatable_item",
    "datatable_item_weapon", "datatable_item_armor", "datatable_item_etc",
    "datatable_item_consume", "datatable_item_costume", "datatable_item_ring",
    "datatable_item_neck", "datatable_item_earring", "datatable_item_belt",
    "datatable_item_boots", "datatable_item_breast", "datatable_item_glove",
    "datatable_item_head", "datatable_item_shoulder", "datatable_item_back",
    "datatable_item_face", "datatable_item_artifact", "datatable_item_book",
    "datatable_item_quest", "datatable_item_scroll", "datatable_item_assist",
    "datatable_item_instant", "datatable_item_recipe", "datatable_item_achieve",
    "datatable_item_lumin", "datatable_item_randombase",
)

# <Class ClassID="30000" ClassName="Vis" ... /> - attribute order is NOT fixed,
# so the tag is captured whole and each attribute read out of it.
TAG_RE = re.compile(r"<Class\s[^>]*?/?>")
ATTR_RE = r'\b%s="([^"]*)"'

STACK_CACHE = os.path.join(CACHE, "item_stack_rules.json")

# The two flags and the cap that decide whether a give MERGES into the stack the
# player already holds (see CInventoryControl::Add in the shipped source).
MERGE_ATTRS = ("EQUP", "Unstack", "MaxStack")

_lock = threading.Lock()
_map = None             # ClassName -> ClassID
_lower = None           # ClassName.lower() -> ClassID, for near misses


def _attrs(tag):
    out = {}
    for key in ("ClassID", "ClassName"):
        found = re.search(ATTR_RE % key, tag)
        if found:
            out[key] = found.group(1)
    return out


def build():
    """{ClassName: ClassID} over every Item-idspace table the client ships."""
    names = {}
    missing = []
    for table in ITEM_TABLES:
        path = os.path.join(XML_DIR, table + ".xml")
        if not os.path.exists(path):
            missing.append(table)
            continue
        with open(path, encoding="utf-8", errors="replace") as fh:
            text = fh.read()
        for tag in TAG_RE.findall(text):
            attr = _attrs(tag)
            if "ClassID" not in attr or "ClassName" not in attr:
                continue
            names[attr["ClassName"]] = int(attr["ClassID"])
    return names, missing


def _atomic_write(path, obj):
    """Atomic, with retry: on Windows `os.replace` can lose a race with a reader."""
    import time
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(obj, fh, ensure_ascii=False)
    for _ in range(20):
        try:
            os.replace(tmp, path)
            return True
        except PermissionError:
            time.sleep(0.05)
    return False


def _write_cache(names):
    return _atomic_write(CACHE_FILE, names)


def _load():
    global _map, _lower
    if _map is not None:
        return _map
    with _lock:
        if _map is not None:
            return _map
        if os.path.exists(CACHE_FILE):
            try:
                with open(CACHE_FILE, encoding="utf-8") as fh:
                    _map = {k: int(v) for k, v in json.load(fh).items()}
            except (OSError, ValueError):
                _map = None
        if _map is None:
            _map, missing = build()
            if missing:
                print("ge_item_index: tables not found: %s" % ", ".join(missing))
            _write_cache(_map)
        _lower = {k.lower(): v for k, v in _map.items()}
        return _map


def classes():
    """A copy of the whole {ClassName: ClassID} map."""
    return dict(_load())


def class_id(name):
    """Item ClassID for a script's item name, or None if the client has no such name."""
    table = _load()
    if name in table:
        return table[name]
    return _lower.get(str(name).lower())


def name_of(class_id_):
    """ClassID -> ClassName (first match), or None."""
    wanted = int(class_id_)
    for name, cid in _load().items():
        if cid == wanted:
            return name
    return None


_stack = None


def _build_stack_rules():
    """{class_id: [mergeable, max_stack]} over the same Item-idspace tables."""
    rules, missing = {}, []
    for table in ITEM_TABLES:
        path = os.path.join(XML_DIR, table + ".xml")
        if not os.path.exists(path):
            missing.append(table)
            continue
        with open(path, encoding="utf-8", errors="replace") as fh:
            text = fh.read()
        for tag in TAG_RE.findall(text):
            cid = re.search(ATTR_RE % "ClassID", tag)
            if not cid:
                continue
            flags = {}
            for key in ("EQUP", "Unstack", "MaxStack"):
                found = re.search(ATTR_RE % key, tag)
                flags[key] = found.group(1) if found else None
            equip = (flags["EQUP"] or "").upper() == "TRUE"
            unstack = (flags["Unstack"] or "").upper() == "TRUE"
            try:
                cap = int(flags["MaxStack"] or 1)
            except ValueError:
                cap = 1
            rules[int(cid.group(1))] = [0 if (equip or unstack) else 1, cap]
    return rules, missing


def stack_rule(class_id_):
    """(mergeable, max_stack) for one class, from the client's own item table.

    `mergeable` False means the shipped server never adds to an existing row for
    this class (equipment and unstackable items), so a give must mint a new id.
    """
    global _stack
    if _stack is None:
        with _lock:
            if _stack is None:
                data = None
                if os.path.exists(STACK_CACHE):
                    try:
                        with open(STACK_CACHE, encoding="utf-8") as fh:
                            data = {int(k): list(v) for k, v in json.load(fh).items()}
                    except (OSError, ValueError):
                        data = None
                if data is None:
                    data, missing = _build_stack_rules()
                    if missing:
                        print("ge_item_index: stack rules missing: %s"
                              % ", ".join(missing))
                    _atomic_write(STACK_CACHE, data)
                _stack = data
    entry = _stack.get(int(class_id_))
    return (bool(entry[0]), int(entry[1])) if entry else (True, 1)


def main():
    names = _load()
    print("item classes: %d (cached in %s)" % (len(names), CACHE_FILE))
    for probe in ("Vis", "Feso", "FireStone", "IceStone", "WindStone"):
        print("  %-12s -> %s" % (probe, class_id(probe)))


if __name__ == "__main__":
    main()
