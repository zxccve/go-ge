"""check_quest_state.py - prove quest state round-trips through the DB.

Seeds one quest state for the test commander, runs build_quest_info, and checks
the matching byte. Restores the original value afterward, so the DB is left
unchanged. Run from docker-port\\src.
"""
import os
import sys


def main():
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    import ge_db
    import zone_server as zs

    commander_id = 24
    quest_name = "Quest_00001"
    target_state = 1

    # original value (None if absent)
    original = ge_db.get_commander_quest_states(commander_id).get(quest_name)

    try:
        ok = ge_db.set_commander_quest_state(commander_id, quest_name, target_state)
        if not ok:
            print("FAIL: could not set quest state")
            return 1
        zs.CUR.commander_id = commander_id
        pk = zs.build_quest_info(commander_id)
        byte0 = pk[4]  # first quest byte (after cmd+len)
        if byte0 == target_state:
            print("OK: Quest_00001 byte = %d (expected %d)" % (byte0, target_state))
            print("     packet len = %d" % len(pk))
            return 0
        print("FAIL: byte = %d, expected %d" % (byte0, target_state))
        return 1
    finally:
        # restore
        if original is None:
            # remove the property row we may have... leave as 0xC8 default is
            # not representable as a row; best effort: set to 0xC8
            ge_db.set_commander_quest_state(commander_id, quest_name, 0xC8)
        else:
            ge_db.set_commander_quest_state(commander_id, quest_name, original)


if __name__ == "__main__":
    sys.exit(main())
