"""check_data - does the SERVER's game data match the CLIENT's?

The server reads tables from four different trees (the server release's
`ge\\ies` + `ge\\xml`, the old client's `Converter`/dictionary, the Andromida
client's unpacked ies, and the script export). Anything the server SENDS by id
or by number must exist in the CLIENT's copy, or the client shows a blank,
a wrong name, or silently refuses.

This tool answers one question per table: which ClassIDs exist on one side and
not the other? It also checks that every item class in the live bag is known to
the client, because that is the failure a player notices first.

Usage:
    python check_data.py              # all checks
    python check_data.py items npc    # only named groups
Env:
    GE_SERVER_XML   server tables as XML (default <path>)
    GE_CLIENT_IES   client tables as .ies (default _andro_ies_stage\\ies10000)
    GE_CHECK_CACHE  converted client XML cache dir
"""
import os

import sys
import tempfile
import time
import xml.etree.ElementTree as ET
from ge_paths import GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER_XML = os.environ.get("GE_SERVER_XML", os.path.join(GAME_ROOT, "ge", "xml"))
CLIENT_IES = os.environ.get("GE_CLIENT_IES",
                            os.path.join(GAME_ROOT, "_andro_ies_stage", "ies10000"))
CACHE = os.environ.get("GE_CHECK_CACHE",
                       os.path.join(tempfile.gettempdir(), "ge_check"))
CONVERTER = os.environ.get(
    "GE_STANCE_CONVERTER", os.path.join(GAME_ROOT, "GEAndromida", "Converter"))

# group -> [(label, client ies name)]; the server side is the same name .xml
GROUPS = {
    "items": [("item_%s" % n, "datatable_item_%s.ies" % n) for n in (
        "weapon", "armor", "consume", "etc", "quest", "costume", "ring",
        "neck", "earring", "belt", "boots", "glove", "head", "back", "book",
        "breast", "shoulder", "scroll", "instant", "lumin", "artifact",
        "assist", "recipe", "randombase", "transfer", "extention_socket",
        "achieve", "achieve_color", "additional_prop")],
    "npc": [("npc", "datatable_npc.ies")],
    "skill": [("skill", "datatable_skill.ies")],
    "stance": [("stance", "datatable_stance.ies"),
               ("stancecondition", "datatable_stancecondition.ies")],
    "job": [("job", "datatable_job.ies")],
    "shop": [("shop", "datatable_shop.ies")],
    "map": [("map", "datatable_map.ies")],
    "xp": [("xp", "datatable_xp.ies")],
}

_ides = {}


def parse_xml(path):
    """Parse a game XML table.

    The server's tables are CP949 with an `encoding="euc-kr"` declaration, which
    ElementTree refuses ("multi-byte encodings are not supported"). Decode it
    ourselves and drop the declaration before parsing.
    """
    data = open(path, "rb").read()
    text = None
    for enc in ("utf-8", "cp949"):
        try:
            text = data.decode(enc)
            break
        except UnicodeDecodeError:
            continue
    if text is None:
        return None
    if text.startswith("\ufeff"):
        text = text[1:]
    if text.lstrip().startswith("<?xml"):
        text = text.split("?>", 1)[1]
    try:
        return ET.fromstring(text)
    except ET.ParseError as exc:
        print("  cannot parse %s: %s" % (path, exc))
        return None


def client_rows(ies_name):
    """Parse the client's .ies (converted once, cached). Returns [Class]."""
    if ies_name in _ides:
        return _ides[ies_name]
    os.makedirs(CACHE, exist_ok=True)
    src = os.path.join(CLIENT_IES, ies_name)
    dst = os.path.join(CACHE, ies_name + ".xml")
    if not os.path.exists(src):
        _ides[ies_name] = []
        return []
    if not os.path.exists(dst) or os.path.getmtime(dst) < os.path.getmtime(src):
        sys.path.insert(0, CONVERTER)
        import ies2
        try:
            # Write beside the target and rename: the rename is atomic, so a
            # reader (the zone server, decoding the same table at the same
            # time) never sees a half-written file.
            tmp = "%s.%d.tmp" % (dst, os.getpid())
            try:
                ies2.ies_to_xml(src, tmp, None, None, "UTF-8", False)
                for attempt in range(20):
                    try:
                        os.replace(tmp, dst)
                        break
                    except PermissionError:
                        # Windows: the rename fails if another process holds
                        # the destination open for reading. Retry.
                        if attempt == 19:
                            raise
                        time.sleep(0.05 * (attempt + 1))
            finally:
                if os.path.exists(tmp):
                    try:
                        os.remove(tmp)
                    except OSError:
                        pass
        except Exception as exc:
            print("  cannot convert %s: %s" % (ies_name, exc))
            _ides[ies_name] = []
            return []
    try:
        root = parse_xml(dst)
    except Exception as exc:
        print("  cannot parse %s: %s" % (dst, exc))
        _ides[ies_name] = []
        return []
    if root is None:
        _ides[ies_name] = []
        return []
    _ides[ies_name] = list(root.iter("Class"))
    return _ides[ies_name]


