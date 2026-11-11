"""make_account.py - create a second account so a friend can play too.

The port had exactly ONE account (account 35, commander 24, login "arcanine"),
so every player who logged in got the SAME character. The zone now resolves
the player from the account id in CZ_START_GAME, but the account has to
exist.

This writes to GE_DATA, so take a backup first (see OPERATIONS.md).

Usage:
    python make_account.py <login> [commander_name] [nation] [zone_id]

Prints the new ACCOUNT_ID and COMMANDER_ID, which are what the client and
the zone use.
"""
import sys

import ge_db


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 1
    login = sys.argv[1]
    cname = sys.argv[2] if len(sys.argv) > 2 else login
    nation = sys.argv[3] if len(sys.argv) > 3 else "USA"
    zone_id = int(sys.argv[4]) if len(sys.argv) > 4 else 10

    if ge_db.get_account(login):
        print("account %r already exists: %s" % (login, ge_db.get_account(login)))
        return 1

    rows = ge_db._run("SELECT ISNULL(MAX(ACCOUNT_ID),0)+1 FROM ACCOUNT_INFO")
    account_id = int(rows[0][0])
    rows = ge_db._run("SELECT ISNULL(MAX(COMMANDER_ID),0)+1 FROM COMMANDER")
    commander_id = int(rows[0][0])

    # The nation is the 3-letter code the client sends (DEU, USA, ...).
    nat = {"USA": 1, "DEU": 2, "KOR": 3, "JPN": 4, "CHN": 5, "TWN": 6}.get(nation.upper(), 1)

    ge_db._run("INSERT INTO ACCOUNT_INFO (ACCOUNT_ID, COMMANDER_ID, MEMBER_ID) "
               "VALUES (%d, %d, '%s')"
               % (account_id, commander_id, login.replace("'", "''")))
    ge_db._run("INSERT INTO COMMANDER (COMMANDER_ID, ACCOUNT_ID, MEMBER_ID, NAME, "
               "NATION_ID, NATION_POINT, RANK, DUTY, FAME, KARMA, DUELWIN, DUELLOST, "
               "DRAW, LADDER, ZONE_ID, CHANNEL_ID, POINT) VALUES "
               "(%d, %d, '%s', '%s', %d, 0, 0, 0, 0, 0, 0, 0, 0, 0, %d, 0, 0)"
               % (commander_id, account_id, login.replace("'", "''"),
                  cname.replace("'", "''"), nat, zone_id))
    print("created account %r: ACCOUNT_ID=%d COMMANDER_ID=%d (zone %d, %s)"
          % (login, account_id, commander_id, zone_id, nation))

    # A starter character, so the friend has something to play.
    #
    # The job matters: a job with many stances makes the zone send a long
    # stance/skill list, and the client has never been seen to like that (see
    # history/multiplayer.md - the client crashed on a brand-new, level-1
    # character with 25 stances and 94 skills). So the default is the job of
    # an existing character that is known to work.
    job = int(sys.argv[5]) if len(sys.argv) > 5 else 72
    exp = int(sys.argv[6]) if len(sys.argv) > 6 else 22133655
    try:
        cid = ge_db.create_character(commander_id, 0, cname, 1, job, 0,
                                     life=99, exp=exp, hp=100, sp=100)
        print("created character %r in slot 0 (character id %s, job %d)"
              % (cname, cid, job))
    except Exception as exc:
        print("could not create a character: %s" % exc)
        print("create one in the client instead.")
        return 0
    # Copy the stance rows of a character that is known to work, so the zone
    # sends the same short stance/skill list. (The worn items cannot be
    # copied: WEARITEM has a unique key on the item, so an item can only be
    # worn once in the whole database.)
    try:
        rows = ge_db._run("SELECT TOP 1 CHARACTER_ID FROM STANCE "
                          "WHERE CHARACTER_ID<>%d ORDER BY CHARACTER_ID" % cid)
        if rows:
            template = int(rows[0][0])
            ge_db._run("INSERT INTO STANCE (CHARACTER_ID, STANCE_ID, EXP, "
                       "SKILL_POINT) SELECT %d, STANCE_ID, EXP, SKILL_POINT "
                       "FROM STANCE WHERE CHARACTER_ID=%d" % (cid, template))
            print("copied the stance rows from character %d" % template)
    except Exception as exc:
        print("could not copy the stance rows: %s" % exc)
    return 0


if __name__ == "__main__":
    sys.exit(main())
