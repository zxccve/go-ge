"""stance_data - the stance table's movement speeds.

The original server sets a character's move speed from its STANCE whenever a move
is dispatched (`m_moveSpeed = GetMoveSPD()`, PlayerChar_FSM.cpp:224), and a
click-move is a RUN, so the number that matters is the stance's **RSPD** (WSPD is
the walk). The client does the same for moves its own AI drives
(`OnHandleAiMove` -> `GetPropertyNumber("Stance", m_stance, "RSPD")`,
GeBaseObject.cpp:1245).

The table is already readable as XML - no converter needed:
    ge\\xml\\datatable_stance.xml      idspace "Stance"   (427 rows, EUC-KR)
The binary twin is `ge\\ies\\datatable_stance.ies`; same rows.

Which stance is the current one? Two paths matter:
- We now send the character's REAL stance list in START_INFO (see
  `build_stance_records`), so the client runs `GetStanceByCondition`
  (GeSession.cpp:487) and picks the first stance of the first job condition whose
  LHand/RHand classTypes match the equipped weapons. `active_stance()` below
  computes the same thing server-side, so the move speed matches the animation.
- With an EMPTY stance list (the old behaviour) the client falls back to
  `GM_STANCE = 40000` ("Fretensis"), not 10010; that is the barrack PC object's
  constructor default only (GEBarrackObject.cpp:72).

The list itself is the UNION of the job's stance conditions, in condition order:
`datatable_job` row `EqpWeaponSet` = condition ids, each row of
`datatable_stancecondition` names `LHand`/`RHand` classType strings plus
`Stance1..6` ids.

Env:
  GE_STANCE_XML        override the stance XML path
  GE_JOB_XML           override datatable_job.xml
  GE_STANCECOND_XML    override datatable_stancecondition.xml
  GE_WEAPON_IES        override datatable_item_weapon.ies
  GE_STANCE_CONVERTER  folder holding ies2.py
"""
import os

import re
import struct
import sys
import tempfile
import xml.etree.ElementTree as ET

import xml_tables
from ge_paths import GAME_ROOT

STANCE_XML = os.environ.get("GE_STANCE_XML", os.path.join(GAME_ROOT, "ge", "xml", "datatable_stance.xml"))
# Fallback copies, tried in order, if the server data folder is elsewhere.
_ALT_XML = (
    os.path.join(GAME_ROOT, "alternate_ge_server", "ge_server", "ge", "xml", "datatable_stance.xml"),
)
JOB_XML = os.environ.get("GE_JOB_XML", os.path.join(GAME_ROOT, "ge", "xml", "datatable_job.xml"))
COND_XML = os.environ.get("GE_STANCECOND_XML",
                          os.path.join(GAME_ROOT, "ge", "xml", "datatable_stancecondition.xml"))
# The item table is per CATEGORY; hands (weapons, shields) live in the weapon one.
# Prefer the CLIENT's copy: it is the table the client itself resolves, and it is
# newer than the server's.
_WEAPON_IES = (
    os.environ.get("GE_WEAPON_IES", ""),
    os.path.join(GAME_ROOT, "_andro_ies_stage", "ies10000", "datatable_item_weapon.ies"),
    os.path.join(GAME_ROOT, "ge", "ies", "datatable_item_weapon.ies"),
)
CONVERTER = os.environ.get(
    "GE_STANCE_CONVERTER", os.path.join(GAME_ROOT, "GEAndromida", "Converter"))
# CPC's own default (the stance the client uses when the server sends none).
DEFAULT_STANCE = 10010
# The client's fallback when its stance list ends up EMPTY (GeBaseObject.h).
GM_STANCE = 40000
# CFieldChar's constructor value, used only if the table cannot be read.
FALLBACK_RSPD = 415.0

_ROWS = None      # {class_id: {"name", "wspd", "rspd", "mspd", "hold"}}
_JOBS = None      # {job_id: [condition ids]}
_CONDS = None     # {cond_id: {"l", "r", "stances": [ids]}}
_WEAPONS = None   # {class_id: (classType, is_double)}


def _xml_root(path):
    """Parse one of the game's XML tables.

    The tree is MIXED encoding: the tables rebuilt from the .ies are UTF-8,
    the hand-made ones are still the original EUC-KR. `xml_tables` reads the
    declaration and falls back, so one reader serves them all. (The old code
    hardcoded cp949, which mangled - or failed on - the UTF-8 tables.)
    """
    return xml_tables.read_table(path)


def _flatten(node):
    """Yield every <Class .../> under an <idspace>, whatever the nesting."""
    for child in node:
        if child.tag == "Class":
            yield child
        else:
            for c in _flatten(child):
                yield c


