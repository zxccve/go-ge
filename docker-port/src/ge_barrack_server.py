"""ge_barrack_server - login + barrack conversation stub.

Supersedes ge_server.py (which only did login). Adds the barrack handshake:

    CB_LOGIN(4)            -> BC_LOGINOK(8)
    CB_STARTBARRACK(9)     -> BC_COMMANDER_INFO(10)
    CB_REQ_BARRACK_INFO(15)-> BC_BARRACK_INFO(16)
                              + BC_BARRACK_EQUIP_INFO_EXTRA(17)
                              + BC_CHANNEL_LIST(34)
    CB_STARTGAME(22)       -> BC_STARTGAMEOK(25)   [points at a zone stub]

Rules that cost us a lot to learn (see RESUME.md):
  * client -> server is Blowfish-encrypted, framed `[WORD blockByte][ciphertext]`
  * server -> client is PLAINTEXT with NO length prefix
  * the SHIPPED client's structs do not match source2's headers
    (its ACC_NAME_LEN is 218, not 51). Sizes below are the source2 layout and
    are flagged for verification against the client binary.

Run:  python ge_barrack_server.py
Then click LOGIN in the client. Ctrl+C to stop.
"""
import datetime
import hashlib
import json
import os
import re
import socket
import struct
import threading

import ge_crypt as gc
import ge_db
import ge_item_props
import ge_tables
import world
from ge_paths import DATA, LOGS, OUT, GAME_ROOT

HOST = "127.0.0.1"
# The port is configurable so a CAPTURE RELAY can sit in front of us:
# `pktmon` cannot see loopback traffic, so to capture what the client sends
# to OUR server the relay must be the thing the client connects to
# (e.g. PORT=7010, and the relay on 7001 forwards to 127.0.0.1:7010).
PORT = int(os.environ.get("BARRACK_PORT", "7001"))
# Fallback only. The real address of the zone for a map comes from the world
# module (world.zone_address), which the running zone announces. This is what
# we used before, and it is still the fallback if no zone has announced itself.
ZONE_STUB_PORT = 7002
LOG_FILE = os.path.join(LOGS, "barrack_server.log")
# The log is written from several connection threads, so the writes are
# serialised.
LOG_LOCK = threading.Lock()
# How long we keep a silent connection. The client goes quiet for a long time
# while it builds the barrack, so a short timeout drops it mid-hang and hides
# the very thing we are debugging. Keep this generous.
IDLE_TIMEOUT = 900

# DEBUG ONLY (default 0 = send every character, i.e. normal behaviour).
# Set env GE_BARRACK_MAX_CHARS=1 to report only the first character. The client
# builds a 3D model for each character it is told about (bkactor::SPCEnter ->
# World()->EnterWorld), so this tells us whether the ~50 s barrack wait scales
# with the character count. Nothing is written to the database either way.
MAX_CHARS_DEBUG = 0
try:
    MAX_CHARS_DEBUG = int(os.environ.get("GE_BARRACK_MAX_CHARS", "0") or 0)
except ValueError:
    MAX_CHARS_DEBUG = 0


def limit_chars(chars):
    """Apply the GE_BARRACK_MAX_CHARS debug limit (0 = no limit)."""
    if EMPTY_BARRACK:
        return []
    if FAKE_CHAR:
        # One synthetic character with neutral fields: no real name, no gear,
        # level 1, job 1. Keeps the client on the normal (MAKE_TEAM) branch
        # while removing every value we read out of the database.
        return [{"slot": 0, "teampos": 0, "name": "Test", "gender": 1, "job": 1,
                 "headtype": 0, "life": 1, "hp": 1, "sp": 1, "exp": 0,
                 "equip": {}}]
    if MAX_CHARS_DEBUG > 0:
        return chars[:MAX_CHARS_DEBUG]
    return chars


def load_chars(commander_id):
    """get_characters() plus the per-character promotion properties.

    The original barrack reads PromotionStep / CashPromotionStep as separate
    rows in CHARACTERS_PROPERTY (BarrackServer/PCLoader.cpp ->
    CPCPropLoader -> S_GET_CHARACTERS_PROPERTY_2) and folds them into PC_INFO.
    """
    chars = ge_db.get_characters(commander_id)
    promos = ge_db.get_character_promotion(commander_id)
    for ch in chars:
        ch["promotion"], ch["cash_promotion"] = promos.get(ch["character_id"], (0, 0))
    return limit_chars(chars)


# DEBUG ONLY (default off = normal behaviour).
# GE_BARRACK_EMPTY=1 sends a barrack with NO characters at all. This tests
# whether the ~50 s stall comes from the per-character PC_INFO records we build
# out of the database, without touching any of the rows.
EMPTY_BARRACK = os.environ.get("GE_BARRACK_EMPTY", "") not in ("", "0")
# GE_BARRACK_FAKE_CHAR=1 sends ONE synthetic, field-stripped character. Use it
# after EMPTY_BARRACK to tell "the character records matter" apart from
# "the mere presence of a character changes which branch the client takes".
FAKE_CHAR = os.environ.get("GE_BARRACK_FAKE_CHAR", "") not in ("", "0")


# DEBUG ONLY (default empty = send everything, i.e. normal behaviour).
# GE_BARRACK_SKIP=lock,job,extra,chan,hotkey omits those replies, to bisect which
# one makes the SHIPPED client burn ~50 s building the barrack. Names:
#   lock   = BC_UPDATE_TABLE ("Lock" rights table)
#   job    = BC_ROTATION_CHAR_LIST (command 66)
#   extra  = BC_BARRACK_EQUIP_INFO_EXTRA
#   chan   = BC_CHANNEL_LIST
#   hotkey = BC_HOTKEY_BIND_INFO (command 56, the script-binding table)
# None of these are written to the database; skipping one only changes what the
# client is told, and is fully reversible by restarting without the variable.
SKIP = set(x.strip().lower()
           for x in os.environ.get("GE_BARRACK_SKIP", "").split(",") if x.strip())

# DEBUG ONLY (default empty = build the Lock table ourselves, normal behaviour).
# GE_BARRACK_LOCK_XML=<path> replays the exact ids-space XML taken from a
# reference server's captured traffic, to test whether our smaller Lock table is
# what makes the client burn ~50 s. The file holds the raw XML, no packet header.
LOCK_XML_FILE = os.environ.get("GE_BARRACK_LOCK_XML", "")

# A/B overrides for the two COMMANDER_INFO values the original DERIVES.
# Empty = derive from the Lock table file, like the original does.
CHARSLOT_OVERRIDE = os.environ.get("GE_BARRACK_CHARSLOT", "")
RANK_OVERRIDE = os.environ.get("GE_BARRACK_RANK", "")

# DEBUG ONLY: GE_BARRACK_CM_BIN=<path> replays a captured reference server's raw
# 744-byte BC_COMMANDER_INFO verbatim (every field, including ones we cannot
# explain), to test whether the stall comes from COMMANDER_INFO at all.
CM_BIN_FILE = os.environ.get("GE_BARRACK_CM_BIN", "")

# ---- BC_HOTKEY_BIND_INFO (the script-binding table) ----
# The live barrack sends this right after LOGINOK and we sent NONE of it.
# Measured 2026-10-21: the live barrack's reply is 86 KB against our 14 KB, and
# the difference is mostly this one packet.
#
#   cmd(2) = 56, len(2) = 7571 (the TOTAL length of the packet, cmd included),
#   then DWORD count = 348, then count x ( DWORD id, NUL-terminated script name ).
#   The names are the game's own scripts: "QuickMove()", "QuickHarvest()",
#   "ChangeTactics(\"TS_HOLD\")", "EscKeyHandler()", ... up to id 403.
#
# It is STATIC server data: the three live captures of it are byte-identical
# (same SHA256) even though they came from different accounts and sessions, so
# it is replayed verbatim from a capture instead of being rebuilt.
#
# The file is produced by `python extract_replay_packets.py <capture> --cmd 56`
# (see that tool for the framing rule it uses).
HOTKEY_BIN_FILE = os.path.join(DATA, "replay", "BC_HOTKEY_BIND_INFO_56.bin")

# DEBUG ONLY: GE_BARRACK_REPLAY_AFTER_HOTKEY=<file,file,...> sends the given
# captured packets (files under data\replay\) right after the script-binding
# table, in the order given. It exists to try the LIVE server's exact packet
# SEQUENCE: the live barrack sends the hotkey table and then NINE
# BC_ROTATION_CHAR_LIST packets, and we send one much later.
# The files are produced by `python extract_replay_packets.py <capture> --cmd 66`.
REPLAY_AFTER_HOTKEY = [x.strip() for x in
                       os.environ.get("GE_BARRACK_REPLAY_AFTER_HOTKEY", "").split(",")
                       if x.strip()]

