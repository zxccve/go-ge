r"""Write a map's WALK GRAPH as JSON, so the server needs no path engine at run time.

WHY: the chase asks `navmesh\pathtool.exe` two questions - "where should I stand" and
"how far is the walk" - and that helper is a 32-bit Windows binary around
`pathengine.dll`, so the Linux VPS cannot run it. The answers do not change: they are
geometry, fixed in the map. So they are computed HERE, once per map, and shipped.

The file carries three things:

  spots  per NPC: the stand-off point and its navmesh CELL. Derived the way the live
         server does it - a point 140 units out from the NPC, clamped back towards the
         NPC until it is clear; when the clamp is shortened, an obstacle is pressed
         against the NPC, the client refuses those poses and the answer is the NPC's OWN
         position (the character then overlaps it, as the live server's does).
         Each spot ALSO carries `sectors`: one clear point per compass direction (16 of
         them, 22.5 degrees apart, dir 0 = +x). At BUILD time there is no player to aim
         towards, so every clear side is kept and the server picks the one facing the
         player - a single stored point sent the character round to the far side of the
         NPC (R58, history\zone-entry.md). `x`/`y`/`cell` stay the OLD single point and
         are used whenever `sectors` is empty.
  nodes  walkable points on a grid over the town, each with a cell.
  edges  pairs of nodes joined by a leg the client can walk STRAIGHT - a route query
         whose length matches the straight distance. Detours are not stored: the client
         does its own pathing, so the graph only has to answer "can I get from here to
         there, and roughly how far is it".

Cost, measured 2026-10-28 on map 10: the spots are 119 NPCs x 48 clamp queries (16
directions x 3 rings) in 0.2 s, the grid is ~500 `near` queries, and edges are a few
thousand batch route queries - seconds in total, because the helper answers a whole
batch in one run. Keeping every direction costs nothing extra: the queries were already
being sent, and the old code threw away all but the first answer.

Usage
-----
    python emit_zone_nav.py 10              # one map -> data\zone_nav\map10.json
    python emit_zone_nav.py --all
    python emit_zone_nav.py 10 --print      # show what was built
"""

import argparse
import json
import os
import sys
import time

from ge_paths import DATA, ensure

# The handle convention lives with the spawn emitter, so both files agree: a click
# comes back as handle = BASE + index in the map's NPC list.
from emit_zone_npcs import NPC_HANDLE_BASE

OUT_DIR = os.path.join(DATA, "zone_nav")

# How far from the NPC the character should stop. The original's own number:
# `m_npcClickDistance` (100) - 10 + the NPC's radius (50 for an "S" body).
STANDOFF = 140.0

# The grid's step. Small enough that a route between neighbours is a short straight
# leg, large enough that a town is hundreds of nodes and not tens of thousands.
SPACING = 300

# How far around the town's NPCs to sample, and how near an NPC a node must be to be
# kept at all. A player can start outside the ring of NPCs, so the grid reaches past
# them - but not to the empty half of the map.
MARGIN = 700

# A leg is stored whenever a route exists. The 10% rule above was a mistake: dropping
# legs that need a detour cut the map into islands, so the graph said "unreachable" where
# the engine walked happily. The detour's LENGTH is stored instead, and A* adds real
# lengths, so a long way round is a long edge and not a missing one.

# The actor's half-size (geShared\Source\ShapeTable.cpp: S = 50).
RADIUS = 50


def _atomic_write(path, doc):
    """Write to a sibling .tmp and rename. A reader never sees a half-written file."""
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, separators=(",", ":"))
    # On Windows the rename can lose a race with a reader holding the target open.
    for _ in range(20):
        try:
            os.replace(tmp, path)
            return
        except PermissionError:
            time.sleep(0.05)
    os.replace(tmp, path)


