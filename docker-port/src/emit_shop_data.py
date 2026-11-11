r"""Write the four files the Go zone needs to BUY and SELL: prices, Feso prices,
shop groups, custom shops.

WHY DATA FILES: the shipped client resolves a normal shop's item list and price
ITSELF, so the server only has to send a group name. Our Go zone has no client
tables, so the two lookups the client does - "what does an item cost" and "which
items are in group X" - have to be replayed from data we emit at build time. This
is the same arrangement as `emit_item_catalog.py`: the Python side reads the
tables once, the Go server reads JSON.

The four files, and where each number comes from:

1. `data\item_prices.json` - `{class_id: [price, sell_price, venderable, max_stack, name]}`
   read from `cache\item_price_cache.json` (version 6). That cache is ALREADY
   built from the client's item xml by `item_prices.build_cache()` - this script
   only re-orders it into the shape the Go struct wants, so nothing is decoded
   twice and no `.ies` file is touched.

2. `data\item_feso_prices.json` - `{class_id: [feso_price, feso_sell_price,
   feso_venderable]}`, the SAME cache, slots 5/6/7. This is the column a
   SHOP_FESO shop prices from instead of `Price` (`CTxBuyItem`: `price =
   property->price` for SHOP_VIS, `fesoPrice` for SHOP_FESO). Rows whose
   `FesoPrice` is the INT32_MAX "not sold for Feso" sentinel are LEFT OUT: a
   missing id means "not purchasable with Feso", which is exactly what the
   sentinel says, and emitting 2147483647 would invite a 2.1-billion bill.

3. `data\shop_groups.json` - `{GroupName: [itemID, ...]}` from
   `datatable_Shop.ies`. A `ShowNPCTradeDlg` carries the GROUP and the client
   looks the ids up in this table (see `npc_data.load_shop_items`).

4. `data\custom_shops.json` - `{GroupName: [[sell, price1, count1, price2, count2], ...]}`
   from `ge\server_xml\datatable_custom_shop.xml` (see
   `npc_data.load_custom_shop`). A custom shop is the opposite case: the PACKET
   carries the items and their prices, so the server must know them.

Each file is DERIVED data: regenerate it, never hand-edit it.

A missing source is not a crash: it writes an empty file with `count: 0` and a
loud warning, so a build on a machine without the game tree still succeeds and
the Go side sees "no data" instead of a stale file.

Usage
-----
    python emit_shop_data.py            # -> the four data\*.json files
    python emit_shop_data.py --print    # also show known groups and item ids
"""

import argparse
import json
import os
import sys
import time

import npc_data
from ge_paths import CACHE, DATA, ensure

PRICE_CACHE = os.path.join(CACHE, "item_price_cache.json")
PRICE_VERSION = 6

OUT_PRICES = os.path.join(DATA, "item_prices.json")
OUT_FESO = os.path.join(DATA, "item_feso_prices.json")
OUT_GROUPS = os.path.join(DATA, "shop_groups.json")
OUT_CUSTOM = os.path.join(DATA, "custom_shops.json")

# The cache row order written by `item_prices.build_cache()`:
#   (price, max_stack, sell_price, venderable, label, feso, feso_sell, feso_vend)
# The emitted order the Go struct wants:
#   [price, sell_price, venderable, max_stack, name]
_CACHE_TO_OUT = (0, 2, 3, 1, 4)

# The Feso row the Go side reads for a SHOP_FESO shop, same cache tuple.
#   [feso_price, feso_sell_price, feso_venderable]
_CACHE_FESO = (5, 6, 7)

# `FesoPrice` at or above INT32_MAX means "this item is not sold for Feso" (the
# client's own convention, `item_prices.FESO_NONE`). Measured 2026-10-26: 22,154
# rows carry exactly 2147483647 and 527 carry 2147483648..2147483656 - the same
# marker, shifted - so the test is `>=`. Such a row is not emitted.
FESO_NONE = 2147483647

SHOW_GROUP = "ETC_LEONARDO"
SHOW_ITEMS = (7201, 21450, 30000)
# A Feso shop item, and the same item the Vis table also sells.
SHOW_FESO_ITEMS = (21006, 23234, 75317, 23553)


def _warn(msg):
    print("WARNING: " + msg, file=sys.stderr)


def _write(path, doc):
    """Write one JSON doc atomically; return its size in bytes."""
    ensure(os.path.dirname(path))
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"), sort_keys=True)
    os.replace(tmp, path)
    return os.path.getsize(path)


def _cache():
    """The `items` dict of the price cache, or {} with a loud warning."""
    if not os.path.exists(PRICE_CACHE):
        _warn("%s is missing - run item_prices.py to build it; writing 0 rows"
              % PRICE_CACHE)
        return {}
    with open(PRICE_CACHE, encoding="utf-8") as fh:
        data = json.load(fh)
    if data.get("version") != PRICE_VERSION:
        _warn("%s is version %r, expected %d - rebuilding is item_prices.py's job, "
              "read anyway" % (PRICE_CACHE, data.get("version"), PRICE_VERSION))
    return data.get("items") or {}


def prices():
    """{class_id: [price, sell_price, venderable, max_stack, name]} from the cache."""
    out = {}
    for key, row in _cache().items():
        try:
            out[str(int(key))] = [row[i] for i in _CACHE_TO_OUT]
        except (IndexError, TypeError, ValueError):
            # One malformed row must not cost the other 25k.
            continue
    return out


