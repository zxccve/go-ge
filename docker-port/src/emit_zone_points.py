r"""Write EVERY map's spawn, town buttons, restart point and gates as one JSON file.

WHY A DATA FILE AND NOT RUNTIME CODE: a position the client accepts is not (x, y).
It is (x, y, CELL), and the cell is a navmesh TRIANGLE INDEX that only
`navmesh\pathtool.exe` can answer - a 32-bit Windows helper around `pathengine.dll`,
so the Linux VPS the server targets cannot run it. The lookup is a BUILD step here
and both servers (Python and Go) replay the result.

Until now each server hardcoded one map's numbers, which is why map 10 "worked" and
every other map had no spawn, no town buttons and no gates. This file replaces those
constants: for any map id it says where a team stands when it enters, where the four
town quick-move buttons lead, which warp the restart ("return to town") uses, and
which gate rows leave this map and where they arrive.

Version 2 adds the FORMATION: a team is three characters, and without offsets all
three stand inside each other on one spot. Every spawn and every town point now
carries a three-slot `formation` whose slot 0 is the point itself and whose slots 1
and 2 are the two followers. The Go zone server has no path engine, so it cannot pick
those spots at runtime - they are resolved here, in the same sweep, for the same
reason the cells are.

Version 3 adds the ARRIVAL side of a gate. A cross-map move is
`ZC_MOVE_ZONE` (3593) -> `CZ_MOVE_ZONE_OK` (3686) -> `ZC_MOVE_ZONE_OK` (3594, 101
bytes) -> the client reconnects and sends `CZ_START_GAME` (3590). The 3594 body
carries the DESTINATION: `+2 zoneInstID`, `+6 zoneIP[16]`, `+22 zonePort`,
`+26 map[41]` (the map CLASS name), `+67..+86` five camera floats, `+87
startPos[3]` (x, y, CELL), `+99 startDir`, `+100 channel`. Version 2 gate rows only
had the SOURCE point, so `startPos` had nowhere to come from. Every gate now also
carries `target_x`, `target_y`, `target_cell`, `target_z`, `target_dir`,
`target_dir_byte`, `range`, and the five camera floats, read from the same warp row
the original server reads (`source2\geShared\Source\WarpTable.cpp:85` follows).

`target_dir` is DEGREES out of the table, but `startDir` at +99 is ONE BYTE, and the
original does not truncate it - it converts with `GetByteAngle()`
(`(BYTE)(deg/360*255+0.5)`, `gePacketBase.h:11`, used at `Commander.cpp:1213`). So the
already-converted byte is shipped as `target_dir_byte`; masking the degrees with
`& 0xFF` would move 278 degrees by 256 and face the character the wrong way.

`target_cell` is resolved on the TARGET map's own mesh, never the source map's: a
cell is a triangle index INTO a mesh, so a source-mesh index lands on unrelated
ground. `target_z` is the row's `TargetH`, which is a HEIGHT hint and NOT a cell -
the original writes it into the third int of `TRANSFER_ZONE_INFO.point`
(`source2\geServer\ZServerLib\WarpPoint.cpp:97`), which is the cell slot, and then
lets the client correct the position. It is kept for reference only.

The file is DERIVED data: regenerate it, never hand-edit it.

Cells are NEVER guessed. When the path engine finds no ground near a point the cell
is written as null and the x/y are kept, because the alternative - writing a HEIGHT
where the client expects a triangle index - crashes the client inside PathEngine.

Usage
-----
    python emit_zone_points.py             # -> data\zone_points.json
    python emit_zone_points.py --print     # also show a few known maps
    python emit_zone_points.py --maps 10,15 --print   # just those

Cost, measured 2026-10-27: 2,381 map rows, 415 distinct walk-meshes. The path engine
loads a whole mesh at startup and answers a batch in one run, so the queries are
grouped BY MESH (one `pathtool` run per mesh, ~360 points on the biggest), not one
run per map - that is what keeps the sweep at minutes instead of an hour. The two
follower slots ride in the same per-mesh batch, so the formation costs no extra runs.
"""

