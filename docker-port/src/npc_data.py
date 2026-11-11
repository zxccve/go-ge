"""npc_data - build the town NPC list for a map zone.

Two problems are solved here:

1. Which model to draw.  `npcgen_<map>.ies` stores no numeric model id: its
   `ClassType` string is the `ClassName` of a row in `datatable_npc.ies`.  That
   row's `ClassID` is the number the spawn packet wants.

2. Where to stand.  `npcgen_<map>.ies` stores a 3D point (PosX/Y/Z), but the
   spawn packet wants WORLDPOS{x, y, cell} - a 2D ground point plus a navmesh
   cell.  The client derives the height from the cell, and rejects a bad one.
   So each point is run through the client's own path engine via
   navmesh/pathtool.exe (32-bit helper around pathengine.dll).

3. Which map.  `datatable_map.ies` turns a zone id into its world name (which
   names the `npcgen_<world>.ies` table) and into its navmesh (`TokName`). Those
   two are NOT always the same: zone 8 `reb_linden` uses the mesh `barrack02`.

4. Where a town quick-move goes.  `datatable_town.ies` (idspace `townpoint`)
   holds four named spots per city (`PointName1..4` / `TownPoint1..4`). The
   NAMES are for the client, which draws those buttons itself; the server only
   needs the points, and they convert to a navmesh cell exactly like npcgen
   rows do (the third number is a height hint, not a cell).

Tables are decoded with the client's own converter (Converter/ies2.py), so we
read the same bytes the game reads.
"""
import os
import subprocess
import sys
import time
import xml.etree.ElementTree as ET

from ge_paths import CACHE as CACHE_DIR, GAME_ROOT

GE = os.path.join(GAME_ROOT, "GEAndromida")
# The game's own data tree - the SERVER's copy, refreshed to the CLIENT's
# generation on 2026-10-22 (R36). The client is the anchor: its tables and
# its dictionary must be one generation, or every name draws as a bare
# number. `ge\ies` was verified hash-identical to the client's own
# `ies10000` (1,489 files) before it was adopted.
#
# The OLD path was GEAndromida\tools\readme\ies - the previous client's
# tables (revision 318), a different generation. Do not go back to it.
IES_DIR = os.path.join(os.path.join(GAME_ROOT, "ge"), "ies")
CONVERTER = os.path.join(GE, "Converter")
sys.path.insert(0, CONVERTER)
import ies2  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
# The navmesh folder sits at the docker-port TOP level (a non-code input, like
# data\ and config\), NOT in src\ - so it must not be resolved from HERE. The
# 2026-10-18 reorg moved this script into src\ and left navmesh\ where it was,
# which broke town NPCs until it was read from the project root again.
ROOT = os.path.dirname(HERE)
PATHTOOL = os.path.join(ROOT, "navmesh", "pathtool.exe")
# Meshes are searched here first (copied in so the port stands alone), then in
# the unpacked distribution they came from.
MESH_DIRS = [
    os.path.join(ROOT, "navmesh", "mesh"),
    os.path.join(GAME_ROOT, "ge draken and laval", "ge", "bg"),
]

# The client's Map row for this zone names the navmesh; keep it in one place.
DEFAULT_MESH = "reb_basic"

# Half-size of a player character's collision body: geShared\Source\ShapeTable.cpp
# builds shape "S" with radius 50 (M=100, L=200, XL=400). A path query with a
# smaller circle returns routes a real character cannot walk, so this is the one
# number that makes `find_paths` trustworthy.
ACTOR_RADIUS = 50

# The real server asks for the nearest ground point within 1000 horizontally
# and 300 vertically (World.cpp InitTriggerObject -> Get3DPointToPosLocaly).
H_RANGE = 1000
V_RANGE = 300


def byte_angle(degrees):
    """Degrees -> the 0..255 angle the wire uses.

    `npcgen_<map>.ies` stores Direction in DEGREES (values above 255 occur), but
    the packet field is a BYTE. The real server converts it in
    `CTriggerObject::Init` via `GetByteAngle` (gePacketBase.h):
        (BYTE)((deg / 360.0f) * 255.0f + 0.5f)
    Sending the raw degrees makes the client read back the wrong angle, so the
    NPC faces the wrong way.
    """
    return int((float(degrees) / 360.0) * 255.0 + 0.5) & 0xFF


