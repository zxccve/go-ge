"""ge_db - helper for the Granado Espada GE_DATA database.

The GE databases live in the SQL Server container `granado_espada`, published on
host port 14330.

TRANSPORT: `pyodbc` with a bounded connection pool. This used to shell out to
`sqlcmd` once per query, which cost ~43 ms per call (a process spawn plus a fresh
login) and made a multi-statement transaction impossible - `TxBegin .. TxEnd`
could never be atomic. Measured after the change: ~1 ms for the same query.

Every public function keeps the same shape as before: `_run()` still returns a
list of rows, each row a list of STRINGS, so no caller had to change.

Two traps from the sqlcmd era that are now gone, kept here so nobody re-adds
them: `_run` used to decode cp1252 so one Korean byte raised UnicodeDecodeError
deep inside subprocess, and errors used to arrive on STDOUT with exit code 0.
With a driver, exceptions are real exceptions - but `_run` still raises
`DbError`, because callers catch that.

NOTE: always confirm you are talking to the CONTAINER, not a host-installed
SQL Server: `SELECT @@SERVERNAME` must return the container id (a hex string
like f288a347597d), not DESKTOP-KHAIRIL. The host instance once took
0.0.0.0:1433 and hijacked `-S localhost` for a whole session; the container is
published on 14330 for exactly that reason. Override with env vars if it moves.
"""
import os
import queue
import threading

import pyodbc

# ---------------------------------------------------------------------------
# WHICH DATABASE: the Docker container, NOT the host's own SQL Server.
SERVER = os.environ.get("GE_SQL_SERVER", "localhost,14330")
USER = os.environ.get("GE_SQL_USER", "sa")
PASSWORD = os.environ.get("GE_SQL_PASSWORD", "<local-dev-password>")   # container SA
DB = "GE_DATA"

# One shared TCPS connection per checked-out query. A pool of 16 serves hundreds
# of players: SQL Server wants FEW long-lived connections, not many. Raised only
# on evidence (pool waits in the log), never "to be safe".
POOL_SIZE = int(os.environ.get("GE_SQL_POOL", "16"))
POOL_WAIT = float(os.environ.get("GE_SQL_WAIT", "10"))
# Driver names differ per machine (this host has 17, the container ships 18), so
# pick what is installed, newest first, unless GE_SQL_DRIVER names one.
DRIVER = os.environ.get("GE_SQL_DRIVER") or ""
_DRIVER_ORDER = ("ODBC Driver 18 for SQL Server", "ODBC Driver 17 for SQL Server",
                 "SQL Server Native Client 11.0", "SQL Server")


def _driver():
    global DRIVER
    if DRIVER:
        return DRIVER
    have = pyodbc.drivers()
    for want in _DRIVER_ORDER:
        if want in have:
            DRIVER = want
            return DRIVER
    raise DbError("no SQL Server ODBC driver installed (have: %s)" % ", ".join(have))


class DbError(RuntimeError):
    pass


def _connect(db):
    if USER:
        auth = "UID=%s;PWD=%s;" % (USER, PASSWORD)
    else:
        auth = "Trusted_Connection=yes;"
    conn = pyodbc.connect(
        "DRIVER={%s};SERVER=%s;DATABASE=%s;%sTrustServerCertificate=yes;"
        % (_driver(), SERVER, db, auth),
        timeout=int(POOL_WAIT),
    )
    conn.autocommit = False
    return conn


_pools = {}                  # database name -> idle connections
_pools_lock = threading.Lock()
_slots = threading.BoundedSemaphore(POOL_SIZE)


def _pool_for(db):
    """One idle-connection queue PER DATABASE.

    A single shared queue is wrong. GE_DATA and RESOURCE hold tables of the SAME
    NAME with DIFFERENT columns - `ACCOUNT_INFO.PASS` exists only in RESOURCE -
    so a connection checked out for another database answers "invalid column
    name", or worse, silently reads the other table. This is what broke login
    on 2026-10-23: `get_password` asked for RESOURCE and was handed a GE_DATA
    connection.
    """
    with _pools_lock:
        return _pools.setdefault(db, queue.Queue())


def _acquire(db):
    """A pooled connection for THIS database, or a new one while there is room."""
    if not _slots.acquire(timeout=POOL_WAIT):
        raise DbError("no free database connection after %.0f s (pool=%d)"
                      % (POOL_WAIT, POOL_SIZE))
    pool = _pool_for(db)
    try:
        conn = pool.get_nowait()
    except queue.Empty:
        conn = None
    if conn is not None:
        try:
            conn.cursor().execute("SELECT 1").fetchall()
            return conn
        except pyodbc.Error:
            try:
                conn.close()
            except Exception:
                pass
    try:
        return _connect(db or DB)
    except Exception:
        _slots.release()
        raise


def _release(conn, db=DB):
    try:
        if conn is not None:
            _pool_for(db).put_nowait(conn)
    finally:
        _slots.release()


def _cell(value):
    """One column, as the string sqlcmd used to print."""
    if value is None:
        return "NULL"
    if isinstance(value, bool):
        return "1" if value else "0"
    if isinstance(value, float):
        return "%g" % value
    if isinstance(value, bytes):
        return value.decode("latin-1")
    return str(value)