import argparse
import json
import os
import sys
import time

from ge_paths import DATA, ensure

OUT = os.path.join(DATA, "zone_points.json")

# The schema the Go reader is written against. Bump it when a field changes meaning.
# 2 = every spawn and town point also carries a three-slot `formation`. Nothing was
# renamed or removed, so a reader written for 1 still works.
# 3 = every gate ALSO carries its arrival side: `target_x`, `target_y`,
#     `target_cell` (on the TARGET map's mesh), `target_z` (a height hint, not a
#     cell), `target_dir` (degrees) + `target_dir_byte` (the value for the byte
#     field), `range`, and `lat`/`long`/`zoom_min`/`zoom_max`/`init_zoom`. Again
#     nothing was renamed or removed, so a reader written for 2 still reads every
#     old key; it just does not know the new ones.
# 4 = a top-level `waypoints`: the TOWN-TELEPORT grid, one row per datatable_waypoint
#     row in the client's own table order (see WAYPOINTS below). Again nothing was
#     renamed or removed, so a reader written for any earlier version still reads
#     every old key.
VERSION = 4

# A team is three characters and they enter one point, so the two followers get a
# slot each. These are the ORIGINAL SERVER'S OWN NUMBERS, not an invented spread:
# CCommander::GetTeamPos (source2\geServer\ZServerLib\Commander.cpp:1605; v28
# FUN_00687bb0 @ 0x00687bb0) takes the leader's position, steps the angle 0x78 ->
# 0x168 (120 degrees and 240 degrees) on a radius of 50 around a centre 50 units
# WEST of the leader, then casts to int with CVTTSS2SI (truncation).
#   slot 1 = (-75, +43)   slot 2 = (-75, -44)   float: -75.000003, +-43.30127
# The V always points west in MAP AXES: GetTeamPos takes no angle and no direction
# byte, so a character's facing never rotates it. The third int (the CELL) is the
# LEADER's in the original; here each slot is resolved to its own cell instead,
# because our clients are placed from data and a wrong cell drops a character
# through the map - a slot with no ground falls back to the leader's point.
FORMATION_OFFSETS = ((0, 0), (-75, 43), (-75, -44))
FOLLOWERS = 2                     # slots per formation that are not the leader

# The search box for a point the client walks to: CCommander::SetPos calls
# Get3DPointToPosLocaly with 500/300. The NPC spawn uses 1000/300 instead, but every
# point in this file is somewhere a CHARACTER stands, so 500/300 is the right one.
H_RANGE = 500
V_RANGE = 300

# How many town quick-move buttons the client draws.
TOWN_BUTTONS = (1, 2, 3, 4)


def _atomic_write(path, doc):
    """Write to a sibling .tmp and rename. A reader never sees a half-written file."""
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"))
    for _ in range(20):
        try:
            os.replace(tmp, path)
            return
        except PermissionError:   # a reader holding the target open (Windows)
            time.sleep(0.05)
    os.replace(tmp, path)


def _text(cls, key):
    """A row's field as text, or None when the table's own word for empty is there.

    The tables spell "no value" as "None" (a string), not as an absent attribute, so
    a plain `is None` check would treat 807 warp rows as gates that leave from a map
    called None.
    """
    value = (cls.get(key) or "").strip()
    if not value or value.lower() == "none":
        return None
    return value


def _int(cls, key, default=0):
    """An integer field, rounded from the table's float spelling (it writes -1088.0)."""
    try:
        return int(float((cls.get(key) or "").strip()))
    except (TypeError, ValueError):
        return default


def _float(cls, key):
    """A float field, or None when the row has no such column or no value.

    The camera floats (Latitude, Longitude, ZoomMin, ZoomMax, InitZoom) are the
    client's own numbers and go out as floats, so they are read as floats and not
    rounded through `_int`. None means "this table has no source for it", which is
    reported instead of being filled with a guess.
    """
    try:
        return float((cls.get(key) or "").strip())
    except (TypeError, ValueError):
        return None


