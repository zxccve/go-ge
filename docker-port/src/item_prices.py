"""item_prices - item -> (price, max stack) for shop purchases.

The original prices a purchase from the ITEM row itself
(`CTxBuyItem`: `property = geItemTable::Get(shopProp->itemID); price =
property->price` for SHOP_VIS, `fesoPrice` for SHOP_FESO), never from the packet.
Our port has no item table loaded, so this builds a small JSON cache from the
CLIENT's own item tables (the same data the client shows), once.

BOTH currencies are cached, in ONE pass over the same rows (version 6): the VIS
fields (`Price`, `SellPrice`, `Venderable`) at slots 0/2/3, the FESO fields
(`FesoPrice`, `FesoSellPrice`, `FesoVenderable`) at slots 5/6/7. A Feso shop
prices from the Feso fields and a Vis shop from the Vis fields - the row has
both, so decoding the tables twice would only cost time.

`FesoPrice` is INT32_MAX (2147483647) for every item that is NOT sold for Feso;
that is a sentinel, not a price. `feso_price()` returns 0 for it so a caller
cannot accidentally charge 2.1 billion.

The client reads the base tables FIRST and then layers the nation/locale
sub-folder on top of them, where the overlay WINS (`geShared\\Source\\geLocalize.cpp`
picks the nation folder, `Server_Init.cpp` load-then-update). So this does the
same: base pass, then the `DEU` folder as an overlay.

Env:
  GE_ITEM_IES_DIR   folder with datatable_item_*.ies (and the DEU sub-folder)
  GE_ITEM_PRICE_CACHE  JSON cache path
"""
import json
import os
import sys
import tempfile
import xml.etree.ElementTree as ET

from ge_paths import CACHE as CACHE_DIR, GAME_ROOT
IES_DIR = os.environ.get("GE_ITEM_IES_DIR",
                         os.path.join(GAME_ROOT, "_andro_ies_stage", "ies10000"))
CACHE = os.environ.get("GE_ITEM_PRICE_CACHE",
                       os.path.join(CACHE_DIR, "item_price_cache.json"))
CONVERTER = os.environ.get(
    "GE_STANCE_CONVERTER", os.path.join(GAME_ROOT, "GEAndromida", "Converter"))
CACHE_VERSION = 6

# The nation/locale overlay the client layers over the base item tables. A DEU
# row always wins over the base row for the fields it carries - measured
# 2026-10-26: class 21613 Fire stone is SellPrice 1100 in the base and 1 here,
# and the client shows 1 Vis.
DEU_DIR = os.path.join(IES_DIR, "DEU")

# Overlay field -> index in the cache tuple
# (price, stack, sell, vend, label, feso, feso_sell, feso_vend).
# A field the overlay row does NOT carry must keep the base value.
_OVERLAY_FIELDS = (("Price", 0), ("MaxStack", 1), ("SellPrice", 2),
                   ("Venderable", 3), ("FesoPrice", 5), ("FesoSellPrice", 6),
                   ("FesoVenderable", 7))

# The "not sold for Feso" sentinel the client's own table uses. Measured
# 2026-10-26: 22,154 of 27,922 rows carry exactly 2147483647, and 527 more carry
# INT32_MAX + a small offset (2147483648..2147483656) - the same marker, shifted.
# So the test is `>=`, not `==`. Every item in the five FESO_* shop groups
# carries a REAL price instead (see emit_shop_data.py).
FESO_NONE = 2147483647

_PRICES = None


def _item_files(folder):
    """Top-level `datatable_item_*.ies` names in one folder (never recursive)."""
    if not os.path.isdir(folder):
        return []
    return sorted(f for f in os.listdir(folder)
                  if f.startswith("datatable_item_") and f.endswith(".ies"))


def _row_of(c):
    """One <Class> element -> (price, stack, sell, vend, label, feso, feso_sell, feso_vend)."""
    try:
        price = int(c.get("Price") or 0)
    except ValueError:
        price = 0
    try:
        stack = int(c.get("MaxStack") or 1) or 1
    except ValueError:
        stack = 1
    try:
        sell = int(c.get("SellPrice") or 0)
    except ValueError:
        sell = 0
    vend = 1 if (c.get("Venderable") or "").strip().upper() == "TRUE" else 0
    label = c.get("EngName") or c.get("ClassName") or ""
    try:
        feso = int(c.get("FesoPrice") or 0)
    except ValueError:
        feso = 0
    try:
        feso_sell = int(c.get("FesoSellPrice") or 0)
    except ValueError:
        feso_sell = 0
    feso_vend = 1 if (c.get("FesoVenderable") or "").strip().upper() == "TRUE" else 0
    return (price, stack, sell, vend, label, feso, feso_sell, feso_vend)


