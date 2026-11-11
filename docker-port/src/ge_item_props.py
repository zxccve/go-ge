"""ge_item_props - the imcIES::CObject wire form for an item's properties.

Shared by the ZONE server (which pushes the bag's item properties) and the
BARRACK server (which pushes the properties of the items a character wears).

It used to live in `zone_server.py`. It was moved out because the barrack
needs the same serializer, and a second copy of a wire format is a second
copy of a bug. Nothing changed for the zone: the names are the same.
"""
import struct

# Per-key wire type, read from the CLIENT's own getters (`UISession.cpp`
# `GetItemAdditionalProperty` -> `->Get("<key>", X)`). A mismatch is fatal in
# the client: `CObject::Get` rejects a STRING entry for a float getter and a
# NUMBER entry for a CStringID getter, logs "can't get <key>", returns false.
# Keys not listed here fall back to the IES rule below.
ITEM_PROPERTY_TYPES = {
    "Creater": "string",        # UIUtil.cpp:1155    -> Get("Creater", CStringID&)
    "PrevEnchant": "string",    # UIScriptFunc.cpp:1402 -> Get("PrevEnchant", CStringID&)
}

# The class every inventory item's property object carries. Taken from the
# original server's own item class: `imcIES::CreateObject("Item_Property", 1)`
# (`ZServerLib\InvItem.cpp:267` and `:1265`). The client resolves the pair
# through its `datatable_item_additional_prop` table.
ITEM_PROPERTY_IDSPACE = b"Item_Property"
ITEM_PROPERTY_CLASSID = 1


def _prop_is_number(value):
    """True when the IES library would type this text as a NUMBER.

    The rule (`imcIES` `IsNumberValue`): the text is a number when it is
    digits and at most one '.'. So "1" and "-3.5" are numbers; "" is not (no
    token), and neither is "120071 , 12," (several tokens).
    """
    tok = (value or "").split()
    if len(tok) != 1:
        return False
    t = tok[0]
    if t.startswith("-"):
        t = t[1:]
    if not t:
        return False
    return t.replace(".", "", 1).isdigit() and t.count(".") <= 1


def item_property_type(name, value):
    """'number' or 'string' for one property row (see the table above).

    NOTE: ProtectIcon looks like an int getter (`UIItemListBox.cpp:2033`) but is
    NOT forced to number here. The original IES library types a value by its
    TEXT (`GetValueType` -> `IsNumberValue`), and the DB holds ProtectIcon as
    "1", so the original server would also send it as NUMBER. Forcing it to
    number is what we tried first and it is untested; the IES rule is the
    behaviour we can justify.
    """
    forced = ITEM_PROPERTY_TYPES.get(name)
    if forced:
        return forced
    return "number" if _prop_is_number(value) else "string"


def build_item_property_object(props):
    r"""imcIES::CObject wire form for one item's properties.

    Layout (`imcIES\source\Object.cpp:607 Serialize` / `:672 Extract`):
        int classCount; int propertyCount;
        classCount x { NUL-terminated idspace; DWORD ClassID }
        propertyCount x { BYTE keyLen; char key[keyLen]; char type; value }
    `type` 0 = NUMBER (double), 1 = STRING, 2 = CALCULATED (string on the wire).
    keyLen/valueLen are `strlen+1` and INCLUDE the NUL.

    **NUMBER is a DOUBLE (8 bytes), not a float.** The shipped source
    (`imcIES\source\Object.cpp:648`) writes `sizeof(float)`, but the CLIENT is a
    newer build and reads 8 bytes - disassembly of its own extractor
    (`FUN_0059cde9`, the function containing the 0x19ce2d crash) does
    `PUSH 0x8; CALL Read(8); MOVSD XMM0, qword ptr [EAX]`. Writing 4 bytes
    desyncs the reader, and the damage shows up in the NEXT record's header,
    not at the value: `Read(4)` runs off the end, returns null, and the class
    loop crashes at `PUSH [EAX]` (`ge.exe+0x19ce2d`). That is why a class-less
    zone_server payload and a correctly-classed payload both died at the same
    address.

    The class is the one the ORIGINAL server puts on an inventory item:
    `imcIES::CreateObject("Item_Property", 1)` (`InvItem.cpp:267` and `:1265`),
    i.e. idspace `Item_Property`, ClassID `1` (`typedef unsigned int ClassID`,
    `imcIES.h:22`, so 4 bytes on the wire). `ZServerLib\Inventory.cpp:466`
    serializes that object as-is.

    A class is NOT optional on the wire. `classCount = 0` was tried on
    2026-10-19 and CRASHED the live client (`0xc0000005` at `ge.exe+0x19ce2d`,
    `"Can't Find BaseClass : %s:%d"`): the client's `CObject::Extract` compares
    the incoming class against its own class table and the caller then uses a
    null. Do not send an object without a resolvable class.
    """
    out = struct.pack("<ii", 1, len(props))
    out += ITEM_PROPERTY_IDSPACE + b"\0" + struct.pack("<I", ITEM_PROPERTY_CLASSID)
    for p in props:
        key = (p["property"] or "").encode("latin-1", "replace")[:255]
        kind = item_property_type(p["property"], p["value"])
        if kind == "number":
            try:
                val = float(p["value"])
            except (TypeError, ValueError):
                val = 0.0
            out += struct.pack("<B", len(key) + 1) + key + b"\0"
            out += struct.pack("<B", 0)          # PROPERTY_ENTRY::NUMBER
            out += struct.pack("<d", val)        # DOUBLE - see the docstring
        else:
            val = (p["value"] or "").encode("latin-1", "replace")[:255]
            out += struct.pack("<B", len(key) + 1) + key + b"\0"
            out += struct.pack("<B", 1)          # PROPERTY_ENTRY::STRING
            out += struct.pack("<B", len(val) + 1) + val + b"\0"
    return out
