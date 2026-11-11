"""Test barrack packet building."""
import struct
import ge_barrack_server as gbs
import ge_db

cm = ge_db.get_commander(24)
chars = ge_db.get_characters(24)

ci = gbs.build_commander_info(cm)
print("COMMANDER_INFO len", len(ci), "cmd", struct.unpack_from("<H", ci, 0)[0])
print("  name field", ci[2:2 + gbs.COMMANDER_NAME_LEN])

bi = gbs.build_barrack_info(chars)
cmd, ln = struct.unpack_from("<HH", bi, 0)
cnt = bi[4]
print("BARRACK_INFO len", len(bi), "declared", ln, "cmd", cmd, "count", cnt)
# walk PC_INFO
off = 6
for i in range(cnt):
    slot, = struct.unpack_from("<H", bi, off)
    name = bi[off + 3:off + 3 + gbs.CHAR_NAME_LEN].split(b"\0")[0].decode()
    lv = bi[off + 3 + gbs.CHAR_NAME_LEN + 2 + 21 * 4 + 21 + 1]
    print("  slot", slot, "name", name, "LV", lv)
    off += 157
print("walked to", off, "== len", len(bi))

print("EQUIP_EXTRA", len(gbs.build_equip_extra()))
print("CHANNEL_LIST", len(gbs.build_channel_list()))
print("STARTGAMEOK", len(gbs.build_startgameok()))
print("LOGINOK", len(gbs.build_loginok(35, 0x3333, "Arcanine")))
