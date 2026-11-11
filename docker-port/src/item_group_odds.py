r"""item_group_odds - the exact odds of ONE item-group / roulette roll.

WHY THIS EXISTS: a box's contents are DATA, and the three tables that decide a
roll were only half decoded. This tool reads them and prints the distribution a
roll would produce, so a Go loader can be checked against a number instead of a
guess.

THE MECHANISM (all references in `source2\`, cited line by line):

  `CWorldTable::GetGambleRandomItem` (geServerShared\source\WorldTable.cpp:2329)
  picks ONE group, then ONE item row inside it, and takes that row's ItemCount:

      Group = geItemGroupTable::GetGiftGroup(name, Point, BackPoint)   :2335
      Prop  = geItemGroupTable::GetGiftProp(Group)                     :2359
      Item  = Prop->type ; PickedCount = Prop->ItemCount               :2364-2365

  `GetGiftGroup` (geShared\Source\ItemGroupTable.cpp:239) finds the block whose
  TableName equals the name; `GetGiftGroupBYProperty` (:256-299) then does:

      rows   = block[Point : Count - BackPoint]        (:263-264 - INDEX trim,
                                                        not a weight)
      S      = sum(ratio of those rows)                (:269)
      r      = RandomInt(0, S)                         (:275)
      pick the FIRST row whose running sum >= r        (:277-284)

  `RandomInt(low, high)` is INCLUSIVE at both ends
  (source2\imclib\source\random.cpp:201-227, returns low + n % (high-low+1)),
  so there are S+1 equally likely values and the first row owns r == 0 as well:

      P(row i) = ratio_i / (S+1)          for i > 0
      P(row 0) = (ratio_0 + 1) / (S+1)    the +1 is the r == 0 case

  `GetGiftProp` (ItemGroupTable.cpp:551-573) picks uniformly among EVERY
  `datatable_itemgroup` row carrying that Group - RandomInt(1, count) - there is
  no per-item weight in that table. `ItemCount` is NOT rolled: it is the row's
  ItemCount, read as float and truncated to DWORD (:2365, :379-392).

  POINT / BACKPOINT both 0 = the whole block. A nonzero Point drops that many
  rows from the FRONT and BackPoint that many from the BACK of the block's row
  list, so one block can serve several boxes with different rarity windows.

WHAT THIS TOOL CANNOT DO: the name -> Group mapping lives in the idspace
`ChangeItemRatio` (ItemGroupTable.cpp:18). The rev136 client ships NO such
table - `datatable_changeitem_serv.ies` in `ies10000.ipf` is the 382-byte
`ChangeItemRatioDelect` stub with one `TableName="SORRY"` row. The only real
copy in the tree is the 2014 Japanese localisation file
`ge\ies\JPN\datatable_changeitem_serv.ies` (idspace ChangeItemRatio, 3,132 rows,
220 blocks), so only the boxes that existed then resolve. For anything else the
tool STOPS and names the missing table - it never invents a group.

Usage (run from `docker-port\src`):

    python item_group_odds.py                    # GLimit_Bellem_Cash, the whole distribution
    python item_group_odds.py --group NAME       # any block in ChangeItemRatio
    python item_group_odds.py --box 1            # a SingleRullet ClassID (1..327)
    python item_group_odds.py --box SBELLEM      # a SingleRullet ClassName
    python item_group_odds.py --coverage         # how many script call sites resolve

Work is in `main()`; importing this module does nothing.
"""
from __future__ import annotations

import argparse
import os
import sys
import xml.etree.ElementTree as ET
from collections import Counter, OrderedDict

import ge_paths

# The only real ChangeItemRatio table in the tree (see the module docstring).
RATIO_IES = os.path.join(ge_paths.GE, "ies", "JPN", "datatable_changeitem_serv.ies")
ITEMGROUP_XML = os.path.join(ge_paths.GE, "xml", "datatable_itemgroup.xml")
SINGLERULLET_XML = os.path.join(ge_paths.GE, "xml", "datatable_singlerullet.xml")
CATALOG = os.path.join(ge_paths.DATA, "item_catalog.json")
CONVERTER = os.path.join(os.path.dirname(ge_paths.ROOT), "GEAndromida", "Converter")
RATIO_XML = os.path.join(ge_paths.CACHE, "datatable_changeitemratio.xml")

DEFAULT_GROUP = "GLimit_Bellem_Cash"


# ---------------------------------------------------------------- table reads

def decode_ratio_table(force: bool = False) -> str:
    """Decode the JPN .ies to XML in `cache\\` (the client's own converter)."""
    if not os.path.exists(RATIO_IES):
        raise SystemExit("missing table: %s" % RATIO_IES)
    if force or not os.path.exists(RATIO_XML) or (
            os.path.getmtime(RATIO_XML) < os.path.getmtime(RATIO_IES)):
        sys.path.insert(0, CONVERTER)
        import ies2
        tmp = "%s.%d.tmp" % (RATIO_XML, os.getpid())
        ies2.ies_to_xml(RATIO_IES, tmp, None, None, "UTF-8", False)
        os.replace(tmp, RATIO_XML)
    return RATIO_XML


