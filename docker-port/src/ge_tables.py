"""ge_tables - the slice of the game's own data tables the barrack server needs.

The original barrack server loads its own copy of the game data at boot
(CBarrackServer::InitIES: SetCurrentDirectory("ies") + CDatatableLoader), and it
computes four PC_INFO fields from it that we used to guess:

    LV              = gePCLevelTable::GetLevel(exp)          Character.cpp:49
    nextEXP         = gePCLevelTable::GetExp(level + 1)      Character.cpp:52
    promotionLv     = min(GetPromotionLevel(exp),
                          character->GetPromotionStep())     CharacterSlot.cpp:368
    cashPromotionLv = character->GetCashPromotionStep()      CharacterSlot.cpp:372

The two tables live in the server data folder:
    ies\\datatable_xp.ies              idspace "Xp"             Lv, NextXp, TotalXp
    ies\\datatable_promotion_step.ies  idspace "promotion_step" BeginLv, EndLv, ClassID

The per-character PromotionStep / CashPromotionStep values are rows in
CHARACTERS_PROPERTY (the original reads them with S_GET_CHARACTERS_PROPERTY_2,
see BarrackServer/PCLoader.cpp).

We convert the two .ies with the game's OWN converter (Converter\\ies2.py) once and
cache the result as JSON, so the running server never needs the converter.

Env:
  GE_TABLES_DIR        server data folder          (default <path>)
  GE_TABLES_CACHE      JSON cache path             (default <this dir>\\ge_tables.json)
  GE_TABLES_CONVERTER  folder holding ies2.py      (default the client's Converter)
"""
import json
import os
import sys

from ge_paths import CACHE as CACHE_DIR, GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))

TABLES_DIR = os.environ.get("GE_TABLES_DIR", os.path.join(GAME_ROOT, "ge"))
CACHE = os.environ.get("GE_TABLES_CACHE", os.path.join(CACHE_DIR, "ge_tables.json"))
CONVERTER = os.environ.get(
    "GE_TABLES_CONVERTER", os.path.join(GAME_ROOT, "GEAndromida", "Converter"))

_LEVELS = None       # [(lv, total_exp, next_exp)] sorted by lv
_BANDS = None        # [(begin_lv, end_lv, class_id)]
_SKILL_CURVES = None  # {xp type: [Lv1..LvN]} for the rows with ClassID > 20000
_COMMANDER = None     # [(lv, total_exp)] the commander/family level curve
CACHE_VERSION = 4    # bump when the parsing below changes, so old caches rebuild


def _convert(name):
    """datatable_<name>.ies -> parsed XML root, using the game's own converter."""
    sys.path.insert(0, CONVERTER)
    import ies2  # noqa: E402
    import tempfile
    import xml.etree.ElementTree as ET
    src = os.path.join(TABLES_DIR, "ies", name + ".ies")
    dst = os.path.join(tempfile.gettempdir(), "ge_tables_" + name + ".xml")
    ies2.ies_to_xml(src, dst, None, None, "UTF-8", False)
    return ET.parse(dst).getroot()