def _run(sql, db=DB):
    """Run SQL and return rows as lists of strings (the old sqlcmd contract).

    Commits on success and rolls back on error, so a caller can build a real
    transaction by running several statements on the same call... which it
    cannot: each call is its own transaction. Use `transaction()` for that.
    """
    conn = _acquire(db)
    try:
        cur = conn.cursor()
        cur.execute(sql)
        rows = []
        while True:
            if cur.description:
                rows.extend([[_cell(v) for v in row] for row in cur.fetchall()])
            if not cur.nextset():
                break
        conn.commit()
        return rows
    except pyodbc.Error as exc:
        try:
            conn.rollback()
        except Exception:
            pass
        raise DbError(" ".join(str(a) for a in exc.args).strip() or str(exc))
    finally:
        _release(conn, db)


class transaction:
    """`with transaction() as cur:` - several statements, ONE commit.

    This is what the script `Tx*` builtins need: they change a property, take
    items and give items, and all of it must land or none of it.
    """
    def __init__(self, db=DB):
        self.db = db
        self.conn = None
        self.cursor = None

    def __enter__(self):
        self.conn = _acquire(self.db)
        self.cursor = self.conn.cursor()
        return self.cursor

    def __exit__(self, exc_type, exc, tb):
        try:
            if exc_type is None:
                self.conn.commit()
            else:
                self.conn.rollback()
        except Exception:
            pass
        finally:
            _release(self.conn, self.db)
        return False


def get_account(member_id):
    """Return dict(account_id, commander_id, member_id) for a login id, or None."""
    rows = _run("SELECT ACCOUNT_ID, COMMANDER_ID, MEMBER_ID FROM ACCOUNT_INFO "
                "WHERE MEMBER_ID='%s'" % member_id.replace("'", "''"))
    if not rows:
        return None
    a = rows[0]
    return {"account_id": int(a[0]), "commander_id": int(a[1]), "member_id": a[2]}


def get_commander_by_account(account_id):
    """The commander id that owns an account id, or None.

    The zone gets the account id in CZ_START_GAME, so this is how a zone
    knows WHICH player just arrived - the original did the same thing through
    the SLService (its login/auth service) and the RESOURCE database.
    """
    try:
        account_id = int(account_id)
    except (TypeError, ValueError):
        return None
    rows = _run("SELECT COMMANDER_ID FROM ACCOUNT_INFO WHERE ACCOUNT_ID=%d"
                % account_id)
    if not rows:
        return None
    return int(rows[0][0])


def get_commander(commander_id):
    rows = _run("SELECT COMMANDER_ID, ACCOUNT_ID, MEMBER_ID, NAME, RANK, DUTY, "
                "FAME, KARMA, POINT, LADDER, ZONE_ID, CHANNEL_ID "
                "FROM COMMANDER WHERE COMMANDER_ID=%d" % commander_id)
    if not rows:
        return None
    r = rows[0]
    keys = ["commander_id", "account_id", "member_id", "name", "rank", "duty",
            "fame", "karma", "point", "ladder", "zone_id", "channel_id"]
    return {k: (int(v) if k != "name" and k != "member_id" else v)
            for k, v in zip(keys, r)}


def get_characters(commander_id):
    rows = _run("SELECT CHARACTER_ID, SLOT, NAME, TEAMPOS, GENDER, JOB, HEADTYPE, "
                "EXP, LIFE, HP, SP, XPOS, YPOS, ZPOS "
                "FROM CHARACTERS WHERE COMMANDER_ID=%d ORDER BY SLOT" % commander_id)
    keys = ["character_id", "slot", "name", "teampos", "gender", "job", "headtype",
            "exp", "life", "hp", "sp", "xpos", "ypos", "zpos"]
    out = []
    for r in rows:
        d = {}
        for i, (k, v) in enumerate(zip(keys, r)):
            d[k] = r[2] if k == "name" else int(v)
        d["equip"] = {}
        d["reinforce"] = {}
        out.append(d)
    by_id = {d["character_id"]: d for d in out}
    if by_id:
        ids = ",".join(str(i) for i in by_id)
        # CLASS_ID comes from ITEM; HIT_COUNT is the per-item reinforce (the "+N"
        # the original server puts in PC_INFO's equipReinforceCnt[] via
        # CCharacterSlot::MakeBCPacket -> GetReinforceCount).
        wrows = _run("SELECT w.CHARACTER_ID, w.SPOT, i.CLASS_ID, r.HIT_COUNT "
                     "FROM WEARITEM w "
                     "LEFT JOIN ITEM i ON i.ITEM_ID=w.ITEM_ID "
                     "LEFT JOIN ITEM_REINFORCE r ON r.ITEM_ID=w.ITEM_ID "
                     "WHERE w.CHARACTER_ID IN (%s)" % ids)
        for w in wrows:
            cid, spot = int(w[0]), int(w[1])
            cls = _num(w[2])
            rn = _num(w[3])
            if cid in by_id:
                by_id[cid]["equip"][spot] = cls
                by_id[cid]["reinforce"][spot] = rn
    return out


def get_stance_levels():
    """{xp_type: [(level, min_exp), ...]} sorted - geStanceLevelTable."""
    rows = _run("SELECT STANCE_XP_TYPE, STANCE_LEVEL, MIN_EXP FROM STANCE_LEVELS")
    out = {}
    for r in rows:
        xp, lv, mn = _num(r[0]), _num(r[1]), _num(r[2])
        if xp:
            out.setdefault(xp, []).append((lv, mn))
    for bands in out.values():
        bands.sort()
    return out


