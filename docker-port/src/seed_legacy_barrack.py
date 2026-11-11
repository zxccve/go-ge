"""seed_legacy_barrack.py - copy ONE commander's barrack rows into PostgreSQL.

Companion to `seed_legacy_account.py`: that one copies the login row, this one
copies what the barrack screen needs - the commander, its characters, the worn
items and their reinforcement.

It is a FIXTURE TOOL, not the data migration: it copies exactly one commander
(and only the characters and items that commander owns), it is an upsert, and it
prints what it wrote. The full row move stays a separate, later job.

No PostgreSQL driver is needed: it writes readable SQL and hands it to
`docker exec -i granado_pg psql`, exactly like the account seeder.

Usage (from src\\):
    python seed_legacy_barrack.py 24            # commander id
    python seed_legacy_barrack.py 24 --dry      # write the .sql, do not apply
"""
import os
import subprocess
import sys

import ge_db
import ge_paths

CONTAINER = "granado_pg"
PG_USER = "granado"
PG_DB = "ge_data"
OUT_DIR = os.path.join(ge_paths.ROOT, "out")

# The legacy rows come back as STRINGS; ints are converted once, here. A SQL
# Server `bit` arrives as "True"/"False", not "1"/"0", so that is handled too.
def INT(v):
    if v in (None, "", "NULL"):
        return 0
    if isinstance(v, bool):
        return 1 if v else 0
    if str(v).lower() in ("true", "false"):
        return 1 if str(v).lower() == "true" else 0
    return int(float(v))


def q(value):
    if value is None:
        return "NULL"
    return "'%s'" % str(value).replace("'", "''")


def rows(sql, db="GE_DATA"):
    return ge_db._run(sql, db=db)