def _build_cache():
    xp = _convert("datatable_xp")
    # The "Xp" idspace holds SEVERAL unrelated curves side by side:
    #   ClassID 1..200   Lv 1..200  <- the real PC level curve (NextXp is the
    #                                  difference of consecutive TotalXp)
    #   ClassID 1001..   Lv 1..99   <- the COMMANDER (family) level curve, read
    #                                  by geCommLevelTable via the Xp class list
    #                                  (CommLevelTable.cpp:62 keeps ClassID
    #                                  1000..2000). NextXp is always 0 here,
    #                                  so it must NOT be scanned as PC levels.
    #   ClassID 2001.., 10001..     <- stray rows with Lv 0
    # Only the first is self-consistent, and only there is ClassID == Lv. Take
    # those rows; a naive "highest Lv whose TotalXp <= exp" scan mixes the two
    # curves and reports a level far too high (it gave 131 where the real answer
    # is 100).
    levels = []
    commander = []
    for c in xp:
        cid = int(c.get("ClassID"))
        if cid == int(c.get("Lv")):
            levels.append((int(c.get("Lv")), int(c.get("TotalXp")),
                           int(c.get("NextXp"))))
        elif 1000 < cid < 2000:
            commander.append((int(c.get("Lv")), int(c.get("TotalXp"))))
    levels.sort()
    commander.sort()
    promo = _convert("datatable_promotion_step")
    bands = sorted((int(c.get("BeginLv")), int(c.get("EndLv")), int(c.get("ClassID")))
                   for c in promo)
    # Skill levels use the SAME "Xp" idspace but the rows with ClassID > 20000,
    # where the levels are the attributes Lv1..LvN (geSkillLevelTable,
    # SkillLevelTable.cpp:113). Keep those curves too.
    curves = {}
    for c in xp:
        cid = int(c.get("ClassID"))
        if cid <= 20000:
            continue
        vals = []
        n = 1
        while True:
            v = c.get("Lv%d" % n)
            if v is None:
                break
            vals.append(int(v))
            n += 1
        if vals:
            curves[cid] = vals
    data = {"version": CACHE_VERSION, "levels": levels, "bands": bands,
            "curves": curves, "commander": commander}
    with open(CACHE, "w", encoding="utf-8") as f:
        json.dump(data, f)
    return data


def _load():
    global _LEVELS, _BANDS, _SKILL_CURVES, _COMMANDER
    if _LEVELS is not None:
        return
    data = None
    if os.path.exists(CACHE):
        try:
            with open(CACHE, encoding="utf-8") as f:
                data = json.load(f)
        except (OSError, ValueError):
            data = None
    if not data or data.get("version") != CACHE_VERSION:
        data = _build_cache()
    _LEVELS = [tuple(x) for x in data["levels"]]
    _BANDS = [tuple(x) for x in data["bands"]]
    _SKILL_CURVES = {int(k): list(v) for k, v in data.get("curves", {}).items()}
    _COMMANDER = [tuple(x) for x in data.get("commander", [])]


def level_of(exp):
    """gePCLevelTable::GetLevel - highest level whose TotalXp <= exp."""
    _load()
    lv = _LEVELS[0][0]
    for l, total, _ in _LEVELS:
        if exp >= total:
            lv = l
        else:
            break
    return lv


def exp_of(level):
    """gePCLevelTable::GetExp - cumulative exp needed to BE at `level`."""
    _load()
    for l, total, _ in _LEVELS:
        if l == level:
            return total
    return _LEVELS[-1][1]


def next_exp_of(level):
    """The table's NextXp for `level` (exp needed to reach level+1)."""
    _load()
    for l, _, nxt in _LEVELS:
        if l == level:
            return nxt
    return 0


def promotion_band(level):
    """promotion_step ClassID whose [BeginLv, EndLv) contains `level`, else 0."""
    _load()
    for b, e, cid in _BANDS:
        if b <= level < e:
            return cid
    return 0


def promotion_level(exp, promotion_step):
    """CCharacterSlot::MakeBCPacket: min(promotionLvByExp, promotionStep)."""
    by_exp = promotion_band(level_of(exp))
    return min(by_exp, int(promotion_step or 0))


# geSkillLevelTable.h: skill levels stop at 10.
MAX_SKILL_LV = 10


def skill_level(xp_type, exp):
    """geSkillLevelTable::GetSkillLevel, quirks included.

    The C++ loop returns MAX_SKILL_LV as soon as `exp` reaches the LAST curve
    value, otherwise the first band [exp[i-1], exp[i]). Reproduce both branches.
    """
    _load()
    curve = _SKILL_CURVES.get(int(xp_type))
    if not curve:
        return 1
    curve = (list(curve) + [0] * MAX_SKILL_LV)[:MAX_SKILL_LV]
    if exp >= curve[MAX_SKILL_LV - 1]:
        return MAX_SKILL_LV
    for i in range(1, MAX_SKILL_LV):
        if curve[i - 1] <= exp < curve[i]:
            return i
    return 0