def get_stance_xp_types():
    """{stance id: xp type} - which EXP curve a stance uses."""
    rows = _run("SELECT STANCE_ID, STANCE_XP_TYPE FROM STANCE_XP_TYPE")
    return {_num(r[0]): _num(r[1]) for r in rows if _num(r[0])}


def get_character_stances(commander_id):
    """{character_id: {stance_id: (exp, skill_point)}} from the STANCE table."""
    rows = _run("SELECT s.CHARACTER_ID, s.STANCE_ID, s.EXP, s.SKILL_POINT "
                "FROM STANCE s JOIN CHARACTERS c ON c.CHARACTER_ID=s.CHARACTER_ID "
                "WHERE c.COMMANDER_ID=%d" % int(commander_id))
    out = {}
    for r in rows:
        cid, sid = _num(r[0]), _num(r[1])
        if cid and sid:
            out.setdefault(cid, {})[sid] = (_num(r[2]), _num(r[3]))
    return out


def get_character_skills(commander_id):
    """{character_id: {skill_id: exp}} from the SKILL table."""
    rows = _run("SELECT s.CHARACTER_ID, s.SKILL_ID, s.EXP "
                "FROM SKILL s JOIN CHARACTERS c ON c.CHARACTER_ID=s.CHARACTER_ID "
                "WHERE c.COMMANDER_ID=%d" % int(commander_id))
    out = {}
    for r in rows:
        cid, sid = _num(r[0]), _num(r[1])
        if cid and sid:
            out.setdefault(cid, {})[sid] = _num(r[2])
    return out


def set_team_positions(commander_id, slots):
    """Persist a barrack team: picked slots get TEAMPOS 1,2,3; others 0.

    The barrack UI rebuilds the team from these bytes on every login, so this is
    what makes "I picked Grace" survive a logout / Move to Barrack. Returns the
    resulting {slot: teampos} so the caller can log what was saved.
    """
    order = [int(s) for s in slots if s is not None and int(s) != 255][:3]
    if order:
        cases = " ".join("WHEN %d THEN %d" % (slot, i + 1)
                         for i, slot in enumerate(order))
        sql = ("UPDATE CHARACTERS SET TEAMPOS = CASE SLOT %s ELSE 0 END "
               "WHERE COMMANDER_ID=%d" % (cases, int(commander_id)))
    else:
        sql = ("UPDATE CHARACTERS SET TEAMPOS = 0 WHERE COMMANDER_ID=%d"
               % int(commander_id))
    _run(sql)
    return {slot: i + 1 for i, slot in enumerate(order)}


_ITEM_CHILD_TABLES = ("ITEM_OPTION_2", "ITEM_PROPERTIES", "ITEM_REINFORCE",
                      "ITEM_SOCKET", "ITEM_RECOVER", "ITEM_CASH")


def apply_purchase(commander_id, vis_item_id, cost, item_class, count,
                   stack_limit=1):
    """Pay `cost` vis and add `count` of item_class, in ONE transaction.

    Mirrors CTxBuyItem: the price comes from the item row (SHOP_VIS) and is paid
    from the vis STACK (ITEM class 30000) that lives in STORAGE. Stack onto an
    existing bag row when it fits, otherwise insert a fresh ITEM + STORAGE pair
    with ITEM_ID = MAX+1. Returns (ok, message); nothing is written on failure.
    """
    rows = _run("SELECT i.COUNT FROM STORAGE s JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.ITEM_ID=%d AND s.TRASH=0"
                % (int(commander_id), int(vis_item_id)))
    have = _num(rows[0][0]) if rows else 0
    if have < cost:
        return False, "not enough vis (%d < %d)" % (have, cost), 0, 0

    stack = _run("SELECT TOP 1 s.ITEM_ID, i.COUNT FROM STORAGE s "
                 "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                 "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0 "
                 "AND i.CLASS_ID=%d AND i.COUNT + %d <= %d "
                 "ORDER BY s.INSERT_DATE"
                 % (int(commander_id), int(item_class), int(count),
                    max(int(stack_limit), int(count))))
    sql = ["SET XACT_ABORT ON", "BEGIN TRAN",
           "UPDATE ITEM SET COUNT = COUNT - %d WHERE ITEM_ID=%d"
           % (int(cost), int(vis_item_id))]
    if stack:
        new_id = int(stack[0][0])
        new_count = _num(stack[0][1]) + int(count)
        sql.append("UPDATE ITEM SET COUNT = COUNT + %d WHERE ITEM_ID=%d"
                   % (int(count), new_id))
        how = "stacked onto ITEM_ID=%d" % new_id
    else:
        maxrow = _run("SELECT ISNULL(MAX(ITEM_ID), 0) FROM ITEM")
        new_id = _num(maxrow[0][0]) + 1 if maxrow else 1
        new_count = int(count)
        sql += [
            "INSERT INTO ITEM (ITEM_ID, CLASS_ID, COUNT, CREATE_DATE, FIRST_DATE, "
            "TYPE) VALUES (%d, %d, %d, GETDATE(), GETDATE(), 0)"
            % (new_id, int(item_class), int(count)),
            "INSERT INTO STORAGE (COMMANDER_ID, ITEM_ID, TRASH, TRASH_COUNT, TYPE, "
            "INSERT_DATE) VALUES (%d, %d, 0, 0, 0, GETDATE())"
            % (int(commander_id), new_id),
        ]
        how = "inserted a new row"
    sql.append("COMMIT")
    _run("; ".join(sql))
    return True, how, new_id, new_count