# DEBUG ONLY: GE_BARRACK_FULL_REPLAY=<file,file,...> sends the given captured
# packets - in the order given - right after LOGINOK, and then sends NOTHING
# else: the barrack's own COMMANDER_INFO / JOB_LIST / UPDATE_TABLE /
# BARRACK_INFO replies are suppressed, so the client sees exactly the LIVE
# server's packet sequence and nothing else.
#
# Why: the 2026-10-17 investigation concluded the ~58 s barrack stall was
# client-side, but it only ever replayed the live's BC_BARRACK_INFO verbatim.
# The live barrack sends 86 KB in a specific ORDER and we send ~14 KB, so the
# honest test is the whole sequence, not one packet.
# The files come from `python extract_replay_packets.py <capture> --out
# data\replay_live`; the order is the manifest's order (by offset).
FULL_REPLAY = [x.strip() for x in
               os.environ.get("GE_BARRACK_FULL_REPLAY", "").split(",") if x.strip()]

# DEBUG ONLY: GE_BARRACK_REPLACE="barrack_info:file.bin,equip_extra:file.bin,..."
# replaces ONE of our own replies with a captured packet, so a bisect is one
# env var and one run. Names:
#   commander_info, update_table, job_list, barrack_info, equip_extra, channel
# The file is looked up in data\replay_live first, then data\replay.
REPLACE = {}
for _item in os.environ.get("GE_BARRACK_REPLACE", "").split(","):
    if ":" in _item:
        _k, _v = _item.split(":", 1)
        REPLACE[_k.strip().lower()] = _v.strip()


def replaced(name, default):
    """The captured packet to send instead of ours, or None to send ours."""
    fn = REPLACE.get(name)
    if not fn:
        return None
    path = fn if os.path.isabs(fn) else replay_dir_for(fn)
    if not os.path.exists(path):
        log("!! REPLACE file missing for %s: %s" % (name, path))
        return None
    with open(path, "rb") as f:
        return f.read()


def replay_dir_for(name):
    """Where a replay file lives: data\replay_live first, then data\replay."""
    for d in (os.path.join(DATA, "replay_live"), os.path.join(DATA, "replay")):
        p = os.path.join(d, name)
        if os.path.exists(p):
            return p
    return os.path.join(DATA, "replay_live", name)

# ---- command ids ----
# Taken from the SHIPPED client's own packet table (dump_table.py), not source2.
# IMPORTANT: the shipped build inserted BC_LOGGING at 30, so every source2
# command from 30 upward is off by +1 here. Verified range:
#     28 CB_ECHO  29 BC_ECHO  30 BC_LOGGING  31 BC_COMMANDER_POINT
#     32 BC_UPDATE_TABLE  33 BC_CHAT_MACRO_LIST  34 BC_WHISPER_LIST
#     35 BC_CHANNEL_LIST  36 BC_BILLING_ALARM  37 BC_COMMANDER_LV_INFO
#     64 CB_EQUIP_ITEM_PRESET_INIT  66 BC_ROTATION_CHAR_LIST
# Anything at or below 29 matches source2, so the login/barrack ids are fine.
CMD_CB_LOGIN = 4
CMD_BC_LOGINOK = 8
CMD_CB_STARTBARRACK = 9
CMD_BC_COMMANDER_INFO = 10
CMD_CB_REQ_BARRACK_INFO = 15
CMD_BC_BARRACK_INFO = 16
CMD_BC_BARRACK_EQUIP_INFO_EXTRA = 17
CMD_CB_CREATE_CHAR = 18
CMD_BC_CREATE_CHAROK = 19
CMD_CB_DESTROY_CHAR = 20
CMD_BC_DESTROY_CHAROK = 21
CMD_CB_STARTGAME = 22
CMD_CB_CONTINUEGAME = 23     # same body as STARTGAME: pick an existing team
CMD_BC_STARTGAMEOK = 25
CMD_CB_REQ_CHANGE_BARRACKNAME = 49
CMD_BC_CHANNEL_LIST = 35
CMD_BC_UPDATE_TABLE = 32          # pushes an IES table as XML (see build_update_table)
CMD_BC_ECHO = 29
CMD_CB_ECHO = 28
CMD_BC_JOB_LIST = 66           # shipped-build only (source enum ends at 56). Handler
                               # FUN_0052334e reads 30 dwords at packet+2 and fills
                               # the CSession job/shape vector (session+0x186c4).
                               # The barrack UI searches it for each character's job id
                               # with FUN_00508cd8(idx); an EMPTY vector asserts and
                               # crashes (VA 0x4ae92d). Send 30 ids before BARRACK_INFO.
CMD_BC_HOTKEY_BIND_INFO = 56   # the script-binding table. VARIABLE size (the
                               # client's table says 0), so it carries its own
                               # length at +2 - and that length is the TOTAL
                               # packet length, cmd and length bytes included.
                               # The live barrack sends it right after LOGINOK.

# How many times the job/rotation table (cmd 66) is sent. The live barrack
# sends the SAME 122-byte packet NINE times, and the count is not decoration:
# with the script-binding table in play, one copy crashes the client (see the
# comment at the send site). Measured, 2026-10-21.
JOB_LIST_REPEATS = 9

# ---- field sizes ----
# Sizes marked "client table" were read straight out of the running client's
# gePacketTable (see dump_table.py). They are authoritative over any header.
ACC_NAME_LEN = 218             # CB_LOGIN id field width (empirical, capture)
PWD_LEN = 17
LOGINOK_SIZE = 57              # client table: sizeof(PKS_BC_LOGINOK)
COMMANDER_INFO_SIZE = 744      # client table: sizeof(PKS_BC_COMMANDER_INFO)
STARTGAMEOK_SIZE = 81          # client table: sizeof(PKS_BC_STARTGAMEOK)
EQUIPMENT_PC_SIZE = 273        # 21 spots * 12 bytes + 21 reinforce bytes = 273.
                               # (BC_CREATE_CHAROK's table size is 277, i.e.
                               #  2 + 273 + 1(LV) + 1 pad - see build_create_charok)
PC_INFO_NAME_LEN = 17          # client's CHAR_NAME_LEN = 16+1 (geShared.h:189),
                               # NOT the 34 the previous session guessed
PC_INFO_SIZE = 343             # client: `BIN_READ(0x157)` before each PC_INFO

# Shipped client's PC_INFO field offsets, read from the client's own code
# (CBarrackNet::BarrackInfo FUN_00521a49 + bkactor::SPCEnter FUN_0052aea2):
#   0    WORD slotID
#   2    BYTE teamPos
#   3    char name[17]
#   20   BYTE gender
#   21   BYTE job            (the client reads a dword here -> job must be the
#                             low byte and bytes 22..24 must be 0)
#   25   EQUIPMENT_PC equipment (273 bytes copied into APPEARANCE.Equipment)
#   298  BYTE headType
#   299  BYTE LV
#   300  WORD HP, 302 maxHP, 304 SP, 306 maxSP
#   308  int64 EXP, 316 int64 nextEXP
#   336  WORD promotionLv
# (source2's header has name[17] too; the shipped build just differs in detail.)
PC_INFO_OFF_GENDER = 20
PC_INFO_OFF_JOB = 21
PC_INFO_OFF_HEADTYPE = 298
PC_INFO_OFF_LV = 299
PC_INFO_OFF_HP = 300
PC_INFO_OFF_MAXHP = 302
PC_INFO_OFF_SP = 304
PC_INFO_OFF_MAXSP = 306
PC_INFO_OFF_EXP = 308
PC_INFO_OFF_NEXTEXP = 316
PC_INFO_OFF_PROMOTION = 336

# Equipment lives at PC_INFO+25. The shipped layout is an array of 12-byte
# entries (classID + 2 dwords), one per EQUIP_SPOT, then the reinforce counts:
#   entry(spot) at PC_INFO + 25 + 12*spot
# Proof: bkactor's appearance setter FUN_005edfb1 reads the appearance's copy of
# this array at blob+0x44, +0x50, +0x5C ... +0xC8, i.e. stride 12, and
# 0xC8 = 0x44 + 11*12 (spot 11 = COSTUME). EQUIPMENT_PC is 21 entries * 12 + 21
# = 273 bytes.
PC_INFO_OFF_EQUIP = 25
EQUIP_ENTRY_STRIDE = 12
EQUIP_SPOT_LAST = 21
# The 21 reinforce ("+N") bytes sit directly after the 21 equipment entries:
#   25 + 21*12 = 277, and headType is at 298 = 277 + 21.  (EQUIPMENT_PC is
#   "21 entries * 12 bytes + 21 reinforce bytes = 273".)
PC_INFO_OFF_REINFORCE = PC_INFO_OFF_EQUIP + EQUIP_SPOT_LAST * EQUIP_ENTRY_STRIDE
# Exact per-spot "None" class ids, read from the client's own initializer
# FUN_0064b4e6 (writes [obj]=17000, [obj+0xC]=14000, ... stride 12).
EQUIP_NONE = [17000, 14000, 18000, 15000, 10000, 10000, 16000, 16000, 19000,
              19500, 41000, 42000, 46000, 45000, 90000, 91000, 24000, 28000,
              60001, 60001, 48000]