def warp_rows():
    """`{warp id: row}` from datatable_warp - the gate and restart rows."""
    import npc_data

    rows = {}
    for cls in npc_data._decode("datatable_warp").iter("Class"):
        rows[_int(cls, "ClassID", -1)] = cls
    return rows


def waypoint_rows():
    """`[(row id, map name, restart warp id, tab)]` from datatable_waypoint.

    The table is tiny and has exactly four columns: `ClassID`, `Location`,
    `RestartWarpID`, `Tab`. `Location` is a ZONE name (it matches
    datatable_map.ClassName), not a display string - the client looks the town's
    name up in its own map table, so a name sent from here would be ignored.

    `Tab` is which grid the row belongs to in the client's own window layout
    (`ge\\ui\\uixml\\waypointlist.xml`): 0 = the 15-slot TOWN tab, 1 = the 9-slot
    FIELD tab. The town rows are the ones with a non-zero RestartWarpID; the field
    rows have 0.
    """
    import npc_data

    out = []
    for cls in npc_data._decode("datatable_waypoint").iter("Class"):
        name = _text(cls, "Location")
        if name:
            out.append((_int(cls, "ClassID", -1), name,
                        _int(cls, "RestartWarpID"), _int(cls, "Tab")))
    return out





_map_rows = None


def _map_row(map_id):
    """The raw datatable_map row for one id (WayPointX/Y/Z live only there)."""
    global _map_rows
    if _map_rows is None:
        import npc_data

        _map_rows = {}
        for cls in npc_data._decode("datatable_map").iter("Class"):
            try:
                _map_rows[int(float(cls.get("ClassID")))] = cls
            except (TypeError, ValueError):
                continue
    return _map_rows.get(map_id, {})


_mesh_seen = {}


def _has_mesh(mesh):
    """Is there a .tok for this mesh? Cached - hundreds of maps share one mesh."""
    if mesh not in _mesh_seen:
        import npc_data

        _mesh_seen[mesh] = bool(mesh) and bool(npc_data.find_mesh(mesh))
    return _mesh_seen[mesh]


def _usable(x, y):
    """False for the tables' (0, 0) placeholder, which is the MESH ORIGIN, not a place.

    758 of the 2,381 map rows carry WayPointX = WayPointY = 0. Resolving that point
    does not fail politely: the engine happily snaps to whatever triangle is near the
    origin, so the map would get a spawn in a wall instead of no spawn at all.
    """
    return not (int(x) == 0 and int(y) == 0)


def _plan_formation(owner, x, y, z, mesh, tasks, owners):
    """Give one point (or one spawn) its three slots and queue the two followers.

    Slot 0 is the leader and IS the point, so it is never asked for twice: its cell
    is copied from the point's own answer after `resolve()`. Slots 1 and 2 are queued
    on the same mesh, so they cost no extra path-engine run - they land in the batch
    that was already going to that mesh.
    """
    slots = [{"x": x, "y": y, "cell": None}]
    for dx, dy in FORMATION_OFFSETS[1:]:
        slot = {"x": x + dx, "y": y + dy, "cell": None}
        slots.append(slot)
        tasks.append((mesh, slot["x"], slot["y"], z, slot, "cell"))
    owner["formation"] = slots
    owners.append(owner)


def finish_formations(owners, stats):
    """Fill slot 0 from the point, and keep each follower's OWN cell.

    The CELL is resolved PER SLOT, not copied from the leader. Evidence, 2026-10-27,
    from the client itself after a town warp: with the leader's cell forced onto
    every slot, the client kept sending CZ_MOVE_PC for the third character to
    (6769, 10815, cell 186) while we had put her on (6790, 10827, cell 183) - the
    leader's triangle. She walked to her own triangle on every warp and kept
    turning, which is exactly the "third character spins" report. Coimbra point 2
    really does span two triangles (183 and 186).

    A follower slot with NO ground must never stay null: the Go server cannot look
    one up, and a null cell sent to the client is a rejected position. It copies the
    leader's x/y/cell instead. The count of times that fired is reported because it
    is the one place a real offset is silently lost.
    """
    for owner in owners:
        slots = owner["formation"]
        slots[0]["cell"] = owner["cell"]
        leader = slots[0]
        if leader["cell"] is None:
            # No ground at the point itself: nothing to fall back ONTO. Counted
            # separately, reported, and left null rather than invented.
            stats["leader_null"] += 1
        for slot in slots[1:]:
            if slot["cell"] is not None:
                continue
            stats["fallback"] += 1
            slot["x"], slot["y"] = leader["x"], leader["y"]
            slot["cell"] = leader["cell"]
            if leader["cell"] is None:
                stats["fallback_null"] += 1