def apply_sale(commander_id, vis_item_id, item_id, count, gain):
    """Sell `count` of item_id and add `gain` vis, in ONE transaction.

    The reverse of apply_purchase: the price is the item's own SellPrice
    (CInvItem::GetSellPrice), the bag row is decremented, and the row is deleted
    (with its option/reinforce/socket/property children) once it hits zero.
    Returns (ok, message); nothing is written on failure.
    """
    rows = _run("SELECT i.CLASS_ID, i.COUNT FROM STORAGE s "
                "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.ITEM_ID=%d AND s.TYPE=0 "
                "AND s.TRASH=0" % (int(commander_id), int(item_id)))
    if not rows:
        return False, "item %d is not in the bag" % int(item_id)
    item_class, have = _num(rows[0][0]), _num(rows[0][1])
    if count <= 0 or have < count:
        return False, "count %d > bag %d" % (int(count), have)

    vis = _run("SELECT i.COUNT FROM STORAGE s JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
               "WHERE s.COMMANDER_ID=%d AND s.ITEM_ID=%d AND s.TRASH=0"
               % (int(commander_id), int(vis_item_id)))
    if not vis:
        return False, "no vis stack (ITEM_ID=%d)" % int(vis_item_id)

    remain = have - count
    sql = ["SET XACT_ABORT ON", "BEGIN TRAN",
           "UPDATE ITEM SET COUNT = COUNT + %d WHERE ITEM_ID=%d"
           % (int(gain), int(vis_item_id))]
    if remain > 0:
        sql.append("UPDATE ITEM SET COUNT = COUNT - %d WHERE ITEM_ID=%d"
                   % (int(count), int(item_id)))
        how = "decremented, %d left" % remain
    else:
        # The last one went: drop the item and everything hanging off it, so no
        # orphan rows are left for the next ITEM_ID scan to trip over.
        for table in _ITEM_CHILD_TABLES:
            sql.append("DELETE FROM %s WHERE ITEM_ID=%d"
                       % (table, int(item_id)))
        sql.append("DELETE FROM STORAGE WHERE ITEM_ID=%d AND COMMANDER_ID=%d"
                   % (int(item_id), int(commander_id)))
        sql.append("DELETE FROM ITEM WHERE ITEM_ID=%d" % int(item_id))
        how = "removed the row (class %d)" % item_class
    sql.append("COMMIT")
    _run("; ".join(sql))
    return True, how


def consume_one(commander_id, item_id, count=1):
    """Use `count` of item_id: decrement the stack, delete the row at zero.

    Same shape as apply_sale without the vis credit. Returns
    (ok, message, item_class, remaining_count). The caller must have already
    checked that the item really is a usable item - this only does the
    bookkeeping.
    """
    rows = _run("SELECT i.CLASS_ID, i.COUNT FROM STORAGE s "
                "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.ITEM_ID=%d AND s.TYPE=0 "
                "AND s.TRASH=0" % (int(commander_id), int(item_id)))
    if not rows:
        return False, "item %d is not in the bag" % int(item_id), 0, 0
    item_class, have = _num(rows[0][0]), _num(rows[0][1])
    if count <= 0 or have < count:
        return False, "count %d > bag %d" % (int(count), have), item_class, have
    remain = have - count
    sql = ["SET XACT_ABORT ON", "BEGIN TRAN"]
    if remain > 0:
        sql.append("UPDATE ITEM SET COUNT = COUNT - %d WHERE ITEM_ID=%d"
                   % (int(count), int(item_id)))
        how = "%d left" % remain
    else:
        for table in _ITEM_CHILD_TABLES:
            sql.append("DELETE FROM %s WHERE ITEM_ID=%d"
                       % (table, int(item_id)))
        sql.append("DELETE FROM STORAGE WHERE ITEM_ID=%d AND COMMANDER_ID=%d"
                   % (int(item_id), int(commander_id)))
        sql.append("DELETE FROM ITEM WHERE ITEM_ID=%d" % int(item_id))
        how = "used up (class %d)" % item_class
    sql.append("COMMIT")
    _run("; ".join(sql))
    return True, how, item_class, remain


def get_character_row(character_id):
    """The few columns the stat/HP maths needs for one character."""
    rows = _run("SELECT HP, EXP, JOB, LIFE, SP FROM CHARACTERS WHERE CHARACTER_ID=%d"
                % int(character_id))
    if not rows:
        return None
    r = rows[0]
    return {"hp": _num(r[0]), "exp": _num(r[1]), "job": _num(r[2]),
            "life": _num(r[3]), "sp": _num(r[4])}


def ensure_position_table():
    """Create our own table for a character's last known position.

    Deliberately NOT the shipped `CHARACTERS` columns: `CHARACTERS.ZPOS` holds
    a z-HEIGHT (the shipped data has values like -1217 and 255), while the
    wire speaks in navmesh CELLS. Storing a cell in a height column is exactly
    the kind of "right type, wrong meaning" mix-up that breaks later - so the
    cell gets its own column, in a table we own.
    """
    try:
        _run("IF OBJECT_ID('CHARACTER_POSITION','U') IS NULL "
             "CREATE TABLE CHARACTER_POSITION ("
             "CHARACTER_ID int PRIMARY KEY, XPOS int, YPOS int, "
             "ZPOS int, MAP_ID int, UPDATED datetime)")
    except Exception:
        pass