def _load():
    global _ROWS
    if _ROWS is not None:
        return _ROWS
    rows = {}
    path = STANCE_XML
    if not os.path.exists(path):
        for alt in _ALT_XML:
            if os.path.exists(alt):
                path = alt
                break
    try:
        for c in _flatten(_xml_root(path)):
            cid = c.get("ClassID")
            if cid is None:
                continue
            skills = []
            for n in range(1, 6):
                v = c.get("SkillID%d" % n)
                if v and v.isdigit() and int(v):
                    skills.append(int(v))
            rows[int(cid)] = {
                "name": c.get("ClassName", ""),
                "wspd": float(c.get("WSPD") or 0),
                "rspd": float(c.get("RSPD") or 0),
                "mspd": float(c.get("StnMSPD") or 1),
                "hold": (c.get("HoldMove", "NO").upper() == "YES"),
                "xp": int(c.get("XPType") or 0),
                "skills": skills,
            }
    except (OSError, ET.ParseError):
        rows = {}
    _ROWS = rows
    return _ROWS


def _load_jobs():
    global _JOBS
    if _JOBS is None:
        jobs = {}
        try:
            for c in _flatten(_xml_root(JOB_XML)):
                cid = c.get("ClassID")
                if cid is None:
                    continue
                ids = []
                for part in (c.get("EqpWeaponSet") or "").split(","):
                    part = part.strip()
                    if part and part.isdigit():
                        ids.append(int(part))
                jobs[int(cid)] = ids
        except (OSError, ET.ParseError):
            jobs = {}
        _JOBS = jobs
    return _JOBS


def _load_conditions():
    global _CONDS
    if _CONDS is None:
        conds = {}
        try:
            for c in _flatten(_xml_root(COND_XML)):
                cid = c.get("ClassID")
                if cid is None:
                    continue
                stances = []
                for n in range(1, 7):
                    v = c.get("Stance%d" % n)
                    if v and v.isdigit() and int(v):
                        stances.append(int(v))
                conds[int(cid)] = {
                    "l": c.get("LHand") or "None",
                    "r": c.get("RHand") or "None",
                    "stances": stances,
                }
        except (OSError, ET.ParseError):
            conds = {}
        _CONDS = conds
    return _CONDS


def _load_weapons():
    """{item class id: (classType, is_double_handed)} from the weapon table."""
    global _WEAPONS
    if _WEAPONS is not None:
        return _WEAPONS
    weapons = {}
    path = next((p for p in _WEAPON_IES if p and os.path.exists(p)), None)
    if path:
        try:
            sys.path.insert(0, CONVERTER)
            import ies2  # noqa: E402
            dst = os.path.join(tempfile.gettempdir(), "ge_stance_item_weapon.xml")
            ies2.ies_to_xml(path, dst, None, None, "UTF-8", False)
            root = ET.parse(dst).getroot()
            for c in _flatten(root):
                cid = c.get("ClassID")
                if cid is None:
                    continue
                dbl = (str(c.get("DBLHand", "NO")).upper() == "YES"
                       or str(c.get("DblAttach", "0")).upper()
                       not in ("0", "", "NO", "FALSE", "NONE"))
                weapons[int(cid)] = (c.get("ClassType") or "None", dbl)
        except Exception:
            weapons = {}
    _WEAPONS = weapons
    return _WEAPONS


def stance(stance_id):
    """One stance row, or None if the table has no such id."""
    return _load().get(int(stance_id))


def run_speed(stance_id=DEFAULT_STANCE, fallback=FALLBACK_RSPD):
    """The stance's RSPD = the speed a click-move runs at."""
    row = stance(stance_id)
    if not row or row["rspd"] <= 0:
        return float(fallback)
    return row["rspd"]


def walk_speed(stance_id=DEFAULT_STANCE, fallback=FALLBACK_RSPD):
    """The stance's WSPD = the speed of a walk (not what a click uses)."""
    row = stance(stance_id)
    if not row or row["wspd"] <= 0:
        return float(fallback)
    return row["wspd"]


def job_stances(job):
    """Every stance the character's job knows - the START_INFO stance list.

    This is CStanceList::Init: walk the job's stance conditions and add each
    condition's stances, keeping order and skipping repeats.
    """
    seen = []
    conds = _load_conditions()
    for cid in _load_jobs().get(int(job), ()):
        cond = conds.get(cid)
        if not cond:
            continue
        for sid in cond["stances"]:
            if sid not in seen:
                seen.append(sid)
    return seen


def job_skills(job):
    """Every skill the character's stances refer to - the START_INFO skill list.

    CStanceList::GetUseSkillID: walk the stances in order and take each non-zero
    SkillID1..5; CSkillList::Init then builds one entry per distinct id.
    """
    rows = _load()
    seen = []
    for sid in job_stances(job):
        row = rows.get(sid)
        if not row:
            continue
        for skill in row["skills"]:
            if skill not in seen:
                seen.append(skill)
    return seen