# ---- fixed-size CB packet header ----
# The client's own packet table gives the FULL wire size for these, and its value
# equals 10 + body, so the body starts at OFFSET 10. The security header for
# PKS_SECURITY_DEFAULT is {WORD command; DWORD seq; int checksum;} = 10 bytes.
# CB_LOGIN is PKS_SECURITY_VARSIZE and DOES carry a len field, so its header is 12
# (that is why parse_login reads its fields from offset 12 - see parse_login).
# Getting this wrong shifts every field by two bytes; a previous attempt did that
# and wrote garbage rows (slot 29537, job 0) that crashed the client on relogin.
CB_HDR_FIXED = 10

# CB_CREATE_CHAR body (10 + 53 = 63, the client's table size):
#   authCode[16], WORD slotID, char name[17], BYTE gender, DWORD job,
#   BYTE headType, DWORD cardType, DWORD invIndex, DWORD costumeID
CREATE_CHAR_OFF_SLOT = CB_HDR_FIXED + 16
CREATE_CHAR_OFF_NAME = CREATE_CHAR_OFF_SLOT + 2
CREATE_CHAR_OFF_GENDER = CREATE_CHAR_OFF_NAME + PC_INFO_NAME_LEN
CREATE_CHAR_OFF_JOB = CREATE_CHAR_OFF_GENDER + 1
CREATE_CHAR_OFF_HEAD = CREATE_CHAR_OFF_JOB + 4
# CB_DESTROY_CHAR body (10 + 35 = 45): authCode[16], WORD slotID, char name[17]
DESTROY_CHAR_OFF_SLOT = CB_HDR_FIXED + 16
DESTROY_CHAR_OFF_NAME = DESTROY_CHAR_OFF_SLOT + 2
# CB_REQ_CHANGE_BARRACKNAME body (10 + 21 = 31): int barrackindex, char name[17]
BARRACKNAME_OFF_INDEX = CB_HDR_FIXED
BARRACKNAME_OFF_NAME = CB_HDR_FIXED + 4
# CB_STARTGAME body (PKS_SECURITY_DEFAULT + CHANNEL_ID + WORD charSlotID[3]):
#   WORD channelID; WORD charSlotID[0..2]   (unused slots = EMPTY_CHARSLOT 255)
# The zone handshake (CZ_START_GAME, cmd 3590) carries NO team, so we record the
# user's selection here and the zone server reads it back (see TEAM_FILE).
STARTGAME_OFF_CHANNEL = CB_HDR_FIXED
STARTGAME_OFF_SLOTS = CB_HDR_FIXED + 2
EMPTY_CHARSLOT = 255
# Handoff file: the character slots the user picked for the team. The zone server
# reads this so it sends exactly the selected character(s), not the DB first-N.
# There is ONE FILE PER COMMANDER: with two players entering at the same time a
# single file would be overwritten and the first player would spawn as the
# second player's character. The zone asks for the file of the player it is
# serving, and falls back to this one.
TEAM_FILE = os.path.join(OUT, "zone_team.json")


def team_file_for(commander_id):
    """The handoff file for one player's team."""
    if not commander_id:
        return TEAM_FILE
    return os.path.join(OUT, "zone_team_%s.json" % commander_id)

COMMANDER_NAME_LEN = 17
PARTY_NAME_LEN = 17
PROFILE_LEN = 251
PARTY_TITLE_LEN = 9
CHAR_NAME_LEN = 17
MAP_NAME_LEN = 41
# The SHIPPED client has 24 barrack channels, not source2's 16. Proof: its
# BarrackInfo clears/loops 0xd8 = 216 slots, and MAX_SPC = SPC_PER_CHANNEL *
# MAX_BARRACK_CHANNEL with SPC_PER_CHANNEL = 9 (the list UI groups slots by
# `slot % 9`), so 216 / 9 = 24. The UI also declares barrack_ch1..barrack_ch24.
MAX_BARRACK_CHANNEL = 24
# The barrack-channel name list. `None` is the protocol's "no custom name"
# sentinel: the client's CBarrackListFrame::SetBarrackName() returns early for
# "None", so the button keeps its own localized default caption instead of being
# overwritten with an empty string (which is what blanked the left-hand grid).
BARRACK_NAME_DEFAULT = "None"
SPC_PER_CHANNEL = 9                      # the list UI groups slots by `slot % 9`
MAX_SPC = SPC_PER_CHANNEL * MAX_BARRACK_CHANNEL          # 9 * 24 = 216

# USER_RESTRICTION_INFO = { DWORD age; double selectiveShutdownTime }
USER_RESTRICTION_SIZE = 12


def log(msg):
    line = "[%s] %s" % (datetime.datetime.now().strftime("%H:%M:%S"), msg)
    # Several clients can be served at once now, so the log lines are written
    # under a lock - otherwise two threads can interleave half a line each.
    with LOG_LOCK:
        print(line, flush=True)
        # Also append to a file: when the server runs in another window we cannot
        # read its console, and the packet timeline is the key evidence for a hang.
        try:
            with open(LOG_FILE, "a", encoding="utf-8") as f:
                f.write(line + "\n")
        except OSError:
            pass


def cstr(s, n):
    """Fixed-width, NUL-terminated byte field."""
    b = s.encode("latin-1", "replace")[:n - 1]
    return b + b"\0" * (n - len(b))


def _int_or(v, default):
    """int(v) if that works, else `default` (handles '' and sqlcmd's 'NULL')."""
    try:
        if v is None or str(v).strip().upper() in ("", "NULL", "NONE"):
            return default
        return int(str(v).strip())
    except (TypeError, ValueError):
        return default


# --------------------------------------------------------------------------
# login
# --------------------------------------------------------------------------
def parse_login(pt):
    cmd, = struct.unpack_from("<H", pt, 0)
    seq, chk, ln = struct.unpack_from("<IIH", pt, 2)
    ident = pt[12:12 + ACC_NAME_LEN].split(b"\0", 1)[0].decode("latin-1")
    pw = pt[12 + ACC_NAME_LEN:12 + ACC_NAME_LEN + PWD_LEN]
    server_id, version = struct.unpack_from("<HI", pt, 12 + ACC_NAME_LEN + PWD_LEN)
    nation = pt[12 + ACC_NAME_LEN + PWD_LEN + 6:][:8].split(b"\0", 1)[0].decode("latin-1")
    return {"command": cmd, "seq": seq, "checksum": chk, "len": ln,
            "id": ident, "pw_md5": pw[:16].hex(), "server_id": server_id,
            "version": version, "nation": nation}


def build_loginok(account_id, session_id, ident):
    # client table says sizeof(PKS_BC_LOGINOK) == 57; sending exactly that keeps
    # the client's stream in sync (a longer packet left 193 stray bytes that
    # broke every later packet).
    p = struct.pack("<H", CMD_BC_LOGINOK)
    p += struct.pack("<I", account_id)
    p += struct.pack("<I", session_id)
    p += ident.encode("latin-1", "replace")[:LOGINOK_SIZE - 10]
    return p.ljust(LOGINOK_SIZE, b"\0")[:LOGINOK_SIZE]


def build_hotkey_bind_info():
    """The script-binding table (BC_HOTKEY_BIND_INFO, cmd 56), replayed verbatim.

    Returns the packet bytes, or None if the replay file is missing or does
    not look like the packet we expect. A missing file is LOUD: this packet is
    what the live barrack sends and we do not, so quietly not sending it would
    hide the very thing we are testing.
    """
    if not os.path.exists(HOTKEY_BIN_FILE):
        log("!! HOTKEY_BIND_INFO replay file is MISSING: %s" % HOTKEY_BIN_FILE)
        log("!! build it with: python extract_replay_packets.py "
            "<capture.bin> --cmd 56")
        return None
    with open(HOTKEY_BIN_FILE, "rb") as f:
        blob = f.read()
    if len(blob) < 4:
        log("!! HOTKEY_BIND_INFO replay file is too short (%d bytes)" % len(blob))
        return None
    cmd, total = struct.unpack_from("<HH", blob, 0)
    if cmd != CMD_BC_HOTKEY_BIND_INFO:
        log("!! HOTKEY_BIND_INFO replay file starts with cmd %d, expected %d"
            % (cmd, CMD_BC_HOTKEY_BIND_INFO))
        return None
    if total != len(blob):
        log("!! HOTKEY_BIND_INFO replay file: length field says %d but the file "
            "is %d bytes" % (total, len(blob)))
        return None
    count, = struct.unpack_from("<I", blob, 4)
    return blob, count


