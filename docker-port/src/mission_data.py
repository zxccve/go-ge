"""mission_data - the mission tables the scripts and the client both read.

`datatable_missionlist` carries the idspace **`MissionList`**: **269 selectable
missions**, one `<Class>` each. A mission here is a LIST ENTRY (an Info Center
row), not a map.

The fields that matter for the list and the lobby:

| field | meaning |
|---|---|
| `ClassID` | the mission id the CLIENT resolves (this is what a 3335 record carries) |
| `ClassName` | the stable name scripts compare against (`mission_rafflesia`) |
| `Name` | the locale token the client draws |
| `Type` | the flag soup behind the tabs: `ShowAll Day Week Rep Sqd Solo Party Mcc1..3 Rank1..3 Hard EXTRA Exp` |
| `ReqAR` / `ReqDR` / `ReqPromotion` | the entry requirement |
| `Item_Enter` / `Item_Count` | the entry item (`Live_Missionenterpoint -1`) |
| `Map` | the map the mission runs on (`fld_coim`) |
| `Npc` | the lobby NPC script (`MISSION_SINGLERAID`) |
| `Rulletlist` | the award roulette group (`GLimit_PRIVATE_MISSION_RANK1`) |

Idspace note: the table is `datatable_missionlist.ies` AND its idspace is
`MissionList`, so both spellings resolve (`npc_data.tables_for_idspace`), but
`Mission_Rank` is a DIFFERENT table (`datatable_missionenter.ies`).
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import npc_data

_cache = {}


def load_mission_list(cache_dir=None):
    """`{class_id: {property: text}}` for the MissionList idspace, cached.

    Keys are the table's own `ClassID`, which is also what the client resolves
    a 3335 record through - so an id we invent is silently dropped by the
    client and the panel just shows fewer rows.
    """
    key = cache_dir or ""
    if key in _cache:
        return _cache[key]
    rows = {}
    for stem in npc_data.tables_for_idspace("MissionList", cache_dir):
        root = npc_data._decode(stem, cache_dir)
        for cls in root.iter("Class"):
            try:
                rows[int(cls.get("ClassID") or 0)] = dict(cls.attrib)
            except (TypeError, ValueError):
                continue
    _cache[key] = rows
    return rows


def mission_entries(cache_dir=None):
    """`[(class_id, row), ...]` in ClassID order - the order the list is sent."""
    rows = load_mission_list(cache_dir)
    return [(cid, rows[cid]) for cid in sorted(rows)]


_mission_rows = {}


def mission_row(mission_id, cache_dir=None):
    """One row of `datatable_mission` (idspace `Mission`), or `{}`.

    This is the table the LOBBY reads, not the list: `MissionRoom` names the room
    window (`mwr1a30`), `MaxPlayer`/`MaxAlliance`/`MaxSlotCount` size it. Mission
    id 20001 is `mission_rafflesia` with all three at 1.
    """
    key = cache_dir or ""
    rows = _mission_rows.get(key)
    if rows is None:
        rows = {}
        for stem in npc_data.tables_for_idspace("Mission", cache_dir):
            root = npc_data._decode(stem, cache_dir)
            for cls in root.iter("Class"):
                try:
                    rows[int(cls.get("ClassID") or 0)] = dict(cls.attrib)
                except (TypeError, ValueError):
                    continue
        _mission_rows[key] = rows
    try:
        return rows.get(int(float(mission_id)))
    except (TypeError, ValueError):
        return None


def mission_max_player(mission_id, default=8):
    """The room's player cap, from the mission row (`MaxPlayer`)."""
    row = mission_row(mission_id) or {}
    try:
        return max(1, int(float(str(row.get("MaxPlayer") or default))))
    except (TypeError, ValueError):
        return default


def type_flags(row):
    """`Type` as a set of words (`{'ShowAll', 'Rep', 'Solo', ...}`).

    The tabs are driven by these flags, so a filter is a set test, never a
    substring test (`Rank1` is a prefix of `Rank10`).
    """
    return set(str(row.get("Type") or "").split())


def meets_requirement(row, ar=0, dr=0, promotion=0):
    """Whether a player at (ar, dr, promotion) may enter this mission.

    `ReqAR`/`ReqDR`/`ReqPromotion` are the gate the lobby shows; a value of 0
    means "no requirement".
    """
    def _num(name):
        try:
            return int(float(str(row.get(name) or 0)))
        except (TypeError, ValueError):
            return 0
    return (_num("ReqAR") <= ar and _num("ReqDR") <= dr
            and _num("ReqPromotion") <= promotion)