def set_character_position(character_id, x, y, cell, map_id=0):
    """Save where a character is standing, so CONTINUEGAME can resume there.

    The original server keeps the character's point in the character slot
    (`m_continueZonePos`, set from `character->point.x/y/z` when the slot is
    loaded - source2\\geServer\\BarrackServer\\CharacterSlot.cpp:126) and
    resumes from it on CB_CONTINUEGAME. Ours has to write it back, or
    "continue" always resumes at a stale position.

    `cell` is the navmesh cell the CLIENT sent in CZ_MOVE_PC - the wire's own
    coordinate, so no reverse-lookup is needed. `map_id` lets a continue
    resume in the map the character was last seen in.
    """
    ensure_position_table()
    _run("DELETE FROM CHARACTER_POSITION WHERE CHARACTER_ID=%d"
         % int(character_id))
    _run("INSERT INTO CHARACTER_POSITION "
         "(CHARACTER_ID, XPOS, YPOS, ZPOS, MAP_ID, UPDATED) "
         "VALUES (%d, %d, %d, %d, %d, GETDATE())"
         % (int(character_id), int(x), int(y), int(cell), int(map_id)))


def get_character_position(character_id):
    """The character's last saved position, or None.

    Returns (x, y, cell, map_id) - the cell is the wire's own coordinate, so
    it is used as-is.
    """
    ensure_position_table()
    rows = _run("SELECT XPOS, YPOS, ZPOS, MAP_ID FROM CHARACTER_POSITION "
                "WHERE CHARACTER_ID=%d" % int(character_id))
    if not rows:
        return None
    try:
        return (int(rows[0][0]), int(rows[0][1]), int(rows[0][2]), int(rows[0][3]))
    except (ValueError, IndexError):
        return None


def set_character_hp(character_id, hp):
    """Persist current HP (CHARACTERS.HP is CURRENT HP, not max)."""
    _run("UPDATE CHARACTERS SET HP=%d WHERE CHARACTER_ID=%d"
         % (int(hp), int(character_id)))


def _bag_stacks(commander_id, item_class):
    """[(ITEM_ID, COUNT)] of one class in the bag, oldest first."""
    rows = _run("SELECT s.ITEM_ID, i.COUNT FROM STORAGE s "
                "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0 "
                "AND i.CLASS_ID=%d ORDER BY s.INSERT_DATE"
                % (int(commander_id), int(item_class)))
    return [(int(r[0]), _num(r[1])) for r in rows]


def apply_custom_purchase(commander_id, buys, costs):
    """Barter at a custom shop: pay `costs` and receive `buys`, one transaction.

    Mirrors CTxBuyCustomItem::Init: the price is the shop row's own
    (pricetype, count) pair, NOT a vis amount, so there is nothing to price from
    the item table. `buys` = [(class_id, count, max_stack)], `costs` =
    [(class_id, count)]. Every cost is checked first; nothing is written unless
    all of them are affordable. Cost items are consumed oldest stack first, and
    their row (with option/reinforce/socket children) is deleted when emptied.
    """
    buys = [(int(c), int(n), int(m) or 1) for c, n, m in buys if int(n) > 0]
    costs = [(int(c), int(n)) for c, n in costs if int(n) > 0]
    if not buys:
        return False, "nothing to buy", []
    plan = []
    for cls, need in costs:
        stacks = _bag_stacks(commander_id, cls)
        have = sum(n for _i, n in stacks)
        if have < need:
            return False, "not enough %d (%d < %d)" % (cls, have, need), []
        take, left = [], need
        for item_id, n in stacks:
            if left <= 0:
                break
            got = min(n, left)
            take.append((item_id, got, n))
            left -= got
        plan.append(take)

    next_id = None
    sql = ["SET XACT_ABORT ON", "BEGIN TRAN"]
    for take in plan:
        for item_id, got, n in take:
            if got >= n:
                for table in _ITEM_CHILD_TABLES:
                    sql.append("DELETE FROM %s WHERE ITEM_ID=%d" % (table, item_id))
                sql.append("DELETE FROM STORAGE WHERE ITEM_ID=%d "
                           "AND COMMANDER_ID=%d" % (item_id, int(commander_id)))
                sql.append("DELETE FROM ITEM WHERE ITEM_ID=%d" % item_id)
            else:
                sql.append("UPDATE ITEM SET COUNT = COUNT - %d WHERE ITEM_ID=%d"
                           % (got, item_id))
    how = []
    added = []
    for cls, count, limit in buys:
        stack = _run("SELECT TOP 1 s.ITEM_ID, i.COUNT FROM STORAGE s "
                     "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                     "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0 "
                     "AND i.CLASS_ID=%d AND i.COUNT + %d <= %d "
                     "ORDER BY s.INSERT_DATE"
                     % (int(commander_id), cls, count, max(limit, count)))
        if stack:
            new_id = int(stack[0][0])
            new_count = _num(stack[0][1]) + count
            sql.append("UPDATE ITEM SET COUNT = COUNT + %d WHERE ITEM_ID=%d"
                       % (count, new_id))
            how.append("%d stacked onto ITEM_ID=%d" % (cls, new_id))
        else:
            if next_id is None:
                maxrow = _run("SELECT ISNULL(MAX(ITEM_ID), 0) FROM ITEM")
                next_id = _num(maxrow[0][0]) + 1 if maxrow else 1
            new_id = next_id
            new_count = count
            next_id += 1
            sql += [
                "INSERT INTO ITEM (ITEM_ID, CLASS_ID, COUNT, CREATE_DATE, "
                "FIRST_DATE, TYPE) VALUES (%d, %d, %d, GETDATE(), GETDATE(), 0)"
                % (new_id, cls, count),
                "INSERT INTO STORAGE (COMMANDER_ID, ITEM_ID, TRASH, TRASH_COUNT, "
                "TYPE, INSERT_DATE) VALUES (%d, %d, 0, 0, 0, GETDATE())"
                % (int(commander_id), new_id),
            ]
            how.append("%d inserted" % cls)
        added.append((new_id, cls, new_count))
    sql.append("COMMIT")
    _run("; ".join(sql))
    return True, ", ".join(how), added