def _decode_to(src, out):
    """Convert a .ies to XML ATOMICALLY.

    Two processes can decode the same table at the same time (the zone
    server and check_data, now that both use cache\\). Writing straight to
    `out` lets a reader pick up a HALF-written file, and a truncated XML
    shows up deep inside the caller as an IndexError on an empty row list -
    a failure that looks like a game bug and is not one. Write beside it and
    rename: the rename is atomic, so a reader sees either the old file or
    the new one, never a broken one.

    On Windows the rename can still fail with "Access is denied" if another
    process happens to hold the destination open for reading, so retry.
    """
    tmp = "%s.%d.tmp" % (out, os.getpid())
    try:
        ies2.ies_to_xml(src, tmp, None, None, "UTF-8", False)
        for attempt in range(20):
            try:
                os.replace(tmp, out)
                return
            except PermissionError:
                if attempt == 19:
                    raise
                time.sleep(0.05 * (attempt + 1))
    finally:
        if os.path.exists(tmp):
            try:
                os.remove(tmp)
            except OSError:
                pass


def _decode(table, cache_dir=None):
    """Decode <table>.ies to XML with the client's converter; return the root.

    The decoded XML is SCRATCH, not source: it is regenerated from the .ies
    whenever the .ies changes. It therefore lands in cache\\ (git-ignored),
    never next to the script - that is what kept 27 .xml files piling up in
    src\\ and made the layout checker cry.
    """
    out = os.path.join(cache_dir or CACHE_DIR, table + ".xml")
    if not os.path.exists(out):
        src = os.path.join(IES_DIR, table + ".ies")
        if not os.path.exists(src):
            raise FileNotFoundError(src)
        _decode_to(src, out)
    try:
        return ET.parse(out).getroot()
    except ET.ParseError:
        # A truncated cache file from an older non-atomic write: drop it and
        # decode again rather than serving a broken table.
        os.remove(out)
        _decode_to(src, out)
        return ET.parse(out).getroot()


_idspace_stems = None


def idspace_stems(ies_dir=None):
    """`{lowercased idspace: [table stem, ...]}` over EVERY `.ies` file.

    **The idspace is NOT the file name.** `datatable_missionenter.ies` carries
    the idspace `Mission_Rank`, and 140 of the 316 datatable files disagree
    with their stem - so a lookup keyed on the stem silently answers 0
    (`GetDataTableNumberValue('Mission_Rank', id, 'Rank')`, 117 call sites).

    The name is PLAIN TEXT in the first 64 bytes of the .ies, NUL-terminated,
    and it agrees with the decoded XML root `id` on all 316 datatable files
    (checked 2026-10-24), so the header answers the question with no decode.

    Not only `datatable_*`: a `mongenflag_<map>.ies` header is the map name
    itself (`MonGenFlag_btl_bah05`), and scripts ask for that name - so the
    whole `.ies` folder is indexed.
    """
    global _idspace_stems
    if _idspace_stems is not None:
        return _idspace_stems
    table = {}
    base = ies_dir or IES_DIR
    try:
        for fname in os.listdir(base):
            if not fname.lower().endswith(".ies"):
                continue
            stem = fname[:-4]
            try:
                with open(os.path.join(base, fname), "rb") as f:
                    name = f.read(64).split(b"\x00")[0]
                name = name.decode("ascii", "replace").strip()
            except OSError:
                continue
            if not name:
                continue
            table.setdefault(name.lower(), []).append(stem)
    except OSError:
        pass
    for stems in table.values():
        stems.sort()
    _idspace_stems = table
    return table


def _stems_by_name(ies_dir=None):
    """`{lowercased stem: stem}` - the files by their OWN name."""
    base = ies_dir or IES_DIR
    out = {}
    try:
        for fname in os.listdir(base):
            if fname.lower().endswith(".ies"):
                out[fname[:-4].lower()] = fname[:-4]
    except OSError:
        pass
    return out