def build_cache():
    sys.path.insert(0, CONVERTER)
    import ies2  # noqa: E402
    rows = {}

    def load(folder, name, tag):
        """Decode one .ies to xml ONCE and hand back its root, or None."""
        src = os.path.join(folder, name)
        dst = os.path.join(tempfile.gettempdir(),
                           "ge_price_%s_%s.xml" % (tag, name))
        try:
            ies2.ies_to_xml(src, dst, None, None, "UTF-8", False)
            return ET.parse(dst).getroot()
        except Exception as exc:
            print("  skip %s/%s: %s" % (tag, name, exc))
            return None

    # PASS 1 - the base item tables.
    for name in _item_files(IES_DIR):
        root = load(IES_DIR, name, "base")
        if root is None:
            continue
        for c in root.iter("Class"):
            cid = c.get("ClassID")
            if not cid or not cid.isdigit():
                continue
            # datatable_item_set.ies reuses item ClassIDs for set definitions and
            # carries no Price/SellPrice/Venderable. It sorts AFTER the real item
            # files, so a blind overwrite used to replace e.g. 15001
            # CrecementoLeather (sell 1) with 15001 QuiriSet (no numbers) and the
            # sell path then refused a perfectly sellable item.
            if int(cid) in rows and c.get("Price") is None:
                continue
            rows[int(cid)] = _row_of(c)
        print("  %-40s %d rows" % (name, len(root)))

    # PASS 2 - the DEU locale overlay, applied ON TOP of the base pass.
    added = 0
    replaced = 0
    for name in _item_files(DEU_DIR):
        root = load(DEU_DIR, name, "deu")
        if root is None:
            continue
        for c in root.iter("Class"):
            cid = c.get("ClassID")
            if not cid or not cid.isdigit():
                continue
            cid = int(cid)
            fresh = _row_of(c)
            old = rows.get(cid)
            if old is None:
                rows[cid] = fresh
                added += 1
                continue
            merged = list(old)
            for attr, idx in _OVERLAY_FIELDS:
                if c.get(attr) is None:
                    continue        # row does not carry it -> keep the base value
                merged[idx] = fresh[idx]
            # The row's readable name: EngName wins, else ClassName, else keep.
            if c.get("EngName"):
                merged[4] = c.get("EngName")
            elif c.get("ClassName"):
                merged[4] = c.get("ClassName")
            if tuple(merged) != old:
                replaced += 1
            rows[cid] = tuple(merged)
        print("  %-40s %d rows (overlay)" % ("DEU/" + name, len(root)))
    print("  DEU overlay: %d row(s) changed, %d new item(s)"
          % (replaced, added))

    data = {"version": CACHE_VERSION, "items": rows}
    with open(CACHE, "w", encoding="utf-8") as f:
        json.dump(data, f)
    return data


def _load():
    global _PRICES
    if _PRICES is None:
        data = None
        if os.path.exists(CACHE):
            try:
                with open(CACHE, encoding="utf-8") as f:
                    data = json.load(f)
            except (OSError, ValueError):
                data = None
        if not data or data.get("version") != CACHE_VERSION:
            data = build_cache()
        _PRICES = {int(k): tuple(v) for k, v in data["items"].items()}
    return _PRICES


def price(item_id):
    row = _load().get(int(item_id))
    return row[0] if row else 0


def max_stack(item_id):
    row = _load().get(int(item_id))
    return row[1] if row else 1


def sell_price(item_id):
    """SellPrice from the item's own row (CInvItem::GetSellPrice)."""
    row = _load().get(int(item_id))
    return row[2] if row else 0


def venderable(item_id):
    """The item row's Venderable flag; a normal shop refuses the rest."""
    row = _load().get(int(item_id))
    return bool(row[3]) if row else False


def name(item_id):
    """Readable item name: the row's EngName, else its ClassName."""
    row = _load().get(int(item_id))
    return row[4] if row and len(row) > 4 else ""


def feso_price(item_id):
    """FesoPrice from the item's own row - what a SHOP_FESO shop charges.

    0 when the row does not exist OR carries the "not sold for Feso" marker
    (INT32_MAX and up, see `FESO_NONE`), so a caller that forgets to check
    cannot bill 2.1 billion.
    """
    row = _load().get(int(item_id))
    if not row or len(row) < 8:
        return 0
    value = row[5]
    return 0 if value >= FESO_NONE else value


def feso_sell_price(item_id):
    """FesoSellPrice: Feso paid back when the item is sold to a Feso vendor."""
    row = _load().get(int(item_id))
    if not row or len(row) < 8:
        return 0
    return 0 if row[6] >= FESO_NONE else row[6]


def feso_venderable(item_id):
    """The row's FesoVenderable flag (the Feso side of `venderable`)."""
    row = _load().get(int(item_id))
    return bool(row[7]) if row and len(row) > 7 else False


if __name__ == "__main__":
    data = build_cache()
    print("cached %d items -> %s" % (len(data["items"]), CACHE))
    for i in (21106, 21107, 7201, 21450, 21006, 20022):
        print("  %d price=%d stack=%d sell=%d venderable=%s feso=%d "
              "feso_sell=%d feso_vend=%s"
              % (i, price(i), max_stack(i), sell_price(i), venderable(i),
                 feso_price(i), feso_sell_price(i), feso_venderable(i)))