def _num(v):
    """sqlcmd renders SQL NULL as the text 'NULL' - treat it as 0/absent."""
    if v is None or v == "" or str(v).strip().upper() in ("NULL", "NONE"):
        return 0
    try:
        return int(v)
    except (TypeError, ValueError):
        return 0


def get_character_promotion(commander_id):
    """{character_id: (promotion_step, cash_promotion_step)} from
    CHARACTERS_PROPERTY.

    The original barrack reads these per-character properties with
    S_GET_CHARACTERS_PROPERTY_2 (BarrackServer/PCLoader.cpp ->
    CPCPropLoader) and folds them into PC_INFO's promotionLv / cashPromotionLv.
    A character with no such row gets 0, which is also what the original sends.
    """
    rows = _run("SELECT c.CHARACTER_ID, p.PROPERTY, p.VALUE "
                "FROM CHARACTERS_PROPERTY p "
                "JOIN CHARACTERS c ON c.CHARACTER_ID=p.CHARACTER_ID "
                "WHERE c.COMMANDER_ID=%d AND p.PROPERTY IN "
                "('PromotionStep','CashPromotionStep')" % commander_id)
    out = {}
    for cid, prop, value in rows:
        try:
            v = int(value)
        except (TypeError, ValueError):
            v = 0
        pair = out.setdefault(int(cid), [0, 0])
        if prop == "PromotionStep":
            pair[0] = v
        else:
            pair[1] = v
    return {k: tuple(v) for k, v in out.items()}


def get_password(member_id):
    """MD5 hex of the account password, from RESOURCE.ACCOUNT_INFO, or None."""
    rows = _run("SELECT PASS FROM ACCOUNT_INFO WHERE MEMBER_ID='%s'"
                % member_id.replace("'", "''"), db="RESOURCE")
    return rows[0][0] if rows else None


def _esc(s):
    """Escape a value for single-quoted SQL."""
    return str(s).replace("'", "''")


# --------------------------------------------------------------------------
# writes (so the client's actions persist)
# --------------------------------------------------------------------------
def count_characters(commander_id):
    rows = _run("SELECT COUNT(*) FROM CHARACTERS WHERE COMMANDER_ID=%d" % commander_id)
    return int(rows[0][0]) if rows else 0


def slot_used(commander_id, slot):
    rows = _run("SELECT COUNT(*) FROM CHARACTERS WHERE COMMANDER_ID=%d AND SLOT=%d"
                % (commander_id, slot))
    return bool(rows and int(rows[0][0]))


def create_character(commander_id, slot, name, gender, job, headtype,
                     life=1, exp=0, hp=100, sp=0, pos=(0, 0, 0), iscard=0):
    """INSERT a character. CHARACTER_ID is an IDENTITY so we read it back with
    SCOPE_IDENTITY() (both statements go in one sqlcmd call). Returns the new id."""
    sql = ("INSERT INTO CHARACTERS (COMMANDER_ID, SLOT, NAME, TEAMPOS, GENDER, JOB, "
           "HEADTYPE, EXP, LIFE, HP, SP, XPOS, YPOS, ZPOS, ISCARD, create_date) "
           "VALUES (%d, %d, '%s', 0, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, %d, "
           "GETDATE()); SELECT CAST(SCOPE_IDENTITY() AS int);"
           % (commander_id, slot, _esc(name), 1 if gender else 0, job, headtype,
              exp, life, hp, sp, pos[0], pos[1], pos[2], iscard))
    rows = _run(sql)
    return int(rows[0][0]) if rows else None


def delete_character(commander_id, slot):
    """Delete a character and its worn items. Returns the number of CHARACTERS
    rows removed."""
    sql = ("DELETE FROM WEARITEM WHERE CHARACTER_ID IN "
           "(SELECT CHARACTER_ID FROM CHARACTERS WHERE COMMANDER_ID=%d AND SLOT=%d); "
           "DELETE FROM CHARACTERS WHERE COMMANDER_ID=%d AND SLOT=%d; "
           "SELECT CAST(@@ROWCOUNT AS int);"
           % (commander_id, slot, commander_id, slot))
    rows = _run(sql)
    return int(rows[0][0]) if rows else 0


def get_inventory(commander_id, exclude=None):
    """Live bag rows: STORAGE TYPE=0 (INVENTORY_ORIGINAL), TRASH=0, joined to
    ITEM for the class id + stack count, oldest first. Returns a list of
    {type, count}. The wire InvIndex is runtime-assigned (the server numbers
    items as it loads them), so the caller numbers them 1..N in this order.
    `exclude` = a set of CLASS_IDs to skip (crash bisect)."""
    rows = _run("SELECT i.CLASS_ID, i.COUNT FROM STORAGE s "
                "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0 "
                "AND i.CLASS_ID IS NOT NULL AND i.CLASS_ID<>0 "
                "ORDER BY s.INSERT_DATE" % commander_id)
    out = []
    for r in rows:
        try:
            t, c = int(r[0]), int(r[1])
        except (ValueError, TypeError, IndexError):
            continue
        if exclude and t in exclude:
            continue
        out.append({"type": t, "count": c})
    return out