def server_rows(label):
    path = os.path.join(SERVER_XML, "datatable_%s.xml" % label)
    if not os.path.exists(path):
        return []
    root = parse_xml(path)
    return list(root.iter("Class")) if root is not None else []


def ids(rows):
    out = {}
    for c in rows:
        cid = c.get("ClassID")
        if cid and cid.lstrip("-").isdigit():
            out[int(cid)] = c.get("ClassName") or c.get("Name") or ""
    return out


def compare(label, ies_name):
    srv = ids(server_rows(label))
    cli = ids(client_rows(ies_name))
    if not srv and not cli:
        return None
    only_srv = sorted(set(srv) - set(cli))
    only_cli = sorted(set(cli) - set(srv))
    print("  %-18s server=%-6d client=%-6d  server-only=%-5d client-only=%d"
          % (label, len(srv), len(cli), len(only_srv), len(only_cli)))
    for cid in only_srv[:6]:
        print("      server-only id %-8d %s" % (cid, srv[cid]))
    return {"server": srv, "client": cli, "only_server": only_srv}


def check_items_in_bag():
    """Every item class the client is asked to draw must exist client-side."""
    try:
        import ge_db
    except Exception as exc:
        print("  skipped (no DB): %s" % exc)
        return
    try:
        rows = ge_db._run(
            "SELECT DISTINCT i.CLASS_ID FROM STORAGE s "
            "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
            "WHERE s.TYPE=0 AND s.TRASH=0 AND i.CLASS_ID<>0")
    except Exception as exc:
        print("  DB query failed: %s" % exc)
        return
    classes = sorted({int(r[0]) for r in rows if str(r[0]).isdigit()})
    known = {}
    files = sorted(f for f in os.listdir(CLIENT_IES)
                   if f.startswith("datatable_item_") and f.endswith(".ies"))
    for name in files:
        known.update(ids(client_rows(name)))
    # 30000/30001 are the vis/feso pseudo-items (g_visItem), drawn as money,
    # never as bag rows - they have no item-table row by design.
    missing = [c for c in classes if c not in known and c not in (30000, 30001)]
    print("  %-18s bag classes=%-5d unknown to client=%d"
          % ("bag items", len(classes), len(missing)))
    for cid in missing[:10]:
        print("      bag class %d is NOT in the client item tables" % cid)


def client_item_ids():
    """Every ClassID in the client's item tables (the id space the bag uses)."""
    out = set()
    for name in sorted(os.listdir(CLIENT_IES)):
        if name.startswith("datatable_item_") and name.endswith(".ies"):
            out.update(ids(client_rows(name)))
    return out


def check_shop_groups():
    """The trade packet names a GROUP; the CLIENT resolves the stock itself."""
    groups = {}
    for r in client_rows("datatable_shop.ies"):
        g = r.get("GroupName")
        if g:
            groups[g] = groups.get(g, 0) + 1
    print("  client shop groups=%d" % len(groups))
    for g in ("BLACKSMITH_SHOP", "REB_ITEMSHOP", "ETC_LEONARDO",
              "PERIOD_COSTUMESHOP7"):
        n = groups.get(g, 0)
        print("    %-22s client rows=%-3d %s"
              % (g, n, "OK" if n else "MISSING - the window would be empty"))


