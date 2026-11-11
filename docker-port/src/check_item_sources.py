r"""Which copy of the item tables is the one the game actually runs?

WHY THIS EXISTS. Three copies of every item table sit on this machine, and they do NOT
agree:

    ge\xml\          the full RAW tables
    ge\xml_export\   a LOCALIZED export of them
    ge\ies\          the binary tables the game/server actually loads

The friends who know the game describe the pipeline as `xml -> xml_export -> ies`, which
would make `xml_export` the source of truth. Measured on this install, it is not:

    47 tables, 29,399 item rows
    ies rows matched by xml        : 29399  (100.00%)
    ies rows matched by xml_export :  8430  ( 28.67%)

So the `ies` we ship against was built from the RAW set, and `xml_export` here is a
different generation (older or from another build). The item catalog
(`src\emit_item_catalog.py`) reads `ge\xml`, which is therefore CORRECT - and this tool is
how that stays true after a data update.

The three fields compared are the ones a give/take acts on: class id, stackable (EQUP or
Unstack -> one per slot), and MaxStack. `.ies` is binary; it is decoded with the game's own
converter (the `Converter\ies2.py` beside the repo root, the same one `check_data_generation.py` uses)
into a temp folder, once per table.

Usage
-----
    python check_item_sources.py               # all 47 tables (about a minute cold)
    python check_item_sources.py --limit 3     # a quick look
    python check_item_sources.py --show FireStone

Exit code 1 when the RAW copy does NOT match the ies: that is the case where the catalog
would give the server a different answer from the game.
"""

import argparse
import os
import re
import sys
import tempfile

from ge_paths import GE, ROOT

RAW_DIR = os.path.join(GE, "xml")
EXPORT_DIR = os.path.join(GE, "xml_export")
IES_DIR = os.path.join(GE, "ies")

# The game's own .ies -> xml converter. It lives beside the shipped tools, not in src\,
# because it is the GAME's code, not ours.
CONVERTER = os.path.join(os.path.dirname(ROOT), "Converter")

TAG_RE = re.compile(r"<Class\s[^>]*?/?>")


def attr(tag, key):
    found = re.search(r'\b%s="([^"]*)"' % key, tag)
    return found.group(1) if found else None


def rows_from_text(text):
    """{ClassName: (class_id, stackable, max_stack)} - the same rule on every copy."""
    out = {}
    for tag in TAG_RE.findall(text):
        class_id = attr(tag, "ClassID")
        class_name = attr(tag, "ClassName")
        if class_id is None or class_name is None:
            continue
        equip = (attr(tag, "EQUP") or "").upper() == "TRUE"
        unstack = (attr(tag, "Unstack") or "").upper() == "TRUE"
        try:
            cap = int(attr(tag, "MaxStack") or 1)
        except ValueError:
            cap = 1
        out[class_name] = (int(class_id), not (equip or unstack), cap)
    return out


def rows_from_xml(folder, table):
    path = os.path.join(folder, table + ".xml")
    if not os.path.exists(path):
        return {}
    with open(path, encoding="utf-8", errors="replace") as fh:
        return rows_from_text(fh.read())


def rows_from_ies(table, cache, ies2):
    out_path = os.path.join(cache, table + ".xml")
    if not os.path.exists(out_path):
        ies2.ies_to_xml(os.path.join(IES_DIR, table + ".ies"), out_path,
                        None, None, "UTF-8", False)
    with open(out_path, encoding="utf-8", errors="replace") as fh:
        return rows_from_text(fh.read())


def table_names(limit=None):
    names = [entry[:-len(".ies")] for entry in sorted(os.listdir(IES_DIR))
             if entry.startswith("datatable_item") and entry.endswith(".ies")]
    return names[:limit] if limit else names


def compare(limit=None, show=None, cache=None):
    if CONVERTER not in sys.path:
        sys.path.insert(0, CONVERTER)
    import ies2  # the game's own converter; imported here so --help works without it

    cache = cache or os.path.join(tempfile.gettempdir(), "ge_item_ies")
    os.makedirs(cache, exist_ok=True)

    tables = table_names(limit)
    total = match_raw = match_export = 0
    problems = []
    sample = {}

    for table in tables:
        try:
            truth = rows_from_ies(table, cache, ies2)
        except Exception as exc:  # a table the converter cannot read is worth naming
            problems.append("%-28s cannot decode: %s" % (table, exc))
            continue
        raw = rows_from_xml(RAW_DIR, table)
        export = rows_from_xml(EXPORT_DIR, table)
        good_raw = sum(1 for name, row in truth.items() if raw.get(name) == row)
        good_export = sum(1 for name, row in truth.items() if export.get(name) == row)
        total += len(truth)
        match_raw += good_raw
        match_export += good_export
        if good_raw != len(truth):
            problems.append("%-28s ies=%5d raw=%5d export=%5d"
                            % (table, len(truth), good_raw, good_export))
        if show and show in truth:
            sample = {"ies": truth[show], "xml": raw.get(show),
                      "xml_export": export.get(show)}

    pct = lambda n: (100.0 * n / total) if total else 0.0
    print("tables                 : %d" % len(tables))
    print("item rows in ies       : %d" % total)
    print("matched by xml         : %d (%.2f%%)" % (match_raw, pct(match_raw)))
    print("matched by xml_export  : %d (%.2f%%)" % (match_export, pct(match_export)))

    if problems:
        print("\ntables where the RAW copy does not match ies:")
        for line in problems:
            print("  " + line)

    if sample:
        print("\n%s:" % show)
        for label in ("ies", "xml", "xml_export"):
            print("  %-11s %s" % (label, sample.get(label) or "(not found)"))

    ok = total > 0 and match_raw == total
    print("\n%s" % ("RAW xml IS the runtime copy (the catalog is right)"
                    if ok else "RAW xml does NOT match ies - the catalog may be wrong"))
    return ok


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--limit", type=int, help="only the first N item tables")
    ap.add_argument("--show", help="print one item from every copy")
    args = ap.parse_args()
    if not compare(limit=args.limit, show=args.show):
        sys.exit(1)


if __name__ == "__main__":
    main()