def collect():
    """Plan every map: what to write, and which points still need a CELL.

    Returns (maps, tasks, owners, notes, skipped). `tasks` are (mesh, x, y, z, dict,
    key) tuples - the dict gets the resolved cell under `key`. They are handed to
    `resolve()` in one batch per mesh, so the path engine is started once per mesh and
    not once per point. `owners` are the spawns and points that carry a formation;
    their slots are finished after the cells come back.
    """
    import npc_data

    table = npc_data.load_map_table()
    name_to_id = {name: map_id for map_id, (name, _mesh) in table.items()}
    warps = warp_rows()
    restarts = {}
    for _wp_id, name, warp_id, _tab in waypoint_rows():
        if warp_id > 0 and warp_id in warps:
            restarts[name] = warp_id

    # Gates are grouped by the map they leave FROM. A row whose CreateZone is empty
    # is ARRIVAL-ONLY (it names a destination and no position), so it cannot say
    # where a player stands and is not a gate.
    gates_by_zone, arrival_only = {}, 0
    for warp_id, cls in warps.items():
        target = _text(cls, "TargetZone")
        if not target:
            continue
        source = _text(cls, "CreateZone")
        if not source:
            arrival_only += 1
            continue
        gates_by_zone.setdefault(source, []).append((warp_id, cls))

    maps, tasks = {}, []
    owners = []
    no_mesh, no_spawn, bad_targets = [], [], 0
    no_target_mesh = set()    # target maps whose arrival cell cannot be resolved
    for map_id in sorted(table):
        name, mesh = table[map_id]
        entry = {
            "name": name,
            "mesh": mesh or "",
            # 1 when this map has town quick-move points (a city). A flag, not the
            # town table's own row id: the two disagree for 15 of the 16 cities.
            "town": 0,
            "spawn": None,
            "points": [],
            "restart": None,
            "gates": [],
        }
        if not _has_mesh(mesh):
            no_mesh.append(map_id)

        for index in TOWN_BUTTONS:
            spot = (npc_data.load_town_points(name) or {}).get(index)
            if not spot:
                continue
            point = {"n": index, "x": spot["x"], "y": spot["y"], "cell": None,
                     "name": spot.get("name") or ""}
            entry["points"].append(point)
            tasks.append((mesh, point["x"], point["y"], spot["z"], point, "cell"))
            _plan_formation(point, point["x"], point["y"], spot["z"], mesh, tasks,
                            owners)
        if entry["points"]:
            entry["town"] = 1

        # Spawn priority: the city's first quick-move point (where the client itself
        # drops a team), else the map's own WayPoint, else the restart warp's target.
        if entry["points"]:
            first = entry["points"][0]
            entry["spawn"] = {"x": first["x"], "y": first["y"], "cell": None,
                              "source": "townpoint:%d" % first["n"]}
            # The cell comes from the same query as the point: nothing extra to ask.
            # The formation is copied off that same point in `build()` for the same
            # reason - a city's spawn IS its first quick-move point.
            entry["_from"] = first
        else:
            cls = _map_row(map_id)
            x, y = _int(cls, "WayPointX"), _int(cls, "WayPointY")
            if _usable(x, y):
                z = _int(cls, "WayPointZ")
                entry["spawn"] = {"x": x, "y": y, "cell": None, "source": "map:waypoint"}
                tasks.append((mesh, x, y, z, entry["spawn"], "cell"))
                _plan_formation(entry["spawn"], x, y, z, mesh, tasks, owners)
            elif name in restarts:
                warp_id = restarts[name]
                cls = warps[warp_id]
                x, y = _int(cls, "TargetX"), _int(cls, "TargetY")
                z = _int(cls, "TargetH")
                entry["spawn"] = {"x": x, "y": y, "cell": None,
                                  "source": "warp:restart:%d" % warp_id}
                tasks.append((mesh, x, y, z, entry["spawn"], "cell"))
                _plan_formation(entry["spawn"], x, y, z, mesh, tasks, owners)
        if entry["spawn"] is None:
            no_spawn.append(map_id)

        if name in restarts:
            warp_id = restarts[name]
            cls = warps[warp_id]
            # THE CAMERA TRAVELS WITH THE WARP ROW (2026-11-09). The restart warp is the
            # "To the Save Point" destination, and a zone move carries the destination's
            # camera in MOVE_ZONE_OK (zoomMin/zoomMax/initZoom/lat/long). Without them the
            # client is handed zeros and its zoom collapses - the owner saw exactly that
            # after the first save-point move. Same five columns the gate rows already keep.
            restart = {"warp_id": warp_id, "x": _int(cls, "TargetX"),
                       "y": _int(cls, "TargetY"), "cell": None,
                       "lat": _float(cls, "Latitude"), "long": _float(cls, "Longitude"),
                       "zoom_min": _float(cls, "ZoomMin"), "zoom_max": _float(cls, "ZoomMax"),
                       "init_zoom": _float(cls, "InitZoom")}
            entry["restart"] = restart
            tasks.append((mesh, restart["x"], restart["y"], _int(cls, "TargetH"),
                          restart, "cell"))

        for warp_id, cls in gates_by_zone.get(name, []):
            target = _text(cls, "TargetZone")
            to_id = name_to_id.get(target)
            gate = {"name": _text(cls, "ClassName") or "", "to": target,
                    "to_id": to_id, "x": _int(cls, "CreateX"),
                    "y": _int(cls, "CreateY"), "cell": None,
                    # --- the ARRIVAL side: what ZC_MOVE_ZONE_OK (3594, 101 bytes)
                    # puts at +87 (startPos x, y, CELL), +99 (startDir) and +67..+86
                    # (the five camera floats). Raw row values; the CELL is the only
                    # one that cannot be copied, because it is a triangle index into
                    # the TARGET map's mesh and is resolved below.
                    "target_x": _int(cls, "TargetX"),
                    "target_y": _int(cls, "TargetY"),
                    "target_cell": None,
                    # TargetH is a HEIGHT, kept for reference only and never as a
                    # cell. The original server writes it into the third int of
                    # TRANSFER_ZONE_INFO.point (WarpPoint.cpp:97) - the cell slot -
                    # and the client then corrects the position itself. A height in
                    # the cell slot drops a character through the map.
                    "target_z": _int(cls, "TargetH")}
            # TargetDir -> the client's startDir (+99). The ROW value is DEGREES
            # (0..360, and a few rows go past 359), and the packet field is ONE BYTE,
            # so the two are not the same number: the original converts with
            # GetByteAngle() - `(BYTE)(deg/360*255 + 0.5)` (gePacketBase.h:11), used at
            # Commander.cpp:1213. A plain `& 0xFF` would turn 278 into 22, a silent
            # 256-degree error. Both are written: `target_dir` is the raw row value so
            # it can be compared with the table, `target_dir_byte` is the byte to send.
            if "TargetDir" in cls.keys():
                degrees = _int(cls, "TargetDir")
                gate["target_dir"] = degrees
                gate["target_dir_byte"] = int(degrees / 360.0 * 255.0 + 0.5) & 0xFF
            # The TRIGGER radius of the source point. The original calls
            # IsNearObject(leader, range) on it (WarpPoint.cpp:71) and hands the same
            # number to the range visitor (WarpPoint.cpp:114), so it is the radius of
            # the circle a player must stand in - the source side is a point PLUS
            # this, not a bare point.
            for column in ("Range", "Radius", "Size"):
                if column in cls.keys():
                    gate["range"] = _int(cls, column)
                    break
            # The five camera floats the client is TOLD to use on arrival; the
            # original copies them straight from the row (WarpPoint.cpp:99-103).
            for key, column in (("lat", "Latitude"), ("long", "Longitude"),
                                ("zoom_min", "ZoomMin"), ("zoom_max", "ZoomMax"),
                                ("init_zoom", "InitZoom")):
                value = _float(cls, column)
                if value is not None:
                    gate[key] = value
            entry["gates"].append(gate)
            if to_id is None:
                # Kept, not dropped: the row is real and the destination name is the
                # answer, but the schema's cell is only meaningful for a destination
                # the server can actually load.
                bad_targets += 1
                continue
            tasks.append((mesh, gate["x"], gate["y"], _int(cls, "CreateH"), gate, "cell"))
            # The arrival point is resolved on the TARGET map's OWN mesh. A target of
            # (0, 0) is the tables' mesh-origin placeholder, so it is left null rather
            # than snapped to whatever triangle sits near the origin.
            _tname, tmesh = table.get(to_id, (target, ""))
            if _usable(gate["target_x"], gate["target_y"]) and _has_mesh(tmesh):
                tasks.append((tmesh, gate["target_x"], gate["target_y"],
                              gate["target_z"], gate, "target_cell"))
            else:
                no_target_mesh.add(to_id)

        maps[map_id] = entry

    notes = []
    if no_mesh:
        notes.append("%d map(s) have no .tok navmesh, so every cell on them is null"
                     % len(no_mesh))
    if arrival_only:
        notes.append("%d warp row(s) carry no CreateZone (arrival-only: a destination "
                     "and no position) and are not listed as gates" % arrival_only)
    if bad_targets:
        notes.append("%d gate row(s) name a TargetZone with no map id; they are kept "
                     "with to_id and cell null" % bad_targets)
    return maps, tasks, owners, notes, {"no_mesh": no_mesh, "no_spawn": no_spawn,
                                        "no_target_mesh": no_target_mesh}