def build_sql(commander_id):
    lines = []

    cm = rows("SELECT COMMANDER_ID, ACCOUNT_ID, MEMBER_ID, NAME, NATION_ID, "
              "NATION_POINT, RANK, DUTY, FAME, KARMA, DUELWIN, DUELLOST, DRAW, "
              "LADDER, ZONE_ID, CHANNEL_ID, POINT FROM COMMANDER "
              "WHERE COMMANDER_ID = %d" % commander_id)
    if not cm:
        print("no commander %d in GE_DATA.COMMANDER" % commander_id)
        return None, 0, 0
    c = cm[0]
    lines.append(
        "INSERT INTO commander (commander_id, account_id, member_id, name, "
        "nation_id, nation_point, rank, duty, fame, karma, duel_win, duel_lost, "
        "duel_draw, ladder, zone_id, channel_id, point) VALUES "
        "(%d, %d, %s, %s, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d) "
        "ON CONFLICT (commander_id) DO UPDATE SET name = EXCLUDED.name, "
        "zone_id = EXCLUDED.zone_id, ladder = EXCLUDED.ladder, point = EXCLUDED.point;"
        % (commander_id, INT(c[1]), q(c[2]), q(c[3]), INT(c[4]), INT(c[5]),
           INT(c[6]), INT(c[7]), INT(c[8]), INT(c[9]), INT(c[10]), INT(c[11]),
           INT(c[12]), INT(c[13]), INT(c[14]), INT(c[15]), INT(c[16])))

    # Per-commander properties (Lock_CharSlot, Lock_TeamGroup, BrkName*).
    props = rows("SELECT COMMANDER_ID, PROPERTY, SECTION, VALUE "
                 "FROM COMMANDER_PROPERTY WHERE COMMANDER_ID = %d" % commander_id)
    for pid, name, section, value in props:
        lines.append(
            "INSERT INTO commander_property (commander_id, property, section, value) "
            "VALUES (%d, %s, %d, %s) ON CONFLICT (commander_id, property, section) "
            "DO UPDATE SET value = EXCLUDED.value;"
            % (INT(pid), q(name), INT(section), q(value)))

    # Characters.
    chars = rows("SELECT CHARACTER_ID, SLOT, NAME, TEAMPOS, GENDER, JOB, HEADTYPE, "
                 "EXP, LIFE, HP, SP, ISCARD, XPOS, YPOS, ZPOS FROM CHARACTERS "
                 "WHERE COMMANDER_ID = %d ORDER BY SLOT" % commander_id)
    char_ids = []
    for ch in chars:
        cid = INT(ch[0])
        char_ids.append(cid)
        lines.append(
            "INSERT INTO character (character_id, commander_id, slot, name, teampos, "
            "gender, job, headtype, exp, life, hp, sp, iscard, pos_x, pos_y, pos_z) "
            "VALUES (%d, %d, %d, %s, %d, %s, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d) "
            "ON CONFLICT (character_id) DO UPDATE SET slot = EXCLUDED.slot, "
            "name = EXCLUDED.name, exp = EXCLUDED.exp, hp = EXCLUDED.hp, sp = EXCLUDED.sp;"
            % (cid, commander_id, INT(ch[1]), q(ch[2]), INT(ch[3]),
               "true" if INT(ch[4]) else "false", INT(ch[5]), INT(ch[6]), INT(ch[7]),
               INT(ch[8]), INT(ch[9]), INT(ch[10]), INT(ch[11]), INT(ch[12]),
               INT(ch[13]), INT(ch[14])))

    # Barrack captions (COMMANDER_BARRACK_NAME) - the reference account renamed
    # its first barrack, and the client shows the caption we send.
    names = rows("SELECT CHANNEL, NAME FROM COMMANDER_BARRACK_NAME WHERE COMMANDER_ID = %d"
                 % commander_id)
    for channel, name in names:
        lines.append(
            "INSERT INTO commander_barrack_name (commander_id, channel, name) "
            "VALUES (%d, %d, %s) ON CONFLICT (commander_id, channel) "
            "DO UPDATE SET name = EXCLUDED.name;"
            % (commander_id, INT(channel), q(name)))

    # Per-character promotion steps (CHARACTERS_PROPERTY). The packet's
    # promotionLv is min(band(level), PromotionStep); cashPromotionLv is the raw
    # CashPromotionStep. A character with no row gets 0, as the original sends.
    props = rows("SELECT p.CHARACTER_ID, p.PROPERTY, p.VALUE FROM CHARACTERS_PROPERTY p "
                 "JOIN CHARACTERS c ON c.CHARACTER_ID = p.CHARACTER_ID "
                 "WHERE c.COMMANDER_ID = %d AND p.PROPERTY IN "
                 "('PromotionStep','CashPromotionStep')" % commander_id)
    for cid, prop, value in props:
        lines.append(
            "INSERT INTO character_property (character_id, property, section, value) "
            "VALUES (%d, %s, 0, %s) ON CONFLICT (character_id, property, section) "
            "DO UPDATE SET value = EXCLUDED.value;"
            % (INT(cid), q(prop), q(value)))

    if not char_ids:
        return lines, len(char_ids), 0

    # Worn items + their class id + their "+N" reinforcement.
    id_list = ",".join(str(i) for i in char_ids)
    worn = rows("SELECT w.CHARACTER_ID, w.SPOT, i.ITEM_ID, i.CLASS_ID, i.COUNT, "
                "ISNULL(r.HIT_COUNT, 0) FROM WEARITEM w "
                "JOIN ITEM i ON i.ITEM_ID = w.ITEM_ID "
                "LEFT JOIN ITEM_REINFORCE r ON r.ITEM_ID = w.ITEM_ID "
                "WHERE w.CHARACTER_ID IN (%s)" % id_list)
    items = set()
    for w in worn:
        item_id = INT(w[2])
        if item_id not in items:
            items.add(item_id)
            lines.append(
                "INSERT INTO item (item_id, class_id, count, type) VALUES (%d, %d, %d, 0) "
                "ON CONFLICT (item_id) DO UPDATE SET class_id = EXCLUDED.class_id;"
                % (item_id, INT(w[3]), INT(w[4])))
            lines.append(
                "INSERT INTO item_reinforce (item_id, atk, def, hit_count, wlv) "
                "VALUES (%d, 0, 0, %d, 1) ON CONFLICT (item_id) DO UPDATE SET "
                "hit_count = EXCLUDED.hit_count;" % (item_id, INT(w[5])))
        lines.append(
            "INSERT INTO wear_item (character_id, spot, item_id) VALUES (%d, %d, %d) "
            "ON CONFLICT (character_id, spot) DO UPDATE SET item_id = EXCLUDED.item_id;"
            % (INT(w[0]), INT(w[1]), item_id))

    return lines, len(char_ids), len(worn)


def apply(sql_path):
    with open(sql_path, encoding="utf-8") as f:
        sql = f.read()
    cmd = ["docker", "exec", "-i", CONTAINER, "psql", "-U", PG_USER,
           "-d", PG_DB, "-v", "ON_ERROR_STOP=1", "-q"]
    proc = subprocess.run(cmd, input=sql, text=True, capture_output=True)
    if proc.returncode != 0:
        print("psql failed:\n%s\n%s" % (proc.stdout, proc.stderr))
        return False
    if proc.stdout.strip():
        print(proc.stdout.strip())
    return True


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    commander_id = int(argv[1])
    dry = "--dry" in argv

    lines, chars, worn = build_sql(commander_id)
    if lines is None:
        return 1

    os.makedirs(OUT_DIR, exist_ok=True)
    path = os.path.join(OUT_DIR, "seed_barrack_%d.sql" % commander_id)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write("-- Seed fixture for the barrack screen, commander %d.\n" % commander_id)
        f.write("-- One commander only. This is NOT the data migration.\nBEGIN;\n")
        f.write("\n".join(lines) + "\nCOMMIT;\n")
    print("commander %d: %d statement(s), %d character(s), %d worn item(s)"
          % (commander_id, len(lines), chars, worn))
    print("wrote %s" % path)

    if dry:
        print("--dry: not applied")
        return 0
    if not apply(path):
        return 1
    print("applied to %s:%s" % (CONTAINER, PG_DB))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