def hand_class_types(equip):
    """(LHand, RHand) classType strings for an {equip spot: item class id} map.

    Mirrors CSession::GetStanceByCondition: a right-hand item flagged
    DBLHand/DblAttach hides the left-hand item (two-handed weapon).
    """
    weapons = _load_weapons()

    def ctype(item_id):
        if not item_id:
            return "None"
        return weapons.get(int(item_id), ("None", False))[0]

    left = ctype(equip.get(4))                  # EQUIP_SPOT_LHAND
    rhand = equip.get(5)                        # EQUIP_SPOT_RHAND
    right = ctype(rhand)
    if rhand and weapons.get(int(rhand), ("", False))[1]:
        left = "None"
    return left, right


def active_stance(job, equip):
    """The stance the client will actually stand in, or None.

    The first stance of the first job condition whose weapon classTypes match.
    The client computes the same thing (GetStanceByCondition), then takes [0].
    """
    left, right = hand_class_types(equip)
    conds = _load_conditions()
    for cid in _load_jobs().get(int(job), ()):
        cond = conds.get(cid)
        if cond and cond["l"] == left and cond["r"] == right:
            for sid in cond["stances"]:
                return sid
    return None


def level_for(stance_id, exp, xp_types, levels):
    """Stance level for `exp`: highest MIN_EXP band that fits (geStanceLevelTable).

    The XP type comes from the CLIENT's own stance table first
    (`datatable_stance.xml` -> `XPType`, complete for all 427 stances), then the
    DB's `STANCE_XP_TYPE` as a fallback. The DB copy is an OLDER list: it stops
    at stance id 30540 and has no row for the newer 30xxx stances (30910 First
    Aid, 30920 Tactical Assistance, 30930 Enhanced Tactics, 30950 Imperial
    Tactics Rare), so using it alone reported every one of them as level 1 while
    their own exp row sat right there in `STANCE`.
    """
    sid = int(stance_id)
    xp = (xp_types or {}).get(sid)
    if not xp:
        xp = (_load().get(sid) or {}).get("xp")
    if not xp or exp <= 0:
        return 1
    best = 1
    for lv, min_exp in (levels or {}).get(xp, ()):
        if exp >= min_exp:
            best = lv
        else:
            break
    return best


def build_stance_records(job, db_rows=None, xp_types=None, levels=None):
    """The START_INFO stance block: packed STANCE_INFO records (14 bytes each).

    type = stance ClassID, level/exp/skillPoint from the character's own STANCE
    rows (level derived from exp). Missing rows send level 1 / exp 0.
    """
    out = bytearray()
    for sid in job_stances(job):
        exp, sp = (db_rows or {}).get(sid, (0, 0))
        out += struct.pack("<IHII", sid,
                           level_for(sid, exp, xp_types, levels),
                           exp, sp)
    return bytes(out)


def stance_count():
    """How many stance rows the table has (for the boot log)."""
    return len(_load())


def summary(stance_id=DEFAULT_STANCE):
    """'Bareknuckle RSPD=469 WSPD=115 StnMSPD=1' - for the boot log."""
    row = stance(stance_id)
    if not row:
        return "(no row for stance %d, using %.0f)" % (stance_id, FALLBACK_RSPD)
    return ("%s RSPD=%.0f WSPD=%.0f StnMSPD=%g HoldMove=%s"
            % (row["name"], row["rspd"], row["wspd"], row["mspd"],
               "YES" if row["hold"] else "NO"))


if __name__ == "__main__":
    rows = _load()
    print("%d stances from %s" % (len(rows), STANCE_XML))
    for sid in (DEFAULT_STANCE, GM_STANCE, 10011, 10012):
        print("  %d %s" % (sid, summary(sid)))
    print("jobs %d, conditions %d, weapon rows %d"
          % (len(_load_jobs()), len(_load_conditions()), len(_load_weapons())))
    for job in (1, 3, 72, 156):
        print("job %-4d stances %s" % (job, job_stances(job)))
    # A Fighter with the DB's own weapons (spots 4/5 = 7215/7201).
    print("fighter hands", hand_class_types({4: 7215, 5: 7201}),
          "-> stance", active_stance(1, {4: 7215, 5: 7201}))
    print("unarmed     ", hand_class_types({}),
          "-> stance", active_stance(72, {}))
    for job in (1, 3, 6, 72):
        print("job %-4d skills %s" % (job, job_skills(job)))
    print("grandies stance block %d bytes"
          % len(build_stance_records(72, {10010: (0, 0), 10420: (315303, 16)})))
    slow = sorted((r["rspd"], cid, r["name"]) for cid, r in rows.items())
    print("slowest rspd:", slow[:3])
    print("fastest rspd:", slow[-3:])

