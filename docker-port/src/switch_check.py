"""switch_check - verify ge_db now talks to the Docker container, create the
barrack-name table there, and prove the error hardening works."""
import ge_db

SQL_CREATE = (
    "IF OBJECT_ID('dbo.COMMANDER_BARRACK_NAME') IS NULL "
    "CREATE TABLE dbo.COMMANDER_BARRACK_NAME ("
    "  COMMANDER_ID int NOT NULL,"
    "  CHANNEL int NOT NULL,"
    "  NAME varchar(20) NOT NULL,"
    "  UPDATED datetime NULL,"
    "  CONSTRAINT PK_COMMANDER_BARRACK_NAME PRIMARY KEY CLUSTERED "
    "    (COMMANDER_ID, CHANNEL));"
)


def main():
    srv = ge_db._run("SELECT @@SERVERNAME")[0][0]
    print("connected to   :", ge_db.SERVER, "user=", ge_db.USER)
    print("@@SERVERNAME   :", srv)
    assert srv != "DESKTOP-KHAIRIL", "STILL pointed at the HOST database!"
    print("  -> good, that is the container, not DESKTOP-KHAIRIL")
    print()

    print("creating COMMANDER_BARRACK_NAME in the container ...")
    ge_db._run(SQL_CREATE)
    print("  done")
    print()

    print("error hardening: a bad query must RAISE, not return rows")
    try:
        ge_db._run("SELECT * FROM NO_SUCH_TABLE_XYZ")
        print("  !! FAILED - it did not raise")
    except ge_db.DbError as e:
        print("  ok, raised DbError:", str(e)[:70])
    print()

    acc = ge_db.get_account("arcanine")
    cid = acc["commander_id"]
    print("account   :", acc)
    print("commander :", ge_db.get_commander(cid)["name"])
    print("characters:", ge_db.count_characters(cid))
    print("names     :", ge_db.get_barrack_names(cid), "(expect empty)")
    print()

    print("round-trip the barrack-name table IN THE CONTAINER")
    ge_db.set_barrack_name(cid, 0, "Docker Test")
    print("  after write:", ge_db.get_barrack_names(cid))
    ge_db._run("DELETE FROM COMMANDER_BARRACK_NAME WHERE COMMANDER_ID=%d" % cid)
    print("  after clean:", ge_db.get_barrack_names(cid))


if __name__ == "__main__":
    main()