def load_blocks(path: str) -> "OrderedDict[str, list[tuple[int, int]]]":
    """`TableName -> [(Group, Ratio), ...]` in row order.

    The row that carries `TableName` OPENS a block and is not a candidate; the
    rows after it (TableName="None") are its weights - the split
    `CItemGroupTable::InitGambleRatio` makes at ItemGroupTable.cpp:157-181.
    """
    blocks: "OrderedDict[str, list[tuple[int, int]]]" = OrderedDict()
    cur = None
    for row in ET.parse(path).getroot():
        a = row.attrib
        name = a.get("TableName", "")
        if name and name != "None":
            cur = name
            blocks[cur] = []
        elif cur is not None:
            blocks[cur].append((int(a["Group"]), int(a["Ratio"])))
    return blocks


def load_itemgroup(path: str) -> dict[int, list[dict]]:
    """`Group -> [row, ...]` over every ItemGroup row (2,706 groups)."""
    groups: dict[int, list[dict]] = {}
    for row in ET.parse(path).getroot():
        groups.setdefault(int(row.attrib["Group"]), []).append(row.attrib)
    return groups


def load_singlerullet(path: str) -> tuple[dict[int, dict], dict[str, dict]]:
    by_id, by_name = {}, {}
    for row in ET.parse(path).getroot():
        a = row.attrib
        by_id[int(a["ClassID"])] = a
        by_name[a["ClassName"]] = a
    return by_id, by_name


# ------------------------------------------------------------------ the roll

def group_weights(rows: list[tuple[int, int]], point: int,
                  backpoint: int) -> tuple[list[tuple[int, int, int]], int]:
    """Apply Point/BackPoint and the +1 on the first row; return (weights, S).

    Each item is `(group, ratio, selections)` where `selections` is how many of
    the S+1 equally likely `r` values land on that row.
    """
    sliced = rows[point:len(rows) - backpoint] if backpoint else rows[point:]
    out, running = [], 0
    for idx, (group, ratio) in enumerate(sliced):
        running += ratio
        picks = ratio + 1 if idx == 0 else ratio
        out.append((group, ratio, picks))
    return out, running


def distribution(group_rows: dict[int, list[dict]], weights, total: int):
    """`[(item, count, p_group, p_item, group)]` for one roll."""
    out = []
    for group, ratio, picks in weights:
        members = group_rows.get(group) or []
        if not members:
            continue
        p_group = picks / (total + 1)
        for m in members:
            out.append((m["ItemClassName"], int(float(m["ItemCount"])),
                        p_group, p_group / len(members), group))
    return out


def item_totals(rows) -> "OrderedDict[tuple[str, int], float]":
    """Item-level odds, summed over groups (an item can sit in two groups)."""
    totals: "OrderedDict[tuple[str, int], float]" = OrderedDict()
    for name, count, _pg, p_item, _g in rows:
        key = (name, count)
        totals[key] = totals.get(key, 0.0) + p_item
    return totals


# ------------------------------------------------------------------- reports

def missing_mapping(name: str, blocks) -> str:
    return (
        "STOP - the name -> Group mapping for %r is a GAP.\n"
        "  Needs: a `ChangeItemRatio` row with TableName=%r and its (Group, Ratio)\n"
        "         rows (columns TableName, LimitGroup, Group, Ratio, RetryItem,\n"
        "         RetryItemCount) in an .ies with idspace `ChangeItemRatio`.\n"
        "  Have:  %s\n"
        "         idspace ChangeItemRatio, %d blocks, and %r is not one of them.\n"
        "  The rev136 client ships no ChangeItemRatio table at all: its\n"
        "  datatable_changeitem_serv.ies (ies10000.ipf) is the 382-byte\n"
        "  ChangeItemRatioDelect stub. Do NOT default the group - an invented\n"
        "  group is an invented item."
        % (name, name, RATIO_IES, len(blocks), name))


