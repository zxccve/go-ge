"""verify_now - end-to-end check of the barrack writes.

Reads through the SAME code path the server uses (ge_db + build_pc_info), so a
pass here means a real client login will see the same thing.
"""
import struct

import ge_barrack_server as g
import ge_db


def main():
    srv = ge_db._run("SELECT @@SERVERNAME")[0][0]
    print("database      :", ge_db.SERVER, "->", srv,
          "(container)" if srv != "DESKTOP-KHAIRIL" else "(!! HOST, WRONG)")

    acc = ge_db.get_account("arcanine")
    cid = acc["commander_id"]
    chars = ge_db.get_characters(cid)
    print("commander     :", ge_db.get_commander(cid)["name"], "id", cid)
    print("characters    :", len(chars))
    print("barrack names :", ge_db.get_barrack_names(cid))
    print()

    # Decode the PC_INFO the server would send for every character, using the
    # client's real offsets. This proves what the client will actually receive.
    print("decoded BARRACK_INFO payload (what the client gets on login):")
    print("  slot  name        job  lv   head  equip(class ids)")
    for c in chars:
        b = g.build_pc_info(c)
        slot, = struct.unpack_from("<H", b, 0)
        name = b[3:3 + g.PC_INFO_NAME_LEN].split(b"\0", 1)[0].decode("latin-1")
        job, = struct.unpack_from("<I", b, g.PC_INFO_OFF_JOB)
        lv = b[g.PC_INFO_OFF_LV]
        head = b[g.PC_INFO_OFF_HEADTYPE]
        equips = [struct.unpack_from("<I", b, g.PC_INFO_OFF_EQUIP + s * 12)[0]
                  for s in range(g.EQUIP_SPOT_LAST)]
        used = [e for e, s in zip(equips, range(g.EQUIP_SPOT_LAST))
                if e != g.EQUIP_NONE[s]]
        print("  %-4d  %-10s  %-4d %-4d %-5d %s"
              % (slot, name, job, lv, head, used or "(none)"))

    p = g.build_barrack_info(chars)
    print()
    print("BARRACK_INFO  : %d bytes total (11 header + %d * %d)"
          % (len(p), len(chars), g.PC_INFO_SIZE))
    print("CREATE_CHAROK : %d bytes, LV at offset 275 = %d"
          % (len(g.build_create_charok({}, 1)), g.build_create_charok({}, 1)[275]))
    ci = g.build_commander_info({"name": "Arcis", "zone_id": 10})
    print("COMMANDER_INFO: %d bytes, MAX_BARRACK_CHANNEL = %d"
          % (len(ci), g.MAX_BARRACK_CHANNEL))


if __name__ == "__main__":
    main()
