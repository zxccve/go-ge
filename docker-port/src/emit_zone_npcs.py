r"""Write the town-NPC spawn data for one map (or all maps) as JSON.

WHY A DATA FILE AND NOT RUNTIME CODE: an NPC's spawn needs a WORLDPOS cell, which
is a navmesh TRIANGLE INDEX resolved by `navmesh\pathtool.exe` - a 32-bit Windows
helper with `pathengine.dll`, so it cannot run on the Linux VPS the server targets.
The lookup is therefore a BUILD step here, and the Go zone replays the result.

The file is DERIVED data: regenerate it, never hand-edit it. Adding or removing an
NPC is a re-run of this tool, not a code change.

Usage
-----
    python emit_zone_npcs.py 10          # one map  -> data\zone_npcs\map10.json
    python emit_zone_npcs.py --all       # every map the map table knows
    python emit_zone_npcs.py 10 --print  # also show the first rows

Cost, measured 2026-10-25: map 10 (Reboldoeux) has 119 NPCs and takes **0.3 s**.
The earlier estimate of ~40 ms PER NPC was wrong - the mesh is loaded once per map,
so a map is sub-second and every map together is minutes, not an hour.
"""

import argparse
import json
import os
import sys
import time

from ge_paths import DATA, ensure, GAME_ROOT

OUT_DIR = os.path.join(DATA, "zone_npcs")

# NPC handles are the client's own convention: a high base plus the index in the
# spawn list. The zone must keep the same numbering, because a click comes back as
# that handle (CZ_CLICK_TRIGGER) and it is how an NPC is identified.
NPC_HANDLE_BASE = 0x40000000

# The client revision our game data was read from. Stamped into every file so a
# mismatch after a data update is visible instead of silent.
def client_revision():
    """The client's own revision string, or "unknown"."""
    for path in (os.path.join(GAME_ROOT, "GEAndromida", "updater.revision.txt"),
                 os.path.join(GAME_ROOT, "GEAndromida", "updater.revision")):
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as fh:
                return fh.read().strip().splitlines()[0][:40]
        except OSError:
            continue
    return "unknown"


def mesh_name(map_id):
    """The map's walk-mesh name (`reb_basic`), or "" when the table has none.

    Coined here, at BUILD time, because the Go zone must name the mesh it asks the
    path engine about - and the map table is Python-only data.
    """
    try:
        import npc_data
        entry = npc_data.load_map_table().get(int(map_id))
        return entry[1] if entry else ""
    except Exception:
        return ""


def emit_map(map_id, print_rows=False):
    """Build and write the data file for one map; returns (count, seconds)."""
    import npc_data

    start = time.time()
    rows = npc_data.zone_npcs(map_id)
    elapsed = time.time() - start

    npcs = []
    for i, row in enumerate(rows):
        npcs.append({
            "handle": NPC_HANDLE_BASE + i,
            # WORLDPOS: x, y, and the navmesh CELL (a triangle index, never a height).
            "x": int(row["x"]),
            "y": int(row["y"]),
            "cell": int(row["cell"]),
            "direction": int(row["direction"]) & 0xFF,
            # ClassID in the client's own "NPC" table.
            "npc_type": int(row["npc_type"]),
            # The client's NPCGen row id, used for its caption fallback.
            "gen_id": int(row["gen_id"]),
            # A locale TOKEN (e.g. "<$>10090776</>"), NOT text. The Go side sends an
            # empty caption and lets the client resolve the name from gen_id, which
            # is correct because these rows came from the CLIENT's own npcgen table.
            "name_token": row.get("name") or "",
            "class_type": row.get("class_type") or "",
            # The NPC table's `Range`: the radius of this NPC's ENTER/LEAVE
            # trigger area (0 = the row declares none). NOT the click reach -
            # see npcs.go's Range comment for the citation.
            "range": int(row.get("range") or 0),
            # The NPCGen `Script` column: the zone answers a click with
            # `SCR_CLICK_<script>`. Empty (or "None", the table's own word for
            # "no script") means every click on this NPC does nothing, which is a
            # real state and worth writing down rather than guessing at runtime.
            "script": "" if (row.get("script") or "None") == "None" else row["script"],
        })

    doc = {
        "map_id": int(map_id),
        # The walk-mesh (navmesh) this map uses, e.g. "reb_basic". The Go zone
        # needs the name to ask the path engine for a route to a clicked NPC;
        # without it the only walk it can offer is a straight line.
        "mesh": mesh_name(map_id),
        "client_revision": client_revision(),
        "generated_by": "src/emit_zone_npcs.py",
        "npc_count": len(npcs),
        "npcs": npcs,
    }

    ensure(OUT_DIR)
    path = os.path.join(OUT_DIR, "map%d.json" % int(map_id))
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=1)
    print("map %-4d %3d NPC(s)  %.2f s  -> %s"
          % (map_id, len(npcs), elapsed, os.path.relpath(path, DATA)))
    if print_rows:
        for row in npcs[:5]:
            print("   ", row)
    return len(npcs), elapsed


def all_maps():
    """Every map id the client's map table knows."""
    import npc_data
    return sorted(npc_data.load_map_table().keys())


def main():
    ap = argparse.ArgumentParser(description="Emit town-NPC spawn data as JSON.")
    ap.add_argument("map_id", nargs="?", type=int, help="one map id (e.g. 10)")
    ap.add_argument("--all", action="store_true", help="every map in the map table")
    ap.add_argument("--print", dest="print_rows", action="store_true",
                    help="show the first rows of each map")
    args = ap.parse_args()

    if args.all:
        maps = all_maps()
        print("emitting %d map(s)" % len(maps))
        total, seconds, empty, missing = 0, 0.0, [], 0
        for map_id in maps:
            # A map with no npcgen_<map>.ies is the NORMAL case, not an error: the client only
            # ships placement tables for maps that have NPCs at all (missions and some arenas
            # have none). Before 2026-11-09 this raised FileNotFoundError on the FIRST such map
            # and killed --all after a handful of files.
            try:
                count, elapsed = emit_map(map_id, False)
            except FileNotFoundError:
                missing += 1
                continue
            total += count
            seconds += elapsed
            if count == 0:
                empty.append(map_id)
        print("done: %d NPC(s) over %d map(s) in %.1f s of lookup time (%d map(s) have no placement table)"
              % (total, len(maps), seconds, missing))
        if empty:
            print("%d map(s) have no NPC placement table: %s"
                  % (len(empty), ", ".join(str(m) for m in empty[:20])))
        return 0

    if args.map_id is None:
        ap.print_help()
        return 1
    emit_map(args.map_id, args.print_rows)
    return 0


if __name__ == "__main__":
    sys.exit(main())