def resolve(tasks):
    """Fill every queued cell, ONE path-engine run per mesh; returns failed meshes.

    Grouping is the whole point: the helper loads the mesh at startup and answers a
    batch, so 2,380 map spawns cost ~415 runs, not 2,380.
    """
    import npc_data

    by_mesh, failed = {}, []
    for mesh, x, y, z, target, key in tasks:
        by_mesh.setdefault(mesh, []).append((x, y, z, target, key))

    for mesh, items in sorted(by_mesh.items()):
        if not npc_data.find_mesh(mesh):
            continue                    # already counted: no .tok means no cell
        points = [(x, y, z) for x, y, z, _t, _k in items]
        try:
            cells = npc_data.resolve_cells(points, mesh, h_range=H_RANGE,
                                           v_range=V_RANGE)
        except Exception as exc:        # one bad mesh must not lose the whole sweep
            failed.append("%s (%s)" % (mesh, exc))
            continue
        for (_x, _y, _z, target, key), cell in zip(items, cells):
            target[key] = cell[2] if cell else None
    return failed


def build(print_maps=None):
    """Read the client's tables, resolve the cells, write the file. Returns counts."""
    start = time.time()
    maps, tasks, owners, notes, skipped = collect()

    # A city's spawn cell IS its first town point's cell: one query serves both, so the
    # spawn is copied from the resolved point instead of being asked for twice.
    from_town = [(entry, entry.pop("_from")) for entry in maps.values() if "_from" in entry]

    failed = resolve(tasks)

    stats = {"fallback": 0, "leader_null": 0, "fallback_null": 0}
    finish_formations(owners, stats)

    for entry, first in from_town:
        # Same values as the point, so the formation is a copy of that point's - a new
        # list of new dicts, so later edits to one cannot reach the other.
        entry["spawn"] = {"x": first["x"], "y": first["y"], "cell": first["cell"],
                          "source": entry["spawn"]["source"],
                          "formation": [dict(slot) for slot in first["formation"]]}
    if failed:
        notes.append("%d mesh(es) gave no answer: %s" % (len(failed), ", ".join(failed)))

    no_cell = sum(1 for e in maps.values()
                  if e["spawn"] and e["spawn"]["cell"] is None)
    if no_cell:
        notes.append("%d map(s) have a spawn position the path engine found no ground "
                     "near; their cell is null" % no_cell)

    # Slots actually WRITTEN: a city's spawn repeats its first point's formation, so
    # counting what was planned would undercount the file by those copies.
    formation_slots = sum(
        (len(e["spawn"]["formation"]) if e["spawn"] else 0)
        + sum(len(p["formation"]) for p in e["points"])
        for e in maps.values())
    follower_slots = FOLLOWERS * len(owners)
    if stats["fallback"]:
        notes.append("%d of %d follower slot(s) found no ground next to the leader and "
                     "were put on the leader's own point instead (leader +-%d/%d)"
                     % (stats["fallback"], follower_slots,
                        FORMATION_OFFSETS[1][0], FORMATION_OFFSETS[1][1]))
    if stats["leader_null"]:
        notes.append("%d point(s) have no cell at all; their formation slot 0 is null "
                     "and %d follower slot(s) copied it"
                     % (stats["leader_null"], stats["fallback_null"]))

    # ------------------------------------------------------------------ gates, v3
    no_target_mesh_ids = skipped["no_target_mesh"]
    gates = [g for e in maps.values() for g in e["gates"]]
    with_tcell = sum(1 for g in gates if g["target_cell"] is not None)
    without_tcell = len(gates) - with_tcell
    with_range = sum(1 for g in gates if "range" in g)
    with_camera = sum(1 for g in gates if "init_zoom" in g)
    with_dest_dir = sum(1 for g in gates if "target_dir" in g)
    if gates:
        notes.append(
            "%d of %d gate(s) have a target_cell resolved on the TARGET map's own "
            "mesh; %d are null (no map id, no .tok, or a (0,0) placeholder), so the "
            "Go side must send a null cell, not a copied or invented one"
            % (with_tcell, len(gates), without_tcell))
    if no_target_mesh_ids:
        notes.append("%d target map(s) have no .tok navmesh, so every arrival cell on "
                     "them is null: %s"
                     % (len(no_target_mesh_ids),
                        ", ".join(str(m) for m in sorted(no_target_mesh_ids)[:12])))
    if with_camera < len(gates) or with_range < len(gates):
        notes.append("%d gate(s) carry the five camera floats and %d carry a trigger "
                     "range; the rest have no column for them and those keys are "
                     "ABSENT, which is why the Go side sends zeros"
                     % (with_camera, with_range))
    if with_dest_dir < len(gates):
        notes.append("%d gate(s) have no TargetDir column, so their target_dir key is "
                     "absent" % (len(gates) - with_dest_dir))
    notes.append("a gate's SOURCE side is usable as a trigger area: x, y, cell plus "
                 "`range` (the row's Range, the radius the original checks with "
                 "IsNearObject(leader, range), WarpPoint.cpp:71), so no radius "
                 "constant is needed; `target_z` is the row's TargetH, a HEIGHT hint "
                 "and NOT a cell; `target_dir` is DEGREES from the row and the "
                 "startDir field is one byte, so send `target_dir_byte` "
                 "(GetByteAngle) and never `target_dir & 0xFF`")

    # The TOWN-TELEPORT grid, in the client's own datatable_waypoint row order. The
    # client fills grid slot i from record i, so this order IS the arrangement the
    # player sees. A row whose Location names no map is left out and counted: the
    # window can then be explained ("14 of 15") instead of quietly short.
    waypoints, wp_unresolved = [], []
    names = {e["name"]: map_id for map_id, e in maps.items()}
    for wp_id, wp_name, wp_warp, wp_tab in waypoint_rows():
        map_id = names.get(wp_name)
        if map_id is None:
            wp_unresolved.append(wp_name)
            continue
        waypoints.append({
            "id": wp_id,
            "location": wp_name,
            "map_id": map_id,
            "restart_warp_id": wp_warp,
            "tab": wp_tab,
        })
    wp_towns = [w for w in waypoints if w["tab"] == 0 and w["restart_warp_id"] > 0]
    notes.append("%d waypoint row(s) in datatable_waypoint, %d of them town rows (Tab 0 "
                 "with a restart warp id) and %d field rows (Tab 1); they are the order "
                 "the client's own waypointlist grid draws, and the Go side sends the "
                 "town rows as ZC_WAYPOINT_LIST (3742) on a click on the Waypoint NPC"
                 % (len(waypoints), len(wp_towns),
                    sum(1 for w in waypoints if w["tab"] == 1)))
    if wp_unresolved:
        notes.append("%d waypoint Location value(s) name no map and are NOT emitted: %s"
                     % (len(wp_unresolved), ", ".join(sorted(wp_unresolved)[:12])))

    doc = {
        "generated_by": "src/emit_zone_points.py",
        "version": VERSION,
        "built": time.strftime("%Y-%m-%d"),
        "counts": {
            "maps": len(maps),
            "with_spawn": sum(1 for e in maps.values() if e["spawn"]),
            "with_town": sum(1 for e in maps.values() if e["town"]),
            "gates": len(gates),
            "gates_with_target_cell": with_tcell,
            "gates_without_target_cell": without_tcell,
            "target_maps_without_mesh": len(no_target_mesh_ids),
            "gates_with_range": with_range,
            "gates_with_camera": with_camera,
            "formation_slots": formation_slots,
            "formation_fallbacks": stats["fallback"],
        },
        "by_name": {e["name"]: map_id for map_id, e in maps.items()},
        "maps": {str(map_id): entry for map_id, entry in maps.items()},
        "waypoints": waypoints,
    }
    if notes:
        doc["note"] = "; ".join(notes)

    ensure(DATA)
    _atomic_write(OUT, doc)
    size = os.path.getsize(OUT)
    print("wrote %s  %.0f KB  %.1f s" % (os.path.relpath(OUT, DATA), size / 1024.0,
                                         time.time() - start))
    print("  %d map(s), %d with a spawn, %d with town points, %d gate row(s)"
          % (doc["counts"]["maps"], doc["counts"]["with_spawn"],
             doc["counts"]["with_town"], doc["counts"]["gates"]))
    print("  %d formation slot(s) written, %d follower slot(s) fell back to the leader"
          % (formation_slots, stats["fallback"]))
    print("  %d of %d gate(s) have a target cell on the TARGET mesh (%d null), "
          "%d with range, %d with camera floats"
          % (with_tcell, len(gates), without_tcell, with_range, with_camera))
    if doc.get("note"):
        print("  note: %s" % doc["note"])
    print("  %d map(s) have no spawn at all" % len(skipped["no_spawn"]))

    if print_maps:
        for map_id in print_maps:
            entry = doc["maps"].get(str(map_id))
            print("  map %s: %s" % (map_id, json.dumps(entry) if entry else "-"))
    return doc


def main():
    ap = argparse.ArgumentParser(description="Emit every map's spawn/town/restart/gate data.")
    ap.add_argument("--maps", help="comma-separated map ids to show after writing")
    ap.add_argument("--print", dest="show", action="store_true",
                    help="show maps 10 and 15 after writing")
    ap.add_argument("--out", help="write somewhere else (testing); default data\\zone_points.json")
    args = ap.parse_args()

    global OUT
    if args.out:
        OUT = os.path.abspath(args.out)
    show = None
    if args.maps:
        show = [int(v) for v in args.maps.split(",") if v.strip()]
    elif args.show:
        show = [10, 15]
    build(show)
    return 0


if __name__ == "__main__":
    sys.exit(main())
