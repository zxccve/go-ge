"""skill_data - the skill list a character sends in START_INFO.

The original builds a PC's skills from its STANCES, not from the job directly:
`CSkillList::Init` calls `CPlayerChar::GetUseSkillID`, which walks every stance of
the stance list and adds each non-zero `SkillID1..5` (SkillList.cpp:42,
StanceList.cpp:29). So the skill set follows from `stance_data.job_stances(job)`.

`SKILL_INFO` is 10 bytes packed (`#pragma pack(1)` geZCTypes.h:308):
    DWORD type; WORD level; DWORD exp;      // exp = REMAINING exp to the next level

`CSkillList::GetAllSkills` fills it as:
    level = skill level for the stored exp
    exp   = curve[level + 1] - stored_exp

Level curve: the "Xp" idspace, rows with ClassID > 20000, attributes Lv1..LvN
(geSkillLevelTable, SkillLevelTable.cpp:113). Which row a skill uses is its
`XPType` in `datatable_skill.xml`. Stored exp is the DB `SKILL` table.

Env:
  GE_SKILL_XML     override datatable_skill.xml
  GE_SKILL_CACHE   JSON cache of skill -> xp type
"""
import json
import os
import re
import struct
import xml.etree.ElementTree as ET

import ge_tables
import xml_tables
from ge_paths import CACHE as CACHE_DIR, GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))
SKILL_XML = os.environ.get("GE_SKILL_XML", os.path.join(GAME_ROOT, "ge", "xml", "datatable_skill.xml"))
CACHE = os.environ.get("GE_SKILL_CACHE", os.path.join(CACHE_DIR, "skill_xp_cache.json"))
CACHE_VERSION = 1

_XP_TYPES = None


def _decode(path):
    """Read one of the game's XML tables, whatever its encoding.

    The tree is mixed (UTF-8 for the rebuilt tables, EUC-KR for the
    hand-made ones) - see `xml_tables`.
    """
    return xml_tables.read_table_text(path)


def _build_cache():
    data = {"version": CACHE_VERSION, "xp": {}}
    try:
        root = ET.fromstring(_decode(SKILL_XML))
        for c in root.iter("Class"):
            cid, xp = c.get("ClassID"), c.get("XPType")
            if cid and xp and cid.isdigit() and xp.isdigit():
                data["xp"][cid] = int(xp)
    except (OSError, ET.ParseError):
        pass
    with open(CACHE, "w", encoding="utf-8") as f:
        json.dump(data, f)
    return data


def _load_xp_types():
    global _XP_TYPES
    if _XP_TYPES is None:
        data = None
        if os.path.exists(CACHE):
            try:
                with open(CACHE, encoding="utf-8") as f:
                    data = json.load(f)
            except (OSError, ValueError):
                data = None
        if not data or data.get("version") != CACHE_VERSION:
            data = _build_cache()
        _XP_TYPES = {int(k): int(v) for k, v in data["xp"].items()}
    return _XP_TYPES


def skill_xp_type(skill_id):
    return _load_xp_types().get(int(skill_id))


def skill_level_and_remaining(skill_id, exp):
    """(level, remaining exp) exactly like CSkillList::GetAllSkills."""
    xp = skill_xp_type(skill_id)
    level, remaining = 1, 0
    if xp:
        level = ge_tables.skill_level(xp, exp)
        remaining = ge_tables.skill_exp_at(xp, level + 1) - int(exp)
        if remaining < 0:
            remaining = 0
    return level, remaining


def build_skill_records(job, db_rows=None):
    """Packed SKILL_INFO records (10 bytes each) for a character's job."""
    import stance_data
    out = bytearray()
    for sid in stance_data.job_skills(job):
        exp = (db_rows or {}).get(sid, 0)
        level, remaining = skill_level_and_remaining(sid, exp)
        out += struct.pack("<IHI", sid, level, remaining)
    return bytes(out)


if __name__ == "__main__":
    import stance_data
    print("skill xp types: %d (cache %s)" % (len(_load_xp_types()), CACHE))
    for job in (1, 6, 72):
        skills = stance_data.job_skills(job)
        print("job %-3d %d skills %s" % (job, len(skills), skills[:12]))
    for sid in (11241, 11242, 11243):
        print(sid, "exp 9 ->", skill_level_and_remaining(sid, 9))
    rec = build_skill_records(6, {11241: 9, 11242: 9, 11243: 16})
    print("record bytes", len(rec), "=", len(rec) // 10, "skills")
