"""ies_pcinfo_diff - compare the PC_INFO values the ORIGINAL barrack server would
compute from the game's own data tables against what our Python server sends.

Original formulas (source2, geServer/BarrackServer):
  Character.cpp:49      level  = gePCLevelTable::GetLevel(exp)   -> SetLv(level)
  Character.cpp:52      nextExp = gePCLevelTable::GetExp(level+1)
  CharacterSlot.cpp:355 promotionLvByExp = gePCLevelTable::GetPromotionLevel(exp)
  CharacterSlot.cpp:356 promotionStep    = character->GetPromotionStep()
  CharacterSlot.cpp:368 promotionLv = min(promotionLvByExp, promotionStep)
  CharacterSlot.cpp:372 cashPromotionLv  = GetCashPromotionStep()

The table access is SHARED with the server (ge_tables), so this tool and the
server can never disagree about which curve is the real one.

Usage:  python ies_pcinfo_diff.py [commander_id]
"""
import sys

import ge_db
import ge_tables

NEXT_EXP_GUESS = 10_000_000  # what ge_barrack_server._next_exp() adds


def main():
    cmd_id = int(sys.argv[1]) if len(sys.argv) > 1 else 24
    promos = ge_db.get_character_promotion(cmd_id)

    hdr = ("%-4s %-10s %-13s %-7s | %-7s %-13s %-4s %-4s | %-6s %-13s %-5s  %s"
           % ("slot", "name", "EXP", "DB LIFE", "REAL LV", "REAL nextEXP",
              "pLv", "cLv", "OUR LV", "OUR nextEXP", "OUR pLv", "diff"))
    print(hdr)
    print("-" * len(hdr))

    for c in ge_db.get_characters(cmd_id):
        exp = int(c["exp"])
        pstep, cstep = promos.get(c["character_id"], (0, 0))
        lv = ge_tables.level_of(exp)
        next_exp = ge_tables.exp_of(lv + 1)
        plv = ge_tables.promotion_level(exp, pstep)
        our_lv, our_next, our_p = int(c["life"]), exp + NEXT_EXP_GUESS, 0

        flag = []
        if lv != our_lv:
            flag.append("LV")
        if next_exp != our_next:
            flag.append("nextEXP")
        if plv != our_p:
            flag.append("promotionLv")
        if cstep != 0:
            flag.append("cashPromotionLv")
        print("%-4d %-10s %-13d %-7d | %-7d %-13d %-4d %-4d | %-6d %-13d %-5d  %s"
              % (c["slot"], c["name"][:10], exp, int(c["life"]), lv, next_exp,
                 plv, cstep, our_lv, our_next, our_p, ",".join(flag)))


if __name__ == "__main__":
    main()