def tables_for_idspace(idspace, ies_dir=None):
    """The table stems to read for `idspace`, best match first.

    The engine keys a table BOTH by its idspace and by its file name, and
    scripts use both spellings (`'Mission_Rank'` is a file `datatable_missionenter`,
    `'MonGenFlag_btl_bah05'` is a file of that name). So: idspace first, then a
    file whose own stem is the name.

    One idspace can span several files (8 of the 261 datatable idspaces do).
    A file whose stem IS the idspace wins, because the rest are
    PROPERTY-EXTENSION tables, not continuations: all 21 `Commander` files hold
    a single unrelated `ClassID=1` row, so merging them would answer with the
    wrong row (`Item`, 26 files, has 33 real ClassID collisions). Only when no
    file carries the idspace as its own stem (`MonsterShape` ->
    `datatable_monster_shape` + `datatable_pet_shape`) is the group merged.
    """
    key = str(idspace or "").lower()
    stems = idspace_stems(ies_dir).get(key, [])
    exact = [s for s in stems if s.lower() == key]
    if exact:
        return exact
    if stems:
        return stems
    stem = _stems_by_name(ies_dir).get(key)
    return [stem] if stem else []


_map_cache = {}


def load_map_table(cache_dir=None):
    """datatable_map.ies: zone id -> (world name, mesh name).

    The placement table is `npcgen_<world name>` (CWorld::InitTriggerObject
    builds that name from GetWorldName()), while the navmesh is
    `<TokName>.tok` (CWorld::CreateGround). Zone 8 `reb_linden` uses the mesh
    `barrack02`, so the two must not be conflated.
    """
    key = cache_dir or ""
    if key not in _map_cache:
        rows = {}
        for cls in _decode("datatable_map", cache_dir).iter("Class"):
            try:
                zid = int(cls.get("ClassID"))
            except (TypeError, ValueError):
                continue
            name = cls.get("ClassName")
            if name:
                rows[zid] = (name, cls.get("TokName") or name)
        _map_cache[key] = rows
    return _map_cache[key]


def load_npc_types(cache_dir=None):
    """datatable_npc.ies: ClassName -> (ClassID, Size, Range)."""
    root = _decode("datatable_npc", cache_dir)
    types = {}
    for cls in root.iter("Class"):
        name = cls.get("ClassName")
        if name:
            types[name] = (int(cls.get("ClassID")),
                           cls.get("Size", "S"),
                           int(cls.get("Range") or 0))
    return types


def load_npc_gen(map_name, cache_dir=None):
    """npcgen_<map>.ies -> one dict per placed NPC, hidden rows dropped."""
    root = _decode("npcgen_" + map_name, cache_dir)
    rows = []
    for cls in root.iter("Class"):
        if (cls.get("Hide") or "FALSE").upper() == "TRUE":
            continue
        rows.append({
            "gen_id": int(cls.get("ClassID")),
            "class_type": cls.get("ClassType"),
            # int() first: World.cpp reads Direction as a float and the call to
            # Init() narrows it to WORD before GetByteAngle, so 214.7 -> 214.
            "direction": byte_angle(int(float(cls.get("Direction") or 0))),
            "x3d": int(float(cls.get("PosX") or 0)),
            "y3d": int(float(cls.get("PosY") or 0)),
            "z3d": int(float(cls.get("PosZ") or 0)),
            "script": cls.get("Script", "None"),
            # A locale token like "<$>136793</>", not readable text. The client
            # resolves it when it draws a name, so we can pass it straight
            # through to the player (see CBaseObject::TriggerObjectEnterWorld).
            "name": cls.get("Name", ""),
        })
    return rows


def find_mesh(mesh):
    """Absolute path of a map's .tok navmesh, or None."""
    for d in MESH_DIRS:
        p = os.path.join(d, mesh + ".tok")
        if os.path.exists(p):
            return p
    return None


