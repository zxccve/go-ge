"""test_continue_position - check the save -> resume chain end to end.

Usage:  python test_continue_position.py

It walks the whole "last point" feature the way the game does, without the
client:

  1. the ZONE saves a position (what its CZ_MOVE_PC handler does),
  2. the BARRACK reads it back for a CONTINUEGAME,
  3. the ZONE reads the handoff file and works out where to spawn.

It prints each step, and it FAILS (exit 1) if any step does not do its job.

This exists because the feature was broken for a long time and nobody noticed:
the zone's save call raised `NameError` (there is no `zone_id` in that
scope), the `except` swallowed it, and the log line that said so was invisible
because the zone's log file was broken too. So: the check is a CHECKER, not a
hope - run it after touching the save path.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ge_barrack_server as B
import ge_db
import zone_server as Z
from ge_paths import OUT

# A character of commander 24, and a position that is plausible for map 10.
CHARACTER_ID = 34
COMMANDER_ID = 24
SLOT = 0
POS = (4745, -24, 149)      # x, y, navmesh cell
MAP_ID = 10


def main():
    ok = True
    # The position is a REAL row in the live database, so save what is there and
    # put it back when we are done. Without this, running a read-only-looking
    # checker silently moves the player's "Last Point" to the test coordinate -
    # which happened once, on 2026-10-25.
    previous = None
    try:
        previous = ge_db.get_character_position(CHARACTER_ID)
    except Exception:
        previous = None
    if previous:
        print("0. the current saved position (will be restored)")
        print("   %s" % (previous,))

    print("1. the zone saves a position")
    try:
        ge_db.set_character_position(CHARACTER_ID, POS[0], POS[1], POS[2], MAP_ID)
        print("   saved %s for character %d" % (str(POS), CHARACTER_ID))
    except Exception as exc:
        print("   !! the save raised %s" % exc)
        return 1

    print("2. the barrack reads it back for a CONTINUEGAME")
    got = B._continue_point(COMMANDER_ID, [SLOT])
    print("   _continue_point -> %s" % (got,))
    if not got:
        print("   !! nothing to resume at - the position was not saved")
        ok = False
    elif tuple(got) != POS:
        print("   !! expected %s, got %s" % (str(POS), str(got)))
        ok = False

    print("3. the barrack writes the handoff the zone reads")
    # The zone reads the file of the player who is CONNECTED (CUR.commander_id),
    # and the barrack writes one file per commander plus the old single file.
    Z.CUR.commander_id = COMMANDER_ID
    path = Z.team_file_for(COMMANDER_ID)
    with open(path, "w", encoding="utf-8") as f:
        f.write('{"continue": true, "continue_pos": [%d, %d, %d], "picked": [%d]}'
                % (POS[0], POS[1], POS[2], SLOT))
    saved = Z._read_continue_pos()
    print("   _read_continue_pos -> %s" % (saved,))
    if not saved:
        print("   !! the zone did not read the handoff")
        ok = False
    else:
        spawn = Z._continue_spawn(saved)
        print("   _continue_spawn -> %s" % (spawn,))
        if not spawn:
            print("   !! the zone cannot spawn at the saved cell - it would "
                  "fall back to the map spawn")
            ok = False
    os.remove(path)

    # Put the player's real position back. This is the whole reason step 0 exists.
    if previous:
        try:
            ge_db.set_character_position(CHARACTER_ID, previous[0], previous[1],
                                         previous[2], previous[3])
            print("4. restored the position it found")
            print("   %s" % (previous,))
        except Exception as exc:
            print("   !! could not restore the previous position: %s" % exc)

    print("RESULT: %s" % ("OK" if ok else "FAILED"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main() or 0)