def get_inventory_full(commander_id):
    """Same rows as get_inventory plus ITEM_ID, so options/reinforce rows can
    be matched to the wire invIndex (1..N in this order)."""
    rows = _run("SELECT s.ITEM_ID, i.CLASS_ID, i.COUNT FROM STORAGE s "
                "JOIN ITEM i ON i.ITEM_ID=s.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0 "
                "AND i.CLASS_ID IS NOT NULL AND i.CLASS_ID<>0 "
                "ORDER BY s.INSERT_DATE" % commander_id)
    out = []
    for r in rows:
        try:
            out.append({"item_id": int(r[0]), "type": int(r[1]),
                        "count": int(r[2])})
        except (ValueError, TypeError, IndexError):
            continue
    return out


def get_item_options(commander_id):
    """(ITEM_ID, option CLASS_ID, VALUE) for the commander's bag items."""
    rows = _run("SELECT o.ITEM_ID, o.CLASS_ID, o.VALUE FROM ITEM_OPTION_2 o "
                "JOIN STORAGE s ON s.ITEM_ID=o.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0" % commander_id)
    out = []
    for r in rows:
        try:
            out.append({"item_id": int(r[0]), "type": int(r[1]),
                        "value": int(r[2])})
        except (ValueError, TypeError, IndexError):
            continue
    return out


def get_char_equip_options(character_id):
    """The equip options of a character's WORN items: (type, value, spot).

    The zone needs them for the spawn packet, where the client reads the
    equip options of the other player (the count and the records).
    """
    rows = _run("SELECT o.CLASS_ID, o.VALUE, w.SPOT FROM ITEM_OPTION_2 o "
                "JOIN WEARITEM w ON w.ITEM_ID = o.ITEM_ID "
                "WHERE w.CHARACTER_ID=%d" % character_id)
    out = []
    for r in rows:
        try:
            out.append({"type": int(r[0]), "value": int(r[1]), "spot": int(r[2])})
        except (ValueError, TypeError, IndexError):
            continue
    return out


def get_char_worn_items(character_ids):
    """{character_id: [(spot, item_id)]} for the given characters.

    The barrack needs the WORN items of every character, because the
    properties (the enchant values, the icons) are sent per item, not per
    character.
    """
    if not character_ids:
        return {}
    ids = ",".join(str(int(i)) for i in character_ids)
    rows = _run("SELECT CHARACTER_ID, SPOT, ITEM_ID FROM WEARITEM "
                "WHERE CHARACTER_ID IN (%s) ORDER BY CHARACTER_ID, SPOT" % ids)
    out = {}
    for r in rows:
        try:
            out.setdefault(int(r[0]), []).append((int(r[1]), int(r[2])))
        except (ValueError, TypeError, IndexError):
            continue
    return out


def get_item_properties_of(item_ids):
    """{item_id: [{property, value}]} for the given items.

    The same rows as `get_item_properties`, but keyed by ITEM_ID so a caller
    can pick the properties of one specific item.
    """
    item_ids = [int(i) for i in item_ids]
    if not item_ids:
        return {}
    out = {}
    # SQL Server has a 2100-parameter limit, so chunk the IN list.
    for i in range(0, len(item_ids), 500):
        chunk = item_ids[i:i + 500]
        rows = _run("SELECT ITEM_ID, PROPERTY, VALUE FROM ITEM_PROPERTIES "
                    "WHERE ITEM_ID IN (%s)"
                    % ",".join(str(x) for x in chunk))
        for r in rows:
            try:
                out.setdefault(int(r[0]), []).append(
                    {"property": r[1], "value": r[2]})
            except (ValueError, TypeError, IndexError):
                continue
    return out


def get_item_properties(commander_id):
    """(ITEM_ID, PROPERTY name, VALUE text) for the commander's bag items.

    The table is `ITEM_PROPERTIES` (ITEM_ID bigint, PROPERTY varchar, VALUE
    varchar) - 3,224 rows in GE_DATA, one row per named property. The original
    server reads exactly these three fields (v28
    `CCommanderLoader::LoadInventoryItemProperty`: int serial, 64-char name,
    128-char value) and appends a 48-byte record per row.

    Values are TEXT here; whether the wire wants a float or a string is decided
    by the client's getter for that key, not by the column - see
    `zone_server.item_property_type`.
    """
    rows = _run("SELECT p.ITEM_ID, p.PROPERTY, p.VALUE FROM ITEM_PROPERTIES p "
                "JOIN STORAGE s ON s.ITEM_ID=p.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0" % commander_id)
    out = []
    for r in rows:
        try:
            out.append({"item_id": int(r[0]),
                        "property": (r[1] or "").strip(),
                        "value": r[2] if r[2] is not None else ""})
        except (ValueError, TypeError, IndexError):
            continue
    return out