def feso_prices():
    """{class_id: [feso_price, feso_sell_price, feso_venderable]} from the cache.

    A Feso shop prices from these columns, not from `Price` (`CTxBuyItem`:
    `price = property->price` for SHOP_VIS, `fesoPrice` for SHOP_FESO), so a
    Feso shop cannot be opened without them.

    The "not sold for Feso" marker (INT32_MAX and up) is DROPPED, not clamped: it
    means the item is not sold for Feso, so an id missing from this file is the
    honest answer, while a literal 2147483647 price would be a 2.1-billion bill.
    Measured 2026-10-26: 4,303 of 27,922 rows carry a real Feso price, and all
    600 items of the group `FESO_ITEMSHOP` are among them.
    """
    out = {}
    for key, row in _cache().items():
        try:
            values = [row[i] for i in _CACHE_FESO]
        except (IndexError, TypeError, ValueError):
            continue
        if values[0] >= FESO_NONE:
            continue
        out[str(int(key))] = values
    return out



def shop_groups():
    """{GroupName: [itemID, ...]} in the TABLE's own order, from datatable_Shop.ies.

    DO NOT SORT THIS. A buy record carries the item's POSITION in the shop the
    client drew, so the server's list must be in exactly the client's order.
    Sorting by id reorders the shop and makes every click buy the item that
    sits at that position in OUR order - measured 2026-10-26: index 0 became
    20022 instead of the client's 21006, and a warp-scroll click bought a
    different item.
    """
    try:
        rows = npc_data.load_shop_items()
    except FileNotFoundError as exc:
        _warn("datatable_Shop.ies not found (%s); writing 0 groups" % exc)
        return {}
    if not rows:
        _warn("datatable_Shop.ies decoded to 0 groups; writing an empty file")
    return {group: list(items) for group, items in rows.items()}


def custom_shops():
    """{GroupName: [[sell, price1, count1, price2, count2], ...]} from the server xml."""
    try:
        rows = npc_data.load_custom_shop()
    except OSError as exc:
        _warn("%s not readable (%s); writing 0 groups"
              % (npc_data.CUSTOM_SHOP_XML, exc))
        return {}
    if not rows:
        _warn("%s gave 0 groups; writing an empty file" % npc_data.CUSTOM_SHOP_XML)
    # Tuples are not JSON; a list per row keeps the column order visible.
    return {group: [list(row) for row in items] for group, items in rows.items()}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--print", action="store_true", dest="show")
    args = ap.parse_args()

    started = time.time()

    item_rows = prices()
    size_prices = _write(OUT_PRICES, {
        "source": "cache\\item_price_cache.json v%d (client item xml, via item_prices.py)"
                  % PRICE_VERSION,
        "count": len(item_rows),
        "items": item_rows,
    })

    feso_rows = feso_prices()
    size_feso = _write(OUT_FESO, {
        "source": "cache\\item_price_cache.json v%d (client item xml, via "
                  "item_prices.py) - FesoPrice/FesoSellPrice/FesoVenderable"
                  % PRICE_VERSION,
        "rule": "a class id absent here is NOT sold for Feso "
                "(FesoPrice >= 2147483647 in the client table)",
        "count": len(feso_rows),
        "items": feso_rows,
    })

    groups = shop_groups()
    size_groups = _write(OUT_GROUPS, {
        "source": "ge\\ies\\datatable_Shop.ies via npc_data.load_shop_items",
        "count": len(groups),
        "groups": groups,
    })

    customs = custom_shops()
    size_custom = _write(OUT_CUSTOM, {
        "source": "ge\\server_xml\\datatable_custom_shop.xml via npc_data.load_custom_shop",
        "count": len(customs),
        "groups": customs,
    })

    elapsed = time.time() - started
    print("item_prices:   %6d row(s), %9d bytes -> %s"
          % (len(item_rows), size_prices, OUT_PRICES))
    print("item_feso:     %6d row(s), %9d bytes -> %s"
          % (len(feso_rows), size_feso, OUT_FESO))
    print("shop_groups:   %6d group(s), %9d bytes -> %s"
          % (len(groups), size_groups, OUT_GROUPS))
    print("custom_shops:  %6d group(s), %9d bytes -> %s"
          % (len(customs), size_custom, OUT_CUSTOM))
    print("total %d bytes in %.2f s (derived data - regenerate, never hand-edit)"
          % (size_prices + size_feso + size_groups + size_custom, elapsed))

    if args.show:
        print("  group %s: %d item(s) %s"
              % (SHOW_GROUP, len(groups.get(SHOW_GROUP, [])),
                 groups.get(SHOW_GROUP, [])[:8]))
        for item_id in SHOW_ITEMS:
            print("  item %-6d -> %s" % (item_id, item_rows.get(str(item_id))))
        for item_id in SHOW_FESO_ITEMS:
            print("  item %-6d feso -> %s"
                  % (item_id, feso_rows.get(str(item_id))))
        if customs:
            first = sorted(customs)[0]
            print("  custom group %s: %d row(s) %s"
                  % (first, len(customs[first]), customs[first][:3]))


if __name__ == "__main__":
    main()