def print_group(name: str, blocks, group_rows, catalog, point=0, backpoint=0) -> int:
    rows = blocks.get(name)
    if not rows:
        print(missing_mapping(name, blocks))
        return 2
    weights, total = group_weights(rows, point, backpoint)
    if not weights:
        print("STOP - %r has %d rows; Point=%d BackPoint=%d leaves none "
              "(GetGiftGroupBYProperty returns group 0 -> GetGiftProp(0) = NULL, "
              "a failed roll)." % (name, len(rows), point, backpoint))
        return 2

    print("ChangeItemRatio block %r: %d rows, Point=%d BackPoint=%d"
          % (name, len(rows), point, backpoint))
    print("  source: %s" % RATIO_IES)
    print("  selection: r = RandomInt(0, %d) -> %d equally likely values; the "
          "first row also owns r=0" % (total, total + 1))
    print()
    print("  %-7s %-7s %-14s %-9s %s" % ("group", "ratio", "P(group)", "items", "contents"))
    for group, ratio, picks in weights:
        members = group_rows.get(group) or []
        print("  %-7d %-7d %-14s %-9s %s" % (
            group, ratio, "%.4f%%" % (100.0 * picks / (total + 1)), len(members),
            ", ".join("%s x%d" % (m["ItemClassName"], int(float(m["ItemCount"])))
                      for m in members) or "<no ItemGroup rows - would fail>"))

    out = distribution(group_rows, weights, total)
    print()
    print("  one roll, item level (a group with N rows splits 1/N):")
    print("  %-34s %-5s %-10s %s" % ("item", "count", "P(item)", "via group"))
    seen = Counter()
    for name_, count, _pg, p_item, group in sorted(out, key=lambda r: -r[3]):
        seen[(name_, count)] += 1
        print("  %-34s %-5d %-10s %d" % (name_, count, "%.4f%%" % (100.0 * p_item), group))

    totals = item_totals(out)
    print()
    print("  item totals over %d picks: %d distinct items, P sums to %.6f"
          % (len(out), len(totals), sum(totals.values())))
    dupes = {k: v for k, v in totals.items() if seen[k] > 1}
    if dupes:
        print("  (an item reached from more than one group is summed: %s)"
              % ", ".join("%s x%d = %.4f%%" % (k[0], k[1], 100.0 * v)
                          for k, v in dupes.items()))

    if catalog:
        names = {n for n, _c, _p, _pi, _g in out}
        unknown = sorted(n for n in names if n not in catalog)
        print("  item names in the rev136 Item table: %d/%d%s"
              % (len(names) - len(unknown), len(names),
                 "" if not unknown else "  UNKNOWN: %s" % unknown))
    return 0


def print_box(key: str, blocks, group_rows, by_id, by_name, catalog) -> int:
    row = by_id.get(int(key)) if key.isdigit() else by_name.get(key)
    if row is None:
        print("STOP - no SingleRullet row for %r.\n"
              "  datatable_singlerullet.ies has %d rows (max ClassID %d, not contiguous);\n"
              "  the ExecuteSingleRullet call site names a ClassID the shipped table does\n"
              "  not carry. That is a second GAP, not a mapping gap."
              % (key, len(by_id), max(by_id)))
        return 2
    print("SingleRullet ClassID %s  ClassName %s  ItemGroupClass %s  "
          "Point %s  BackPoint %s  ConsumeItem %s x%s"
          % (row["ClassID"], row["ClassName"], row["ItemGroupClass"],
             row.get("Point", "0"), row.get("BackPoint", "0"),
             row.get("ConsumeItem", "0"), row.get("ConsumeItemCount", "0")))
    print()
    return print_group(row["ItemGroupClass"], blocks, group_rows, catalog,
                       int(row.get("Point", 0) or 0), int(row.get("BackPoint", 0) or 0))


def print_coverage(blocks, by_id) -> int:
    import re
    call = re.compile(r"ExecuteSingleRullet\s*\(\s*self\s*,\s*(\d+)", re.IGNORECASE)
    calls: Counter = Counter()
    for base, _dirs, files in os.walk(ge_paths.GE_SCRIPT):
        for fn in files:
            if fn.lower().endswith(".scp"):
                with open(os.path.join(base, fn), encoding="utf-8",
                          errors="replace") as fh:
                    for hit in call.findall(fh.read()):
                        calls[int(hit)] += 1
    known = {c: by_id[c] for c in calls if c in by_id}
    resolved = [c for c, r in known.items() if r["ItemGroupClass"] in blocks]
    print("ExecuteSingleRullet call sites: %d, distinct ClassID: %d"
          % (sum(calls.values()), len(calls)))
    print("  with a SingleRullet row:      %d" % len(known))
    print("  whose group name resolves:    %d  (%d call sites)"
          % (len(resolved), sum(calls[c] for c in resolved)))
    print("  mapping missing (GAP):        %d  (%d call sites)"
          % (len(known) - len(resolved),
             sum(calls.values()) - sum(calls[c] for c in resolved)))
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--group", help="ChangeItemRatio TableName (default %s)" % DEFAULT_GROUP)
    ap.add_argument("--box", help="SingleRullet ClassID or ClassName")
    ap.add_argument("--coverage", action="store_true",
                    help="count the script call sites this data resolves")
    ap.add_argument("--no-catalog", action="store_true",
                    help="skip checking item names against the Item table")
    args = ap.parse_args(argv)

    group_rows = load_itemgroup(ITEMGROUP_XML)
    by_id, by_name = load_singlerullet(SINGLERULLET_XML)
    blocks = load_blocks(decode_ratio_table())

    catalog = None
    if not args.no_catalog and os.path.exists(CATALOG):
        import json
        with open(CATALOG, encoding="utf-8") as fh:
            catalog = json.load(fh)["items"]

    if args.coverage:
        return print_coverage(blocks, by_id)
    if args.box:
        return print_box(args.box, blocks, group_rows, by_id, by_name, catalog)
    return print_group(args.group or DEFAULT_GROUP, blocks, group_rows, catalog)


if __name__ == "__main__":
    raise SystemExit(main())