def check_npc_types_all():
    """Every NPC the server can EVER spawn, for every zone with an npcgen table.

    A spot check on one town proves nothing about the others, so walk the map
    table -> world name -> npcgen_<world> tables (the same chain the server uses
    at runtime) and require each mapped id to exist client-side under the SAME
    class name.
    """
    try:
        import npc_data
    except Exception as exc:
        print("  skipped (no npc_data): %s" % exc)
        return
    cache = os.path.join(CACHE, "npc")
    os.makedirs(cache, exist_ok=True)
    old_types = npc_data.load_npc_types(cache)
    cli = {r.get("ClassName"): int(r.get("ClassID"))
           for r in client_rows("datatable_npc.ies")
           if (r.get("ClassID") or "").isdigit()}
    cli_ids = set(cli.values())
    maps = npc_data.load_map_table(cache)
    worlds = sorted({w for (w, _mesh) in maps.values() if w})
    checked = placed = no_type = unknown = id_diff = 0
    examples = []
    for world in worlds:
        src = os.path.join(npc_data.IES_DIR, "npcgen_%s.ies" % world)
        if not os.path.exists(src):
            continue
        try:
            rows = npc_data.load_npc_gen(world, cache)
        except Exception as exc:
            print("      %s: %s" % (world, exc))
            continue
        checked += 1
        for row in rows:
            placed += 1
            entry = old_types.get(row["class_type"])
            if entry is None:
                no_type += 1
                continue
            nid = entry[0]
            if nid not in cli_ids:
                unknown += 1
                if len(examples) < 10:
                    examples.append((world, row["class_type"], nid, "no such id"))
            elif cli.get(row["class_type"]) != nid:
                id_diff += 1
                if len(examples) < 10:
                    examples.append((world, row["class_type"], nid,
                                     "client says %s" % cli.get(row["class_type"])))
    print("  worlds with npcgen=%-4d placed npcs=%-5d class types with no old row=%-4d"
          % (checked, placed, no_type))
    print("  unknown client-side=%-4d same class but different id=%d"
          % (unknown, id_diff))
    for world, cls, nid, why in examples:
        print("      %-18s %-28s id=%-8s %s" % (world, cls, nid, why))


def check_npc_types(zones=(10,)):
    """npc_type must be an id the client knows, and map to the same class name."""
    try:
        import npc_data
    except Exception as exc:
        print("  skipped (no npc_data): %s" % exc)
        return
    cli = {r.get("ClassName"): int(r.get("ClassID"))
           for r in client_rows("datatable_npc.ies")
           if (r.get("ClassID") or "").isdigit()}
    cli_ids = set(cli.values())
    for zone in zones:
        try:
            npcs = npc_data.zone_npcs(int(zone))
        except Exception as exc:
            print("  zone %s: %s" % (zone, exc))
            continue
        missing = [n for n in npcs if n["npc_type"] not in cli_ids]
        same = sum(1 for n in npcs if cli.get(n["class_type"]) == n["npc_type"])
        print("  zone %-5s npcs=%-4d unknown type=%-3d same id for same class=%d"
              % (zone, len(npcs), len(missing), same))
        for n in missing[:5]:
            print("      %s type=%s name=%s" %
                  (n["class_type"], n["npc_type"], n["name"]))


def check_team_ids():
    """Stances/skills the server sends must exist client-side, with same speeds."""
    try:
        import ge_db
        import stance_data
    except Exception as exc:
        print("  skipped (no DB/stance_data): %s" % exc)
        return
    srv = {int(r.get("ClassID")): r for r in server_rows("stance")
           if (r.get("ClassID") or "").isdigit()}
    cli = {int(r.get("ClassID")): r for r in client_rows("datatable_stance.ies")
           if (r.get("ClassID") or "").isdigit()}
    cli_skills = set(ids(client_rows("datatable_skill.ies")))
    bad_ids, bad_speed = [], []
    try:
        chars = ge_db.get_characters(24)
    except Exception as exc:
        print("  skipped (DB): %s" % exc)
        return
    for ch in chars:
        job = ch.get("job")
        for s in stance_data.job_stances(job):
            a, b = srv.get(int(s)), cli.get(int(s))
            if b is None:
                bad_ids.append((job, "stance", s))
                continue
            if a is not None:
                for f in ("WSPD", "RSPD", "StnMSPD"):
                    if (a.get(f) or "") != (b.get(f) or ""):
                        bad_speed.append((s, f, a.get(f), b.get(f)))
        for sk in stance_data.job_skills(job):
            if int(sk) not in cli_skills:
                bad_ids.append((job, "skill", sk))
    print("  characters=%d unknown ids=%d speed differences=%d"
          % (len(chars), len(bad_ids), len(bad_speed)))
    for row in bad_ids[:8]:
        print("      job %s sends %s %s, absent client-side" % row)


def main():
    want = [a.lower() for a in sys.argv[1:]]
    print("server tables: %s" % SERVER_XML)
    print("client tables: %s" % CLIENT_IES)
    results = {}
    for group, tables in GROUPS.items():
        if want and group not in want:
            continue
        print("[%s]" % group)
        for label, ies_name in tables:
            r = compare(label, ies_name)
            if r:
                results[label] = r
        if group == "items":
            check_items_in_bag()
    if not want or "shops" in want:
        print("[shops]")
        check_shop_groups()
    if not want or "npcs" in want:
        print("[npcs]")
        check_npc_types()
        print("[npcs:all zones]")
        check_npc_types_all()
    if not want or "team" in want:
        print("[team]")
        check_team_ids()
    print("\ndone. 'server-only' ids are safe only if the client never has to\n"
          "resolve them; 'client-only' ids mean the client knows rows we do not.")


if __name__ == "__main__":
    main()