def _run(path, lines, mesh):
    """Send many commands to ONE pathtool run and return its output lines.

    One process per query is what made this tool take 15 minutes: the helper loads the
    whole mesh at startup, so a run costs ~0.11 s whether it answers one query or ten
    thousand. Everything is batched into three runs (clamps, grid, edges).
    """
    import subprocess

    import npc_data

    mesh_path = npc_data.find_mesh(mesh)
    if not mesh_path:
        raise FileNotFoundError("no %s.tok in %s" % (mesh, npc_data.MESH_DIRS))
    if not os.path.exists(npc_data.PATHTOOL):
        raise FileNotFoundError(npc_data.PATHTOOL)
    proc = subprocess.run([npc_data.PATHTOOL, mesh_path],
                          input="\n".join(lines + ["quit"]).encode(),
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    return proc.stdout.decode(errors="replace").splitlines()


def _parse_paths(lines):
    """[(points, length)] for `path` answers, in order. None when there is no route."""
    out = []
    i = 0
    while i < len(lines):
        line = lines[i]
        if line.startswith("path ok "):
            n = int(line.split()[2])
            pts = []
            for j in range(n):
                i += 1
                if i >= len(lines):
                    break
                f = lines[i].split()
                pts.append((int(f[2]), int(f[3]), int(f[4])))
            length = int(line.split()[4].split("=")[1])
            out.append((pts, length) if pts else None)
        elif line.startswith("path none") or line.startswith("path err"):
            out.append(None)
        i += 1
    return out


def _spots(rows, mesh):
    """Per NPC: every clear side of it, plus the old single stand-off as the fallback.

    Sixteen directions at three distances, because a single ring is not enough: a stall
    or a garden bed beside an NPC makes the 140-unit point on that side collide, while a
    shorter stand-off on the same side is fine. Only when EVERY ring collides - an
    obstacle pressed right against the NPC - does the answer become the NPC's own
    position, which is the live server's rule and why the character overlaps those.

    Two answers are kept per NPC:

      x/y/cell  the FIRST clear point in ring order (140 first, then 110, then 80, and
                within a ring the lowest direction). This is the old build-time answer:
                the server uses it when it cannot aim, so it must not move.
      sectors   the nearest-140 clear point for EACH of the 16 directions that has one.
                The server picks the sector facing the player, so the character stops on
                the side it walked in from instead of always on the stored side.
    """
    import math

    rings = (STANDOFF, 110.0, 80.0)
    queries, order = [], []
    for index, row in enumerate(rows):
        base = (int(row["x"]), int(row["y"]), int(row["cell"]))
        for distance in rings:
            for step in range(16):
                rad = math.radians(step * 22.5)
                ideal = (base[0] + int(round(math.cos(rad) * distance)),
                         base[1] + int(round(math.sin(rad) * distance)))
                queries.append("clamp %d %d %d %d %d %d"
                               % (base[0], base[1], base[2], ideal[0], ideal[1], RADIUS))
                order.append((index, step, distance))
    answers = [ln for ln in _run("clamp", queries, mesh) if ln.startswith("clamp ")]

    best = {}
    sectors = {}
    for (index, step, distance), line in zip(order, answers):
        f = line.split()
        if f[1] != "ok" or f[5] == "hit":
            continue  # obstructed (or the start itself is invalid): try the next
        got = (int(f[2]), int(f[3]), int(f[4]))
        # Rings arrive widest first and directions in order, so the FIRST clear answer
        # is the best one for the old single point: keep it and ignore the rest.
        best.setdefault(index, got)
        # Per direction, the first clear answer is the NEAREST 140-point on that side,
        # for the same reason - setdefault keeps it and the 110/80 rings are ignored.
        sectors.setdefault(index, {}).setdefault(step, got)

    out = []
    for index, row in enumerate(rows):
        base = (int(row["x"]), int(row["y"]), int(row["cell"]))
        spot = best.get(index)
        sides = [{"dir": step * 22.5, "x": p[0], "y": p[1], "cell": p[2]}
                 for step, p in sorted(sectors.get(index, {}).items())]
        out.append({"handle": NPC_HANDLE_BASE + index,
                    # The NPC's OWN position travels with the spot: the server looks the
                    # spot up by where the NPC stands, and needs no handle plumbing.
                    "anchor_x": base[0], "anchor_y": base[1],
                    "x": (spot or base)[0], "y": (spot or base)[1],
                    "cell": (spot or base)[2], "overlap": spot is None,
                    "sectors": sides})
    return out


def _grid(rows, mesh, spacing, margin):
    """Walkable nodes near the town's NPCs, each with a real cell.

    Only points within `margin` of an NPC are kept. The bounding box of a whole map is
    mostly empty ground - sampling all of it produced 9,067 nodes and 77k edges for a
    town whose NPCs stand in a few clusters - and every one of those points costs a
    route query and file space.
    """
    import math
    import npc_data

    xs = [int(r["x"]) for r in rows]
    ys = [int(r["y"]) for r in rows]
    if not xs:
        return []
    points = [(int(r["x"]), int(r["y"])) for r in rows]
    lo_x, hi_x = min(xs) - margin, max(xs) + margin
    lo_y, hi_y = min(ys) - margin, max(ys) + margin

    probes = []
    y = lo_y
    while y <= hi_y:
        x = lo_x
        while x <= hi_x:
            for px, py in points:
                if abs(px - x) <= margin and abs(py - y) <= margin:
                    probes.append((x, y, 0))
                    break
            x += spacing
        y += spacing

    resolved = npc_data.resolve_cells(probes, mesh)
    return [p for p in resolved if p is not None]


def _edges(nodes, spacing, mesh):
    """Pairs joined by a route, with that route's length as the weight. One batch run."""
    import math

    reach = spacing * 1.6
    pairs = []
    for i, a in enumerate(nodes):
        for j in range(i + 1, len(nodes)):
            b = nodes[j]
            d = math.hypot(a[0] - b[0], a[1] - b[1])
            if d <= reach:
                pairs.append((i, j, d))
    if not pairs:
        return []

    queries = ["path %d %d %d %d %d %d %d"
               % (nodes[i][0], nodes[i][1], nodes[i][2],
                  nodes[j][0], nodes[j][1], nodes[j][2], RADIUS)
               for i, j, _ in pairs]
    answers = _parse_paths(_run("path", queries, mesh))

    edges = []
    for (i, j, direct), route in zip(pairs, answers):
        if not route:
            continue
        # The route's own length, so a detour stays a walkable edge instead of becoming
        # a hole in the graph. `direct` is only used to spot a nonsense answer.
        length = route[1]
        if length < direct * 0.5:
            continue
        edges.append([i, j, int(length)])
    return edges


def emit_map(map_id, print_rows=False):
    import npc_data

    rows = npc_data.zone_npcs(map_id)
    entry = npc_data.load_map_table().get(int(map_id))
    mesh = entry[1] if entry else None
    if not mesh:
        print("map %-4d has no walk-mesh in the map table - nothing to emit" % map_id)
        return None

    start = time.time()
    spots = _spots(rows, mesh) if rows else []
    print("  spots   %3d NPC(s)  %5.1f s" % (len(spots), time.time() - start))
    grid_start = time.time()
    nodes = _grid(rows, mesh, SPACING, MARGIN) if rows else []
    print("  grid    %3d node(s) %5.1f s" % (len(nodes), time.time() - grid_start))
    edge_start = time.time()
    edges = _edges(nodes, SPACING, mesh) if nodes else []
    print("  edges   %5d link(s) %5.1f s" % (len(edges), time.time() - edge_start))

    doc = {
        # 2 adds `sectors` to every spot: one clear stand-off per compass direction. A
        # reader that does not understand it still has x/y/cell, which never changed.
        "schema": 2,
        "map_id": int(map_id),
        "mesh": mesh,
        "radius": RADIUS,
        "spacing": SPACING,
        "standoff": int(STANDOFF),
        "generated_by": "src/emit_zone_nav.py",
        "npc_count": len(spots),
        "node_count": len(nodes),
        "overlaps": sum(1 for s in spots if s["overlap"]),
        "sector_counts": {
            "npcs": len(spots),
            "npcs_with_sectors": sum(1 for s in spots if s["sectors"]),
            "sectors": sum(len(s["sectors"]) for s in spots),
            "min": min([len(s["sectors"]) for s in spots] or [0]),
            "max": max([len(s["sectors"]) for s in spots] or [0]),
        },
        "spots": spots,
        # Flat triples / pairs keep the file small; a town is ~500 nodes. An EDGE is
        # (a, b, length): the length is the engine's own route, so a detour is a long
        # edge and never a missing one.
        "nodes": [c for n in nodes for c in n],
        "edges": [c for e in edges for c in e],
    }

    ensure(OUT_DIR)
    path = os.path.join(OUT_DIR, "map%d.json" % int(map_id))
    _atomic_write(path, doc)
    sc = doc["sector_counts"]
    print("map %-4d %3d NPC(s), %3d node(s), %5d edge(s), %d overlap, sectors %d/%d "
          "NPC(s) %d-%d each  %.1f s  -> %s"
          % (map_id, len(spots), len(nodes), len(edges), doc["overlaps"],
             sc["npcs_with_sectors"], sc["npcs"], sc["min"], sc["max"],
             time.time() - start, os.path.relpath(path, DATA)))
    if print_rows:
        for s in spots[:5]:
            print("   spot", s)
        print("   first nodes", [tuple(nodes[i]) for i in range(min(3, len(nodes)))])
    return doc


def all_maps():
    import npc_data
    return sorted(npc_data.load_map_table().keys())


def main():
    ap = argparse.ArgumentParser(description="Emit a map's walk graph as JSON.")
    ap.add_argument("map_id", nargs="?", type=int)
    ap.add_argument("--all", action="store_true", help="every map in the map table")
    ap.add_argument("--print", dest="print_rows", action="store_true")
    args = ap.parse_args()

    if args.all:
        maps = all_maps()
        print("emitting %d map(s)" % len(maps))
        for map_id in maps:
            try:
                emit_map(map_id, False)
            except Exception as exc:  # one bad map must not stop the sweep
                print("map %-4d failed: %s" % (map_id, exc))
        return 0
    if args.map_id is None:
        ap.print_help()
        return 1
    emit_map(args.map_id, args.print_rows)
    return 0


if __name__ == "__main__":
    sys.exit(main())