def skill_exp_at(xp_type, level):
    """The curve's Lv<level> threshold (what CSkillList uses for the bar)."""
    _load()
    curve = _SKILL_CURVES.get(int(xp_type))
    if not curve:
        return 0
    curve = (list(curve) + [0] * MAX_SKILL_LV)[:MAX_SKILL_LV]
    if level <= 1:
        return curve[0]
    if level > MAX_SKILL_LV:
        return curve[MAX_SKILL_LV - 1]
    return curve[level - 1]


# geShared.h:183
MAX_COMM_LV = 99


def commander_exp_of(level):
    """geCommLevelTable::GetExp - the commander curve's TotalXp for `level`."""
    _load()
    for lv, total in _COMMANDER:
        if lv == level:
            return total
    return 0


def commander_level_of(exp):
    """geCommLevelTable::GetLevel, branch for branch (CommLevelTable.cpp:142).

    The C++ scans i = 2..99 and returns i-1 for the band [exp(i-1), exp(i));
    at i == 99 it also returns 99 once `exp` reaches exp(99). `exp` below
    exp(1) (which is 0) cannot happen, so level 0 is only the fallthrough.
    """
    _load()
    for i in range(2, MAX_COMM_LV + 1):
        e1, e2 = commander_exp_of(i - 1), commander_exp_of(i)
        if e1 <= exp < e2:
            return i - 1
        if i == MAX_COMM_LV and exp >= e1:
            return i
    return 0


def commander_level_percent(exp):
    """geCommLevelTable::GetLevelPercentage, the same banding as GetLevel."""
    _load()
    for i in range(2, MAX_COMM_LV + 1):
        e1, e2 = commander_exp_of(i - 1), commander_exp_of(i)
        if e1 <= exp < e2:
            return ((exp - e1) * 100) // (e2 - e1)
        if i == MAX_COMM_LV and exp >= e1:
            return 100
    return 0


def commander_exp_from_characters(characters):
    """CCommander::ComputeCommanderLv's exp sum (BarrackServer\\Commander.cpp:856).

    `characters` is any iterable of (level, job, gender). The exp is built from
    the CHARACTER LEVELS only - no database column holds it:
      * level*level per character,
      * plus 500 * ((Lv-101)/10 + 1) for every level from 101 to the character's,
      * x1.2 for the HIGHEST-level character of each (job, gender) pair.
    """
    rows = [(int(lv), int(job), int(gen)) for lv, job, gen in characters]
    rows = [r for r in rows if r[0] != 0]

    unique = {}  # job*2+gender -> index of the highest-level holder
    for i, (lv, job, gen) in enumerate(rows):
        key = job * 2 + gen
        if key not in unique or lv > rows[unique[key]][0]:
            unique[key] = i

    total = 0
    for i, (lv, job, gen) in enumerate(rows):
        exp = lv * lv
        for count_level in range(101, lv + 1):
            exp += ((count_level - 101) // 10 + 1) * 500
        if unique.get(job * 2 + gen) == i:
            exp = int(exp * 1.2)  # C++ multiplies an int by 1.2, then truncates
        total += exp
    return total


if __name__ == "__main__":
    _load()
    print("levels %d (%d..%d), bands %s, skill curves %d"
          % (len(_LEVELS), _LEVELS[0][0], _LEVELS[-1][0],
             [b[2] for b in _BANDS], len(_SKILL_CURVES)))
    for e in (0, 419, 187183514, 19487183514, 22133655):
        lv = level_of(e)
        print("exp %-12d -> level %-4d next %-12d promo(step=4) %d"
              % (e, lv, exp_of(lv + 1), promotion_level(e, 4)))