def get_item_reinforces(commander_id):
    """(ITEM_ID, accumCount=HIT_COUNT) for bag items with reinforces."""
    rows = _run("SELECT r.ITEM_ID, r.HIT_COUNT FROM ITEM_REINFORCE r "
                "JOIN STORAGE s ON s.ITEM_ID=r.ITEM_ID "
                "WHERE s.COMMANDER_ID=%d AND s.TYPE=0 AND s.TRASH=0 "
                "AND r.HIT_COUNT>0" % commander_id)
    out = []
    for r in rows:
        try:
            out.append({"item_id": int(r[0]), "count": int(r[1])})
        except (ValueError, TypeError, IndexError):
            continue
    return out


def get_barrack_names(commander_id):
    """{channel(0-based): name} saved in COMMANDER_BARRACK_NAME."""
    rows = _run("SELECT CHANNEL, NAME FROM COMMANDER_BARRACK_NAME "
                "WHERE COMMANDER_ID=%d" % commander_id)
    return {int(r[0]): r[1] for r in rows}


def set_barrack_name(commander_id, channel, name):
    """Upsert one barrack channel name."""
    _run("MERGE dbo.COMMANDER_BARRACK_NAME AS t "
         "USING (SELECT %d AS COMMANDER_ID, %d AS CHANNEL) AS s "
         "ON t.COMMANDER_ID=s.COMMANDER_ID AND t.CHANNEL=s.CHANNEL "
         "WHEN MATCHED THEN UPDATE SET NAME='%s', UPDATED=GETDATE() "
         "WHEN NOT MATCHED THEN INSERT (COMMANDER_ID, CHANNEL, NAME, UPDATED) "
         "VALUES (%d, %d, '%s', GETDATE());"
         % (commander_id, channel, _esc(name), commander_id, channel, _esc(name)))


def get_commander_quest_states(commander_id):
    """{quest ClassName: state int} for a commander, from COMMANDER_PROPERTY.

    Quest states live in the commander's IES object: each quest class
    (e.g. 'Quest_00001') is a property whose value is the state byte that
    ZC_QUEST_INFO sends (1 in progress, 100 done, 0xC8 not reached, ...).
    The row is stored by (SECTION, PROPERTY_IDX); MASTER_PROPERTY maps the
    idx to its name. Only names the caller cares about need filtering.
    """
    try:
        commander_id = int(commander_id)
    except (TypeError, ValueError):
        return {}
    try:
        rows = _run("SELECT mp.PROPERTY, cp.VALUE "
                    "FROM COMMANDER_PROPERTY cp "
                    "JOIN MASTER_PROPERTY mp ON mp.IDX = cp.PROPERTY_IDX "
                    "WHERE cp.COMMANDER_ID=%d" % commander_id)
    except Exception:
        return {}
    out = {}
    for r in rows:
        if len(r) < 2:
            continue
        name = (r[0] or "").strip()
        if not name or name.upper() == "NULL":
            continue
        val = (r[1] or "").strip()
        if val.upper() == "NULL" or val == "":
            continue
        try:
            out[name] = int(val, 0)
        except (TypeError, ValueError):
            try:
                out[name] = int(val)
            except (TypeError, ValueError):
                pass
    return out


def set_commander_quest_state(commander_id, name, state):
    """Set one quest's state byte for a commander (IES property).

    `name` is the quest ClassName (e.g. 'Quest_00001'). The property must
    already exist in MASTER_PROPERTY; otherwise there is nothing to set and
    False is returned. Returns True on success.
    """
    try:
        commander_id = int(commander_id)
        state = int(state)
    except (TypeError, ValueError):
        return False
    name = (name or "").strip()
    if not name:
        return False
    try:
        rows = _run("SELECT IDX FROM MASTER_PROPERTY WHERE PROPERTY='%s'"
                    % name.replace("'", "''"))
    except Exception:
        return False
    if not rows:
        return False
    try:
        idx = int(rows[0][0])
    except (TypeError, ValueError):
        return False
    try:
        _run("DELETE FROM COMMANDER_PROPERTY "
             "WHERE COMMANDER_ID=%d AND PROPERTY_IDX=%d" % (commander_id, idx))
        _run("INSERT INTO COMMANDER_PROPERTY "
             "(COMMANDER_ID, SECTION, VALUE, PROPERTY_IDX, PROPERTY, UPDATE_DATE) "
             "VALUES (%d, 1, '%d', %d, NULL, GETDATE())"
             % (commander_id, state, idx))
    except Exception:
        return False
    return True


def get_quest_state_rows(commander_id):
    """[(name, stage, object, count)] kill-count objectives for a commander.

    From the QUEST_STATE table: one row per (quest, stage, monster) with the
    current kill count. Used to build ZC_QUEST_STAGE_INFO.
    """
    try:
        commander_id = int(commander_id)
    except (TypeError, ValueError):
        return []
    try:
        rows = _run("SELECT NAME, STAGE, OBJECT, COUNT FROM QUEST_STATE "
                    "WHERE COMMANDER_ID=%d" % commander_id)
    except Exception:
        return []
    out = []
    for r in rows:
        if len(r) < 4:
            continue
        try:
            out.append((str(r[0]).strip(), int(r[1]), str(r[2]).strip(),
                       int(r[3])))
        except (TypeError, ValueError):
            continue
    return out


if __name__ == "__main__":
    acc = get_account("arcanine")
    print("account:", acc)
    if acc:
        print("commander:", get_commander(acc["commander_id"]))
        chars = get_characters(acc["commander_id"])
        print("characters:", len(chars))
        for c in chars[:5]:
            print("  ", c)
    print("password md5:", get_password("arcanine"))