# --------------------------------------------------------------------------
# BC_COMMANDER_INFO
# --------------------------------------------------------------------------
def build_commander_info(cm):
    _r = replaced("commander_info", None)
    if _r is not None:
        return _r
    p = struct.pack("<H", CMD_BC_COMMANDER_INFO)
    p += cstr(cm["name"], COMMANDER_NAME_LEN)
    p += cstr("", PARTY_NAME_LEN)
    p += cstr("", PROFILE_LEN)
    p += cstr("", PARTY_TITLE_LEN)
    # rank and charSlot are DERIVED by the original, not constants:
    #   bcPacket->charSlot = <commander's Lock_CharSlot property>
    #   bcPacket->rank     = GetBarrackServer()->GetMaxTeamGroupCount()
    # We used to hardcode 15 and 3, which are values the original never sends.
    # GE_BARRACK_CHARSLOT / GE_BARRACK_RANK override them for A/B testing.
    lock = dict(load_lock_props())
    char_slot = _int_or(CHARSLOT_OVERRIDE, _int_or(lock.get("Lock_CharSlot"), 4))
    rank = _int_or(RANK_OVERRIDE, _int_or(lock.get("Lock_TeamGroup"), 3))
    p += struct.pack("<H", _u16(rank))              # rank == max team groups
    p += struct.pack("<H", _u16(char_slot))         # charSlot
    p += struct.pack("<I", 0)                       # commanderPoint
    p += struct.pack("<H", 0)                       # ladderPoint
    # The client does geMapTable::Get(newMapID)->name with NO null check, so
    # newMapID must be a real map class id. The real server sends the zone id
    # (COMMANDER.ZONE_ID); contMapID / continueMissionMapID are null-checked.
    zone = cm.get("zone_id", 0) & 0xFFFF
    p += struct.pack("<H", zone)   # newMapID
    p += struct.pack("<H", zone)   # contMapID
    p += struct.pack("<H", 0)      # continueMissionMapID
    p += struct.pack("<H", 0)   # commLvMax
    p += struct.pack("<B", 0)   # commLvPercent
    p += struct.pack("<H", 0)   # commLvTotal
    p += struct.pack("<H", 0)   # commLvCurrent
    p += struct.pack("<I", 0)   # mccTime
    p += struct.pack("<I", 0)   # mccCashTime
    # Barrack channel names (one per channel). CBarrackNet::CommanderInfo does
    #   for (j = 0; j < MAX_BARRACK_CHANNEL; j++)
    #       pFrame->SetBarrackName(packet->m_barrackName[j], j);
    # so whatever we put here REPLACES the button's own caption. Sending "" made
    # the left-hand barrack grid blank; sending "None" makes the client keep its
    # own localized caption ("Barrack 1" ... "Barrack 24"). The real server reads
    # a per-commander property "BrkName<j+1>" and only overrides with that.
    barrack_names = cm.get("barrack_names") or {}
    for j in range(MAX_BARRACK_CHANNEL):
        p += cstr(barrack_names.get(j, BARRACK_NAME_DEFAULT), CHAR_NAME_LEN)
    p += struct.pack("<B", 0)   # WorldPVPClass
    p += struct.pack("<B", 0)   # MeClique
    # client's struct is bigger (744); any extra trailing fields read as zero
    return p.ljust(COMMANDER_INFO_SIZE, b"\0")[:COMMANDER_INFO_SIZE]


# --------------------------------------------------------------------------
# PC_INFO / BC_BARRACK_INFO
# --------------------------------------------------------------------------
def _u16(v):
    return max(0, min(int(v), 0xFFFF))


# GE_BARRACK_PCINFO selects where PC_INFO's level fields come from:
#   "table"  (default) - the game's own data tables, exactly like the original
#                        server (see ge_tables). CCharacterSlot::MakeBCPacket:
#                          LV              = GetLevel(exp)          (from EXP,
#                                             NOT the DB's LIFE column)
#                          nextEXP         = GetExp(level + 1)
#                          promotionLv     = min(GetPromotionLevel(exp),
#                                                PromotionStep)
#                          cashPromotionLv = CashPromotionStep
#                        (Character.cpp:49,52 and CharacterSlot.cpp:355-378.)
#   "legacy" - the old behaviour: LV = the DB's LIFE column, nextEXP = exp + a
#              guess, both promotion fields 0. Kept so the two can be A/B tested
#              without editing code.
PCINFO_MODE = os.environ.get("GE_BARRACK_PCINFO", "table").strip().lower()
# GE_BARRACK_NEXTEXP=zero pins the *legacy* nextEXP to 0 (an even older mode).
NEXT_EXP_MODE = os.environ.get("GE_BARRACK_NEXTEXP", "derive").strip().lower()


def _legacy_next_exp(c):
    if NEXT_EXP_MODE == "zero":
        return 0
    return int(c["exp"]) + 10_000_000


def pc_info_levels(c):
    """(LV, nextEXP, promotionLv, cashPromotionLv) for one character.

    We used to send the DB's LIFE column as the level, `exp + 10_000_000` as
    nextEXP and 0 for both promotion fields. The DB's LIFE column is NOT the
    level: it disagrees with the game's own table for 10 of the 12 characters
    here (e.g. Grandies has LIFE 99 but is level 60, Hilda has 104 but is 140),
    so the client was shown a wrong level plus a nonsensical nextEXP.
    """
    if PCINFO_MODE != "table":
        return int(c["life"]), _legacy_next_exp(c), 0, 0
    exp = int(c["exp"])
    lv = ge_tables.level_of(exp)
    return (lv,
            ge_tables.exp_of(lv + 1),
            ge_tables.promotion_level(exp, c.get("promotion", 0)),
            int(c.get("cash_promotion", 0)))


def build_pc_info(c):
    # Build using the client's OWN field offsets, not source2's header.
    # (The old code used a 34-byte name and put job at +38, so the client read
    #  job = 0 at +21 and crashed in geJobTable::Get(0).)
    buf = bytearray(PC_INFO_SIZE)
    struct.pack_into("<H", buf, 0, _u16(c["slot"]))              # slotID
    buf[2] = c["teampos"] & 0xFF                                  # teamPos
    buf[3:3 + PC_INFO_NAME_LEN] = cstr(c["name"], PC_INFO_NAME_LEN)
    buf[PC_INFO_OFF_GENDER] = 1 if c["gender"] else 0
    buf[PC_INFO_OFF_JOB] = c["job"] & 0xFF                        # job
    equip = c.get("equip", {})
    reinforce = c.get("reinforce", {})
    for spot in range(EQUIP_SPOT_LAST):
        cls = equip.get(spot) or EQUIP_NONE[spot]
        struct.pack_into("<I", buf, PC_INFO_OFF_EQUIP + spot * EQUIP_ENTRY_STRIDE,
                         cls & 0xFFFFFFFF)
        # Per-spot reinforce ("+N") byte. The original fills this from
        # GetReinforceCount(item) in CCharacterSlot::MakeBCPacket; we used to
        # leave all 21 zero, so the barrack could not show a reinforced item
        # (and the client saw "no reinforce" for gear that has it).
        buf[PC_INFO_OFF_REINFORCE + spot] = min(int(reinforce.get(spot, 0)), 255)
    buf[PC_INFO_OFF_HEADTYPE] = c["headtype"] & 0xFF
    lv, next_exp, promotion_lv, cash_promotion_lv = pc_info_levels(c)
    buf[PC_INFO_OFF_LV] = min(lv, 255)                            # level (see above)
    struct.pack_into("<H", buf, PC_INFO_OFF_HP, _u16(c["hp"]))
    struct.pack_into("<H", buf, PC_INFO_OFF_SP, _u16(c["sp"]))
    struct.pack_into("<q", buf, PC_INFO_OFF_EXP, int(c["exp"]))
    # nextEXP is the CUMULATIVE score needed for the next level (the client
    # subtracts the current EXP from it), i.e. the table's TotalXp for level+1.
    struct.pack_into("<q", buf, PC_INFO_OFF_NEXTEXP, next_exp)
    struct.pack_into("<H", buf, PC_INFO_OFF_PROMOTION, min(promotion_lv, 0xFFFF))
    # cashPromotionLv is the WORD right after promotionLv (PC_INFO header:
    # `WORD promotionLv; WORD cashPromotionLv;`).
    struct.pack_into("<H", buf, PC_INFO_OFF_PROMOTION + 2,
                     min(cash_promotion_lv, 0xFFFF))
    assert len(buf) == PC_INFO_SIZE
    return bytes(buf)