def find_paths(start, goals, mesh=DEFAULT_MESH, radius=ACTOR_RADIUS):
    """Shortest WALKABLE path from `start` to each goal, via the client's engine.

    `start` and each goal are (x, y, cell) WORLDPOS points. Returns a list the
    same length as `goals`; an entry is None when the engine found NO route, or
    (points, length) where points is the polyline start..goal in (x, y, cell)
    and length is its length in world units.

    Why this exists: the client's answer to ZC_MOVE_PC is a PATH move
    (`CActor::MoveTo` sets `CMS_PATH_MOVE`, GeActor.cpp:249), so a destination
    it cannot route to leaves the actor standing - the packet animates nothing
    and the character "won't move". `positionIsValid` (what `resolve_cells`
    uses) only says the point sits on a mesh triangle; it does NOT say a body
    of the actor's size can WALK there. Only a path query answers that, and the
    two disagree in practice: the point 140 units from NPC #49 (the wagon
    Leonardo) is valid ground, but NO path reaches it with a 50-unit body while
    the point 180 units out is fine.

    The shape matters. `radius` is the actor's half-size from the shape table
    (geShared\\Source\\ShapeTable.cpp: S=50, M=100, L=200, XL=400) - a path for a
    20-unit body can squeeze through gaps a 50-unit one cannot, so do not tune
    this to make a stubborn target work.
    """
    if not goals:
        return []
    mesh_path = find_mesh(mesh)
    if not mesh_path:
        raise FileNotFoundError("no %s.tok in %s" % (mesh, MESH_DIRS))
    if not os.path.exists(PATHTOOL):
        raise FileNotFoundError(PATHTOOL)

    lines = ["path %d %d %d %d %d %d %d"
             % (start[0], start[1], start[2], g[0], g[1], g[2], radius)
             for g in goals]
    lines.append("quit")
    proc = subprocess.run([PATHTOOL, mesh_path],
                          input="\n".join(lines).encode(),
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    out = proc.stdout.decode(errors="replace").splitlines()

    # One `path ok <n> radius=... length=...` line per goal, each followed by
    # `  pt <i> x y cell` while it walks the polyline. `path none` is a goal the
    # engine refused outright (no ground under it); `points=0` is "no route".
    results = []
    i = 0
    while i < len(out):
        ln = out[i]
        if ln.startswith("path ok "):
            f = ln.split()
            n = int(f[2])
            length = int(f[4].split("=")[1])
            pts = []
            for j in range(n):
                i += 1
                g = out[i].split()
                pts.append((int(g[2]), int(g[3]), int(g[4])))
            results.append((pts, length) if pts else None)
        elif ln.startswith("path none") or ln.startswith("path err"):
            results.append(None)
        i += 1
    if len(results) != len(goals):
        raise RuntimeError("pathtool answered %d of %d path queries"
                           % (len(results), len(goals)))
    return results


def clamp_point(start, point, mesh=DEFAULT_MESH, radius=ACTOR_RADIUS):
    """The last unobstructed spot from `start` towards `point`, or None.

    This is the original server's `GetValidPos` (imcPathEngine\\Ground.cpp):
    `firstCollision` along the segment, take the intersection, then nudge out of
    it with `findClosestUnobstructedPoint`. It is how an NPC-click chase point is
    derived: the ideal point is measured OUTWARD FROM THE NPC, so an obstacle
    beside the NPC shortens the gap instead of making the pose unreachable.

    Returns (x, y, cell), or None when the START itself is not valid ground.
    """
    mesh_path = find_mesh(mesh)
    if not mesh_path:
        raise FileNotFoundError("no %s.tok in %s" % (mesh, MESH_DIRS))
    if not os.path.exists(PATHTOOL):
        raise FileNotFoundError(PATHTOOL)
    lines = ["clamp %d %d %d %d %d %d"
             % (start[0], start[1], start[2], point[0], point[1], radius), "quit"]
    proc = subprocess.run([PATHTOOL, mesh_path],
                          input="\n".join(lines).encode(),
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    for ln in proc.stdout.decode(errors="replace").splitlines():
        if ln.startswith("clamp ok "):
            f = ln.split()          # clamp ok <x> <y> <cell> <free|hit>
            return ((int(f[2]), int(f[3]), int(f[4])), f[5] == "hit")
        if ln.startswith("clamp none"):
            return (None, False)
    raise RuntimeError("pathtool gave no clamp answer for %s -> %s" % (start, point))


def resolve_cells(points, mesh=DEFAULT_MESH, h_range=H_RANGE, v_range=V_RANGE):
    """[(x, y, z)] -> [(x, y, cell)] using the client's path engine.

    Returns the same length; an entry is None when the engine found no ground
    near the point (the client would reject those). `h_range`/`v_range` are the
    search box; callers that mirror a different server call pass its own values
    (CCommander::SetPos uses 500/300, CWorld::InitTriggerObject 1000/300).
    """
    mesh_path = find_mesh(mesh)
    if not mesh_path:
        raise FileNotFoundError("no %s.tok in %s" % (mesh, MESH_DIRS))
    if not os.path.exists(PATHTOOL):
        raise FileNotFoundError(PATHTOOL)

    lines = ["near %d %d %d %d %d" % (x, y, z, h_range, v_range)
             for (x, y, z) in points]
    lines.append("quit")
    proc = subprocess.run([PATHTOOL, mesh_path],
                          input="\n".join(lines).encode(),
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    out = [ln for ln in proc.stdout.decode(errors="replace").splitlines()
           if ln.startswith("near ")]
    if len(out) != len(points):
        raise RuntimeError("pathtool returned %d of %d results"
                           % (len(out), len(points)))

    resolved = []
    for ln in out:
        # near x y z ok px py cell height
        f = ln.split()
        resolved.append((int(f[5]), int(f[6]), int(f[7])) if f[4] == "1" else None)
    return resolved


def build_zone_npcs(world_name, cache_dir=None, mesh=None):
    """Full list for a world: dicts ready to hand to the spawn packet builder."""
    types = load_npc_types(cache_dir)
    rows = load_npc_gen(world_name, cache_dir)

    keep, points = [], []
    for row in rows:
        entry = types.get(row["class_type"])
        if entry is None:
            continue
        row["npc_type"] = entry[0]
        # Size drives the NPC's "domain" radius (geShapeTable: S/M/L/XL), which
        # CFieldChar::GetDomainOuterPos uses to pick where a chase stops.
        row["size"] = entry[1]
        # `Range`: the radius of this NPC's ENTER/LEAVE trigger area, in world
        # units (0 = the row declares none). It is NOT a click reach - the click
        # gate is DISTANCE_TO_CHAR folded with both shape radii, the same for
        # every NPC (source2/geServer/ZServerLib/PlayerChar_Request.cpp:959-961,
        # FieldObject.cpp:120-128). The SERVER reads it into CTriggerObject::m_range
        # for the SCR_ENTER_/SCR_LEAVE_ visitor, and sends it in the NPC's
        # appearance packet (source2/geShared/Source/NPCTable.cpp:111 ->
        # ZServerLib/TriggerObject.cpp:314,389,415).
        row["range"] = entry[2]
        keep.append(row)
        points.append((row["x3d"], row["y3d"], row["z3d"]))

    cells = resolve_cells(points, mesh or world_name)
    out = []
    for row, pos in zip(keep, cells):
        if pos is None:
            row["unresolved"] = True
            continue
        row["x"], row["y"], row["cell"] = pos
        out.append(row)
    return out


def zone_npcs(zone_id, cache_dir=None):
    """All town NPCs for a zone id; [] when the map has no placement table."""
    entry = load_map_table(cache_dir).get(zone_id)
    if not entry:
        return []
    return build_zone_npcs(entry[0], cache_dir, entry[1])


_town_cache = {}


def load_shop_items(cache_dir=None):
    """datatable_Shop.ies -> {GroupName: [itemID, ...]}.

    The server's ShowNPCTradeDlg looks a shop up by GROUP in exactly this table
    (geShopTable, loaded from datatable_Shop) and sends the item ids; the client
    resolves each id's name and price itself, so the ids are all we need.
    """
    key = cache_dir or ""
    if key not in _shop_cache:
        rows = {}
        try:
            root = _decode("datatable_Shop", cache_dir)
        except FileNotFoundError:
            _shop_cache[key] = rows
            return rows
        for cls in root.iter("Class"):
            group = cls.get("GroupName")
            item = cls.get("ItemID")
            if group and item:
                rows.setdefault(group, []).append(int(item))
        _shop_cache[key] = rows
    return _shop_cache[key]


_shop_cache = {}


# The custom-shop table ships as READY XML in the server data folder (it is not in
# the ies tree at all): `server_xml\datatable_custom_shop.xml`. Columns are exactly
# what CShopTable::AddCustomProperty reads: GroupName, SellClassID, PriceClassID,
# Count, PriceClassID2, Count2.
CUSTOM_SHOP_XML = os.environ.get(
    "GE_CUSTOM_SHOP_XML", os.path.join(GAME_ROOT, "ge", "server_xml", "datatable_custom_shop.xml"))
_custom_shop_cache = {}


def load_custom_shop(group=None, path=None):
    """datatable_custom_shop.xml -> {GroupName: [(sell, price1, count1, price2, count2)]}

    A custom shop (script call `ShowCustomNPCTradeDlg(self, pc, "<GROUP>")`) is the
    one the SERVER feeds: the packet carries the items and their prices, unlike the
    normal shop packet which only names a group the client resolves itself.
    """
    key = path or CUSTOM_SHOP_XML
    if key not in _custom_shop_cache:
        rows = {}
        try:
            with open(key, "rb") as f:
                text = f.read().decode("cp949", "replace")
            if text.lstrip().startswith("<?xml"):
                text = text.split("?>", 1)[-1]
            root = ET.fromstring(text)
            for cls in root.iter("Class"):
                grp = cls.get("GroupName")
                if not grp:
                    continue
                try:
                    rows.setdefault(grp, []).append((
                        int(cls.get("SellClassID") or 0),
                        int(cls.get("PriceClassID") or 0),
                        int(cls.get("Count") or 0),
                        int(cls.get("PriceClassID2") or 0),
                        int(cls.get("Count2") or 0)))
                except ValueError:
                    continue
        except OSError:
            rows = {}
        _custom_shop_cache[key] = rows
    if group is None:
        return _custom_shop_cache[key]
    return _custom_shop_cache[key].get(group, [])


def load_town_points(world_name, cache_dir=None):
    """datatable_town.ies: a map's four quick-travel spots.

    Returns {1..4: {"name": locale id, "x", "y", "z"}}, or {} when the map has
    no row (only cities do). The server needs the coordinates
    (CCommander::OnReqTownMove); `name` is the caption on the client's button.
    """
    key = cache_dir or ""
    if key not in _town_cache:
        rows = {}
        try:
            root = _decode("datatable_town", cache_dir)
        except FileNotFoundError:
            _town_cache[key] = rows
            return rows
        for cls in root.iter("Class"):
            name = cls.get("ClassName")
            if not name:
                continue
            spots = {}
            for i in range(1, 5):
                # "P <x> <y> <z>" - the first token is the vector type.
                raw = (cls.get("TownPoint%d" % i) or "").split()
                if len(raw) < 4:
                    continue
                try:
                    spots[i] = {"name": cls.get("PointName%d" % i),
                                "x": int(raw[1]), "y": int(raw[2]),
                                "z": int(raw[3])}
                except ValueError:
                    continue
            rows[name] = spots
        _town_cache[key] = rows
    return _town_cache[key].get(world_name, {})


def town_move(zone_id, index, cache_dir=None):
    """WORLDPOS{x, y, cell} for town button `index` (1..4), or None.

    `index` is the client's button number. The point goes through the path
    engine with a 500/300 search box to match CCommander::SetPos (which calls
    Get3DPointToPosLocaly with 500, 300), unlike the 1000/300 NPC spawn.
    """
    entry = load_map_table(cache_dir).get(zone_id)
    if not entry:
        return None
    world, mesh = entry
    spot = load_town_points(world, cache_dir).get(index)
    if not spot:
        return None
    return resolve_cells([(spot["x"], spot["y"], spot["z"])], mesh,
                         h_range=500, v_range=300)[0]


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1].isdigit():
        zone = int(sys.argv[1])
        npcs = zone_npcs(zone)
        print("zone %d (%s): %d npcs"
              % (zone, load_map_table().get(zone, ("?",))[0], len(npcs)))
    else:
        name = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_MESH
        npcs = build_zone_npcs(name)
        print("%s: %d npcs" % (name, len(npcs)))
    for n in npcs[:12]:
        print("  gen=%-4d type=%-6d dir=%-3d pos=(%s,%s,%s) %s"
              % (n["gen_id"], n["npc_type"], n["direction"],
                 n["x"], n["y"], n["cell"], n["class_type"]))