def build_barrack_info(chars):
    _r = replaced("barrack_info", None)
    if _r is not None:
        return _r
    # The SHIPPED client reads the header with BIN_READ(0x0B): 11 bytes, not the
    # 6 the source2 header implies. Proof: the handler FUN_00521a49 decompiles to
    # `bin.Create(buf,len); bin.Read(0xb); count = packet[4]; ... loop
    # bin.Read(0x157)`. So PC_INFO records start at offset 11 in the packet.
    # Layout the client relies on: command(2), len(2), characterCount(1),
    # pad(1), existOnGoingMission DWORD(4) at +6, pad(1).
    body = b""
    for c in chars:
        body += build_pc_info(c)
    total = 11 + len(body)                    # command + len + 7 header bytes + records
    hdr = struct.pack("<HH", CMD_BC_BARRACK_INFO, total)
    hdr += struct.pack("<B", len(chars))      # +4 characterCount
    hdr += b"\0"                              # +5 pad
    hdr += struct.pack("<I", 0)               # +6 existOnGoingMission (read as DWORD)
    hdr += b"\0"                              # +10 pad
    assert len(hdr) == 11, len(hdr)
    return hdr + body


def build_equip_extra(chars=None):
    """BC_BARRACK_EQUIP_INFO_EXTRA (cmd 17): the per-character item data.

    The CLIENT's header for this packet is 16 bytes, not 12: the shipped
    handler FUN_0052195d does `Read(0x10)` for the header and then reads
    THREE counts straight out of the packet memory at +4, +8 and +12:

        cmd(2) + len(2) + nOptions(4) + nSockets(4) + nProperties(4)

    then `nOptions` records of 16 bytes, `nSockets` records of 12 bytes, and
    `nProperties` records of 9 bytes (+ a property object when the flag is
    set).

    We used to send a 12-byte packet, so the client read the third count from
    BEYOND our packet - a garbage count - and then spent ~58 s grinding
    through records that were never there (ROADMAP R7, measured 2026-10-21:
    the same packet with a correct 16-byte header and the real option records
    brings the barrack up in 2.6 s instead of 58 s).

    The records:
      * option, 16 bytes: DWORD optionType; DWORD optionValue;
                           DWORD charSlot; DWORD equipSpot
      * socket, 12 bytes: the item's socket data (we send none: the
                           `ITEM_SOCKET` table is empty in our data)
      * property, 9 bytes + object: DWORD charSlot; DWORD equipSpot;
                                   BYTE exist; [CObject if exist]
        The CObject is the same `imcIES::CObject` form the zone sends for the
        bag's items (see `ge_item_props.py`).
    """
    _r = replaced("equip_extra", None)
    if _r is not None:
        return _r
    chars = chars or []
    options = b""
    for c in chars:
        slot = int(c.get("slot", 0)) & 0xFFFFFFFF
        for opt in ge_db.get_char_equip_options(c["character_id"]):
            options += struct.pack("<IIII", int(opt["type"]) & 0xFFFFFFFF,
                                   int(opt["value"]) & 0xFFFFFFFF,
                                   slot, int(opt["spot"]) & 0xFFFFFFFF)
    # The properties of the worn items: one record per worn item, with the
    # object only when that item has properties.
    #
    # NOT SENT BY DEFAULT. The shape of this section is NOT established: the
    # live server's third section holds 329 records in 6,723 bytes, i.e. ~20
    # bytes each - too small for an `imcIES::CObject` with two properties
    # (~100 bytes). So our reading of that section is wrong, and sending our
    # guess CRASHED the real client (0xc0000005, three seconds after
    # BC_BARRACK_INFO, 2026-10-21). The option records alone are accepted by
    # the client, so they are what we send; the property section waits for its
    # real shape.
    #   GE_BARRACK_EQUIP_PROPERTIES=1 sends the (unproven) property records.
    properties = b""
    n_props = 0
    if os.environ.get("GE_BARRACK_EQUIP_PROPERTIES", "0") == "1":
        worn = ge_db.get_char_worn_items([c["character_id"] for c in chars])
        item_ids = [i for lst in worn.values() for _spot, i in lst]
        props_by_item = ge_db.get_item_properties_of(item_ids)
        for c in chars:
            slot = int(c.get("slot", 0)) & 0xFFFFFFFF
            for spot, item_id in worn.get(c["character_id"], []):
                props = props_by_item.get(item_id) or []
                rec = struct.pack("<IIB", slot, spot & 0xFFFFFFFF,
                                  1 if props else 0)
                if props:
                    rec += ge_item_props.build_item_property_object(props)
                properties += rec
                n_props += 1
    total = 16 + len(options) + len(properties)
    return struct.pack("<HHIII", CMD_BC_BARRACK_EQUIP_INFO_EXTRA, total,
                       len(options) // 16, 0, n_props) + options + properties


# --------------------------------------------------------------------------
# character create / delete
# --------------------------------------------------------------------------
def build_equipment_pc(equip=None):
    """EQUIPMENT_PC: 21 spots at a 12-byte stride (classID + 2 dwords), then the
    per-spot reinforce bytes, padded to the client's 274-byte size. Same layout
    build_pc_info writes into PC_INFO, so the client reads it the same way."""
    buf = bytearray(EQUIPMENT_PC_SIZE)
    equip = equip or {}
    for spot in range(EQUIP_SPOT_LAST):
        cls = equip.get(spot) or EQUIP_NONE[spot]
        struct.pack_into("<I", buf, spot * EQUIP_ENTRY_STRIDE, cls & 0xFFFFFFFF)
    return bytes(buf)


def build_create_charok(equip=None, lv=1):
    # Client table: sizeof(BC_CREATE_CHAROK) = 277.
    # The equipment region is 273 bytes (same as PC_INFO+25), so LV lands at
    # offset 275 (275 = 2 + 273). Writing LV at 276 made the barrack show the new
    # character at level 0, so we write it at 275 and pad one byte to reach 277.
    return (struct.pack("<H", CMD_BC_CREATE_CHAROK) + build_equipment_pc(equip)
            + struct.pack("<B", lv) + b"\0")


def build_destroy_charok():
    # client table: sizeof(BC_DESTROY_CHAROK) = 2 (the command word only)
    return struct.pack("<H", CMD_BC_DESTROY_CHAROK)


def build_channel_list(map_id=1, channels=1):
    """BC_CHANNEL_LIST (cmd 35, PKS_VARSIZE).

    `channels` is a LIST of channel numbers (or a count, for the old
    single-channel case).

    Client layout: command(2) + len(2) + count(1) + newZoneChannelCount(1)
    + continueZoneChannelCount(1), then count * (MAP_ID(2) + channel(1)
    + traffic(1)).  MAP_ID is a WORD.

    WHY THIS MATTERS: the client extracts `count` entries into
    Session().CHANNEL_LIST. If that list is EMPTY, CBarrackMode::UpdateTeamButton
    (GEBarrackMode.cpp:465) sets enableNewGame=false and enableContinueGame=false,
    so BOTH the "Save Point" and "Last Point" buttons are DISABLED and the player
    can never leave the barrack. CSelectTeamFrame::UpdateChannel then calls
    Session().GetChannelInfo(i, mapType), which matches channel==i AND
    mapID==mapType, so each entry must carry the zone's map class id (that is
    what BC_COMMANDER_INFO sends as newMapID/contMapID, e.g. 10 = reb_basic).
    The old 7-byte packet packed a WORD where the client reads the three BYTE
    counters and carried NO entries, so the list was empty.
    """
    if isinstance(channels, int):
        channels = list(range(channels))
    channels = list(channels) or [0]
    _r = replaced("channel", None)
    if _r is not None:
        return _r
    body = struct.pack("<BBB", len(channels), len(channels), len(channels))
    for ch in channels:
        body += struct.pack("<HBB", map_id & 0xFFFF, ch & 0xFF, 0)  # traffic 0
    total = 4 + len(body)
    return struct.pack("<HH", CMD_BC_CHANNEL_LIST, total) + body


def build_job_list(jobs):
    _r = replaced("job_list", None)
    if _r is not None:
        return _r
    # command(2) + exactly 30 dwords (the client reads 30 entries unconditionally).
    vals = list(dict.fromkeys(int(j) & 0xFFFFFFFF for j in jobs))[:30]
    vals += [0] * (30 - len(vals))
    return struct.pack("<H", CMD_BC_JOB_LIST) + struct.pack("<30I", *vals)


# --------------------------------------------------------------------------
# BC_UPDATE_TABLE - push an IES table to the client as XML
# --------------------------------------------------------------------------
# WHY THIS IS NEEDED (the character-creation list was empty because of it):
# CMakeCharFrame::SetCreatableJobList() lists every job in the client's job
# table where IsCreatableJob() is true. For a CREATE_DEFAULT job that is:
#     Lock_<classType>CrtRight  must be non-zero in the "Lock" ids space:
#         imcIES::GetPropertyNumber("Lock", 1, "Lock_FGCrtRight")
# The client's OWN data defines those properties in the "Commander" ids space
# (ge\ies\datatable_commanderlock.ies), NOT in "Lock", so the lookup fails and
# EVERY default job is filtered out -> empty dropdown. The real server fixes
# this by extracting the commander's Lock_* properties under a NEW ids space
# called "Lock" and pushing them with BC_UPDATE_TABLE
# (CCommander::SendLockInfo -> geXmlExtractor::ExtractXmlFromIES).
#
# The XML shape is exactly what ExtractXmlFromIES emits:
#   <idspace id="Lock"> <Class ClassID="1" ClassName="Lock" N = "V" ... /></idspace>
# Packet layout: command(2) + len(2) + type[20] + xml + NUL, len = whole size.
LOCK_XML_SOURCES = [
    os.path.join(GAME_ROOT, "ge", "xml", "datatable_commanderlock.xml"),
]
# Fallback if that file cannot be read: the class types with CrtRight=1 in it.
LOCK_PROPS_FALLBACK = [
    ("Lock_BarrackLv", 1), ("Lock_MCCLv", 3), ("Lock_TradeRight", 1),
    ("Lock_PoseLevel", 1), ("Lock_FGTCrtRight", 1), ("Lock_SCTCrtRight", 1),
    ("Lock_WIZCrtRight", 1), ("Lock_MUSCrtRight", 1), ("Lock_WARCrtRight", 1),
    ("Lock_NEO10CrtRight", 1), ("Lock_PartyJoinRight", 1), ("Lock_PartyCrtRight", 0),
    ("Lock_CharSlot", 4), ("Lock_UserShopSlot", 4), ("Lock_Rank", 10000),
    ("Lock_WeightLimit", 50000), ("Lock_Warp", 5), ("Lock_InventoryCount", 248),
    ("Lock_WareCount", 500),
]


def load_lock_props():
    """Lock_* properties of Class ClassID=1 from the server's
    datatable_commanderlock.xml. The real server reads these from the
    commander's own IES object; our DB has no such columns, so the file's
    defaults are the right values."""
    for path in LOCK_XML_SOURCES:
        if not os.path.exists(path):
            continue
        try:
            with open(path, "r", encoding="latin-1") as f:
                text = f.read()
        except OSError:
            continue
        m = re.search(r'<Class\s+ClassID="1"(.*?)/>', text, re.S)
        if not m:
            continue
        props = re.findall(r'(Lock_[A-Za-z0-9_]+)\s*=\s*"([^"]*)"', m.group(1))
        if props:
            return props
    return LOCK_PROPS_FALLBACK


def build_update_table(idspace="Lock", props=None):
    _r = replaced("update_table", None)
    if _r is not None:
        return _r
    if LOCK_XML_FILE and os.path.exists(LOCK_XML_FILE):
        # DEBUG/test path: replay the EXACT ids-space XML a reference server
        # pushes (captured from its own traffic), instead of our hand-built one.
        with open(LOCK_XML_FILE, "rb") as f:
            xml = f.read().decode("latin-1")
        log("UPDATE_TABLE: using %s (%d bytes) as the %s XML"
            % (LOCK_XML_FILE, len(xml), idspace))
    else:
        if props is None:
            props = load_lock_props()
        attrs = "".join('%s = "%s" ' % (k, v) for k, v in props)
        xml = '<idspace id="%s"> <Class ClassID="1" ClassName="%s" %s/></idspace> \n' \
              % (idspace, idspace, attrs)
    body = xml.encode("latin-1", "replace") + b"\0"
    total = 24 + len(body)                  # cmd(2) + len(2) + type[20] + xml + NUL
    return (struct.pack("<HH", CMD_BC_UPDATE_TABLE, total)
            + cstr(idspace, 20) + body)


# --------------------------------------------------------------------------
# BC_STARTGAMEOK  (points at the zone that serves this map)
# --------------------------------------------------------------------------
def zone_target(map_id):
    """Where the client should go for this map: (ip, port).

    The world module owns the answer. The old hardcoded 127.0.0.1:7002 is
    only the fallback, for when no zone has announced itself yet.
    """
    addr = world.zone_address(map_id)
    if addr:
        return addr[0], int(addr[1])
    return "127.0.0.1", ZONE_STUB_PORT


def zone_channels(map_id):
    """The channel numbers of the live zones serving this map.

    The client matches a channel entry against the map id AND the channel
    number, so this has to be the real list of channels, not a guess.
    """
    chans = sorted({int(r.get("channel") or 0)
                    for r in world.zone_records()
                    if r.get("map_id") == map_id})
    return chans or [0]


def build_startgameok(zone_inst_id=1, ip="127.0.0.1", port=ZONE_STUB_PORT,
                      map_name="reb_basic", pos=(0.0, 0.0, 0.0), channel=0):
    # The real barrack sends map->mapName, which is the map's CLASS name
    # (datatable_map.xml ClassName, e.g. "reb_basic" for id 10), NOT a display
    # name. The client feeds this straight into SwitchMode(MT_NORMAL, name).
    # The start position is float[3] (Client.cpp uses character->GetPos()).
    p = struct.pack("<H", CMD_BC_STARTGAMEOK)
    p += struct.pack("<I", zone_inst_id)
    p += cstr(ip, 16)
    p += struct.pack("<I", port)
    p += cstr(map_name, MAP_NAME_LEN)
    p += struct.pack("<fff", pos[0], pos[1], pos[2])
    p += struct.pack("<B", channel)
    p += struct.pack("<B", 0)                 # continueMission
    return p


# --------------------------------------------------------------------------
# connection handling
# --------------------------------------------------------------------------
class Session:
    def __init__(self):
        self.account = None
        self.commander = None
        self.chars = []


def _continue_point(commander_id, picked_slots):
    r"""The saved position of the character to resume, or None.

    The original server keeps it per character: CCharacterSlot::m_continueZonePos
    is set from `character->point.x/y/z` when the slot is loaded
    (source2\\geServer\\BarrackServer\\CharacterSlot.cpp:126) and read back by
    CCommander::GetContGameZoneInfo (Commander.cpp:398).

    Ours lives in our own `CHARACTER_POSITION` table, written by the zone when
    the client moves (CZ_MOVE_PC carries the position AND the navmesh cell,
    so the cell is stored as-is). It is deliberately NOT the shipped
    `CHARACTERS.XPOS/YPOS/ZPOS`: those hold creation-time values and a
    z-HEIGHT, not a cell, so they cannot say where the player last stood.

    Returns the position of the FIRST picked slot that has one.
    """
    if not commander_id or not picked_slots:
        return None
    try:
        chars = ge_db.get_characters(commander_id)
    except Exception as exc:
        log("     !! could not read the characters: %s" % exc)
        return None
    by_slot = {c["slot"]: c for c in chars}
    for slot in picked_slots:
        c = by_slot.get(slot)
        if not c:
            continue
        pos = ge_db.get_character_position(c["character_id"])
        if pos:
            return (pos[0], pos[1], pos[2])
    return None


def handle_packet(sess, conn, pt):
    cmd, = struct.unpack_from("<H", pt, 0)
    if cmd == CMD_CB_LOGIN:
        info = parse_login(pt)
        log("     LOGIN id=%r version=%d nation=%s md5=%s"
            % (info["id"], info["version"], info["nation"], info["pw_md5"]))
        acc = ge_db.get_account(info["id"])
        if acc is None:
            log("     !! no such account %r in GE_DATA - using fallback commander 24"
                % info["id"])
            acc = {"account_id": 35, "commander_id": 24, "member_id": info["id"]}
        sess.account = acc
        pw = ge_db.get_password(acc["member_id"])
        if pw:
            log("     pw check: client=%s db=%s %s"
                % (info["pw_md5"], pw, "OK" if info["pw_md5"] == pw else "MISMATCH"))
        reply = build_loginok(acc["account_id"], 0x00003333, info["id"])
        conn.sendall(reply)
        log("SENT LOGINOK plaintext len=%d" % len(reply))
        # FULL REPLAY MODE: send the LIVE server's whole packet sequence and
        # nothing else. See FULL_REPLAY above.
        if FULL_REPLAY:
            for name in FULL_REPLAY:
                full = name if os.path.isabs(name) else replay_dir_for(name)
                if not os.path.exists(full):
                    log("!! replay file missing: %s" % full)
                    continue
                with open(full, "rb") as f:
                    pkt = f.read()
                conn.sendall(pkt)
                log("SENT FULL_REPLAY %s len=%d" % (os.path.basename(full), len(pkt)))
            return
        # The script-binding table. The live barrack sends it HERE, right after
        # LOGINOK, and we sent none of it (our barrack replied with 14 KB
        # against the live server's 86 KB). ROADMAP R7: the client is FLAT
        # during the ~57 s first-load stall, which is what a client WAITING
        # for a packet looks like.
        if "hotkey" not in SKIP:
            hk = build_hotkey_bind_info()
            if hk is not None:
                blob, count = hk
                conn.sendall(blob)
                log("SENT HOTKEY_BIND_INFO cmd=%d len=%d scripts=%d"
                    % (CMD_BC_HOTKEY_BIND_INFO, len(blob), count))
                for path in REPLAY_AFTER_HOTKEY:
                    full = path if os.path.isabs(path) else replay_dir_for(path)
                    if not os.path.exists(full):
                        log("!! replay file missing: %s" % full)
                        continue
                    with open(full, "rb") as f:
                        pkt = f.read()
                    conn.sendall(pkt)
                    log("SENT REPLAY %s len=%d" % (os.path.basename(full), len(pkt)))
        else:
            log("SKIP HOTKEY_BIND_INFO (GE_BARRACK_SKIP=hotkey)")

    elif cmd == CMD_CB_STARTBARRACK:
        if FULL_REPLAY:
            log("FULL_REPLAY mode: not sending our own COMMANDER_INFO")
            return
        cm = ge_db.get_commander(sess.account["commander_id"]) if sess.account else None
        if cm is None:
            log("     !! commander not found; falling back to 24")
            cm = ge_db.get_commander(24)
        sess.commander = cm
        cm["barrack_names"] = ge_db.get_barrack_names(cm["commander_id"])
        # Send the shipped-build job list (command 66) so the client's session
        # job vector is filled BEFORE we send BARRACK_INFO and it updates the
        # barrack UI (otherwise the UI search runs off an empty vector).
        #
        # HOW MANY TIMES, and WHERE: the live barrack sends this packet NINE
        # times, byte-identical, and it sends them BEFORE BC_COMMANDER_INFO.
        # That is not decoration. MEASURED 2026-10-21, with the script-binding
        # table (cmd 56) in play:
        #   * hotkey table + ONE job list, sent after COMMANDER_INFO
        #                                          -> the client CRASHES
        #                                            (0xc0000409, no minidump)
        #   * hotkey table + NINE job lists, sent after COMMANDER_INFO
        #                                          -> the client CRASHES
        #   * hotkey table + NINE job lists, sent BEFORE COMMANDER_INFO
        #                                          -> the client is fine
        #   * hotkey table + ONE job list, sent BEFORE COMMANDER_INFO
        #                                          -> the client CRASHES
        # So BOTH matter: the table must arrive NINE times AND it must arrive
        # before the commander info. The content does not matter - one list
        # carrying the live server's own ids still crashed.
        chars = load_chars(cm["commander_id"])
        sess.chars = chars
        if "job" not in SKIP:
            jobs = [c["job"] for c in chars]
            pkt = build_job_list(jobs)
            for _ in range(JOB_LIST_REPEATS):
                conn.sendall(pkt)
            log("SENT JOB_LIST cmd=%d x%d jobs=%s"
                % (CMD_BC_JOB_LIST, JOB_LIST_REPEATS, jobs))
        else:
            log("SKIP JOB_LIST (GE_BARRACK_SKIP=job)")
        if CM_BIN_FILE and os.path.exists(CM_BIN_FILE):
            with open(CM_BIN_FILE, "rb") as f:
                reply = f.read()
            log("COMMANDER_INFO: REPLAYING %s (%d bytes, reference packet, name=%r)"
                % (CM_BIN_FILE, len(reply),
                   reply[2:19].split(b"\0", 1)[0].decode("latin-1", "replace")))
        else:
            reply = build_commander_info(cm)
        conn.sendall(reply)
        log("SENT COMMANDER_INFO name=%r rank=%d point=%d len=%d"
            % (cm["name"], cm["rank"], cm["point"], len(reply)))
        # The "Lock" rights table. The client needs it before it builds the
        # character-creation job list (see build_update_table for why).
        if "lock" not in SKIP:
            upd = build_update_table()
            conn.sendall(upd)
            log("SENT UPDATE_TABLE idspace='Lock' len=%d" % len(upd))
        else:
            log("SKIP UPDATE_TABLE (GE_BARRACK_SKIP=lock)")

    elif cmd == CMD_CB_REQ_BARRACK_INFO:
        if FULL_REPLAY:
            log("FULL_REPLAY mode: not sending our own BARRACK_INFO")
            return
        chars = load_chars(sess.commander["commander_id"])
        sess.chars = chars
        conn.sendall(build_barrack_info(chars))
        if "extra" not in SKIP:
            extra = build_equip_extra(chars)
            conn.sendall(extra)
            log("SENT EQUIP_INFO_EXTRA len=%d nOptions=%d nSockets=%d nProps=%d"
                % (len(extra), *struct.unpack_from("<III", extra, 4)))
        if "chan" not in SKIP:
            ch_map = (sess.commander["zone_id"] if sess.commander else 0) & 0xFFFF
            conn.sendall(build_channel_list(ch_map, zone_channels(ch_map)))
        log("SENT BARRACK_INFO chars=%d (%s)%s"
            % (len(chars), ", ".join(c["name"] for c in chars[:6]),
               ("  [skipped: %s]" % ",".join(sorted(SKIP))) if SKIP else ""))

    elif cmd in (CMD_CB_STARTGAME, CMD_CB_CONTINUEGAME):
        # Body: WORD channelID; WORD charSlotID[0..2] (EMPTY_CHARSLOT for unused).
        # Remember the picked slots so the zone server can spawn exactly these.
        #
        # CB_CONTINUEGAME(23) has the SAME body as CB_STARTGAME(22) - the
        # original server routes both to the same "pick a team and go" code
        # (source2\geServer\BarrackServer\Client.cpp: `PACKET_CB_STARTGAME ->
        # OnStartNewGame`, `PACKET_CB_CONTINUEGAME -> OnContinueGame`).
        # The live client sends 23 when the player picks an EXISTING team and
        # 22 for a new one, so handling only 22 leaves the client sitting on
        # the character-select screen with no way in.
        raw = " ".join("%02X" % x for x in pt[:24])
        channel, = struct.unpack_from("<H", pt, STARTGAME_OFF_CHANNEL)
        slots = list(struct.unpack_from("<3H", pt, STARTGAME_OFF_SLOTS))
        picked = [s for s in slots if s != EMPTY_CHARSLOT]
        log("     %s raw=%s" % ("CONTINUEGAME" if cmd == CMD_CB_CONTINUEGAME
                                else "STARTGAME", raw))
        log("     %s channel=%d slots=%s picked=%s"
            % ("CONTINUEGAME" if cmd == CMD_CB_CONTINUEGAME else "STARTGAME",
               channel, slots, picked))
        try:
            cid = sess.commander["commander_id"] if sess.commander else None
            payload = {"channel": channel, "slots": slots, "picked": picked}
            # A CONTINUEGAME must resume at the character's SAVED position, not
            # at the map's default spawn. That position is SERVER-SIDE state
            # keyed by the character: the original server keeps it in
            # CCharacterSlot::m_continueZonePos, set from character->point.x/y/z
            # (source2\geServer\BarrackServer\CharacterSlot.cpp:126) and read
            # back by CCommander::GetContGameZoneInfo (Commander.cpp:398).
            # So we look it up here and hand it to the zone in the handoff.
            if cmd == CMD_CB_CONTINUEGAME:
                payload["continue"] = True
                saved = _continue_point(cid, picked)
                if saved is not None:
                    payload["continue_pos"] = list(saved)
                    log("     continue: saved point %s" % (saved,))
                else:
                    log("     continue: no saved point for the picked slots")
            # One file per player, so two players entering at the same time
            # cannot overwrite each other's team.
            path = team_file_for(cid)
            with open(path, "w", encoding="utf-8") as f:
                json.dump(payload, f)
            log("     wrote team handoff %s -> %s" % (path, picked))
            if path != TEAM_FILE:
                with open(TEAM_FILE, "w", encoding="utf-8") as f:
                    json.dump(payload, f)
        except OSError as exc:
            log("     !! could not write %s: %s" % (TEAM_FILE, exc))
        # Persist the pick: the barrack rebuilds its team from CHARACTERS.TEAMPOS
        # on the next login, so without this it falls back to whatever leader the
        # DB last had (e.g. after Move to Barrack it showed the old leader).
        try:
            cid = sess.commander["commander_id"] if sess.commander else None
            if cid:
                saved = ge_db.set_team_positions(cid, slots)
                log("     saved team positions in the DB -> %s" % saved)
        except Exception as exc:
            log("     !! could not save team positions: %s" % exc)
        zone_id = sess.commander["zone_id"] if sess.commander else 0
        # The address and the map name both come from the world module now.
        # The map name used to be the literal "reb_basic" for every player,
        # which meant a character saved in another map could never be entered
        # (the client loads the world the barrack names).
        ip, port = zone_target(zone_id)
        name = world.map_name(zone_id) or "reb_basic"
        conn.sendall(build_startgameok(ip=ip, port=port, map_name=name))
        log("SENT STARTGAMEOK zone=%s map=%s -> %s:%d"
            % (zone_id, name, ip, port))

    elif cmd == CMD_CB_ECHO:
        msg = pt[12:12 + 20]
        conn.sendall(struct.pack("<H", CMD_BC_ECHO) + msg)
        log("SENT ECHO reply")

    elif cmd == CMD_CB_REQ_CHANGE_BARRACKNAME:
        # body (offset 10): int barrackindex; char name[17]   -> 10 + 21 = 31
        raw = " ".join("%02X" % x for x in pt[:40])
        idx, = struct.unpack_from("<i", pt, BARRACKNAME_OFF_INDEX)
        nm = pt[BARRACKNAME_OFF_NAME:BARRACKNAME_OFF_NAME + CHAR_NAME_LEN] \
            .split(b"\0", 1)[0].decode("latin-1")
        cid = sess.commander["commander_id"] if sess.commander else 0
        log("     RENAME raw=%s" % raw)
        if not cid:
            log("     !! RENAME ignored - no commander")
        elif not (0 <= idx < MAX_BARRACK_CHANNEL) or not nm:
            log("     !! RENAME rejected: channel=%r name=%r (offset seems wrong - "
                "do NOT write)" % (idx, nm))
        else:
            ge_db.set_barrack_name(cid, idx, nm)
            # No reply exists for this one; the client updates its own button.
            log("     RENAME channel=%d name=%r -> saved to DB" % (idx, nm))

    elif cmd == CMD_CB_CREATE_CHAR:
        # body (offset 10): authCode[16]; WORD slotID; char name[17]; BYTE gender;
        #                   DWORD job; BYTE headType; cardType; invIndex; costumeID
        raw = " ".join("%02X" % x for x in pt[:72])
        slot, = struct.unpack_from("<H", pt, CREATE_CHAR_OFF_SLOT)
        name = pt[CREATE_CHAR_OFF_NAME:CREATE_CHAR_OFF_NAME + PC_INFO_NAME_LEN] \
            .split(b"\0", 1)[0].decode("latin-1")
        gender = pt[CREATE_CHAR_OFF_GENDER]
        job, = struct.unpack_from("<I", pt, CREATE_CHAR_OFF_JOB)
        headtype = pt[CREATE_CHAR_OFF_HEAD]
        log("     CREATE_CHAR raw=%s" % raw)
        log("     CREATE_CHAR slot=%d name=%r gender=%d job=%d head=%d"
            % (slot, name, gender, job, headtype))
        cid = sess.commander["commander_id"] if sess.commander else 0
        # Guard hard: a wrong offset here used to write garbage rows (slot 29537,
        # job 0) and the client crashed on the next login. Refuse anything that
        # does not look like a real character.
        bad = []
        if not cid:
            bad.append("no commander")
        if not (0 <= slot < MAX_SPC):
            bad.append("slot %d out of range 0..%d" % (slot, MAX_SPC - 1))
        if not name or len(name) > 16 or not name.isprintable():
            bad.append("name %r" % name)
        if not (0 < job < 1000000):
            bad.append("job %d" % job)
        if bad:
            log("     !! CREATE_CHAR REJECTED (offsets wrong?): %s" % "; ".join(bad))
        elif ge_db.slot_used(cid, slot):
            log("     !! slot %d is already taken - not inserting" % slot)
        else:
            newid = ge_db.create_character(cid, slot, name, gender, job, headtype)
            log("     INSERTED into CHARACTERS: CHARACTER_ID=%s" % newid)
            sess.chars = load_chars(cid)
        reply = build_create_charok({}, 1)
        conn.sendall(reply)
        log("SENT CREATE_CHAROK len=%d" % len(reply))

    elif cmd == CMD_CB_DESTROY_CHAR:
        # body (offset 10): authCode[16]; WORD slotID; char name[17] -> 10 + 35 = 45
        raw = " ".join("%02X" % x for x in pt[:56])
        slot, = struct.unpack_from("<H", pt, DESTROY_CHAR_OFF_SLOT)
        name = pt[DESTROY_CHAR_OFF_NAME:DESTROY_CHAR_OFF_NAME + PC_INFO_NAME_LEN] \
            .split(b"\0", 1)[0].decode("latin-1")
        cid = sess.commander["commander_id"] if sess.commander else 0
        log("     DESTROY_CHAR raw=%s" % raw)
        if not cid or not (0 <= slot < MAX_SPC):
            log("     !! DESTROY_CHAR rejected: slot=%s" % slot)
        else:
            gone = ge_db.delete_character(cid, slot)
            log("     DESTROY_CHAR slot=%d name=%r -> %d row(s) deleted"
                % (slot, name, gone))
            sess.chars = load_chars(cid)
        conn.sendall(build_destroy_charok())
        log("SENT DESTROY_CHAROK")

    else:
        log("     (unhandled command %d, len %d)" % (cmd, len(pt)))


def recv_packets(buf):
    """Yield complete decrypted packets from a wire buffer, return leftover."""
    out = []
    while len(buf) >= 2:
        block, = struct.unpack_from("<H", buf, 0)
        if len(buf) < 2 + block:
            break
        wire, buf = buf[:2 + block], buf[2 + block:]
        out.append(gc.decrypt_packet(wire))
    return out, buf


def serve_client(conn, addr):
    """Handle one client connection.

    The barrack used to run this inline in the accept loop, so it served ONE
    player at a time and a second login waited for the first to leave. Each
    connection now gets its own thread, and each connection has its own
    Session (account, commander, characters), so the two never share state.
    """
    conn.settimeout(IDLE_TIMEOUT)
    log("CONNECT from %s:%d" % addr)
    sess = Session()
    try:
        buf = b""
        while True:
            data = conn.recv(8192)
            if not data:
                log("CLOSE by client")
                break
            buf += data
            packets, buf = recv_packets(buf)
            for pt in packets:
                cmd, = struct.unpack_from("<H", pt, 0)
                log("RECV command=%d len=%d" % (cmd, len(pt)))
                handle_packet(sess, conn, pt)
    except socket.timeout:
        log("TIMEOUT - closing")
    except Exception as e:
        log("ERROR %r" % e)
    finally:
        conn.close()


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    # On Windows, SO_REUSEADDR lets a SECOND server bind the same port, and
    # then the two fight over the connections. SO_EXCLUSIVEADDRUSE makes the
    # second start fail loudly instead.
    if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    log("===== server start =====")
    log("UP on %s:%d (key=%s) - waiting for client" % (HOST, PORT, gc.KEY.decode()))
    if MAX_CHARS_DEBUG > 0:
        log("DEBUG: GE_BARRACK_MAX_CHARS=%d - reporting only %d character(s)"
            % (MAX_CHARS_DEBUG, MAX_CHARS_DEBUG))
    if SKIP:
        log("DEBUG: GE_BARRACK_SKIP=%s - omitting %s"
            % (",".join(sorted(SKIP)), ", ".join(sorted(SKIP))))
    if EMPTY_BARRACK:
        log("DEBUG: GE_BARRACK_EMPTY=1 - sending an EMPTY barrack (no characters)")
    if FAKE_CHAR:
        log("DEBUG: GE_BARRACK_FAKE_CHAR=1 - sending ONE synthetic character "
            "(neutral fields, no gear)")
    try:
        while True:
            conn, addr = srv.accept()
            # A thread per connection, so several players can be in the barrack
            # at once. Each connection owns its own socket, so two threads can
            # never interleave writes on one socket.
            t = threading.Thread(target=serve_client, args=(conn, addr),
                                 daemon=True)
            t.start()
    except KeyboardInterrupt:
        log("STOP by user")
    finally:
        srv.close()


if __name__ == "__main__":
    main()
