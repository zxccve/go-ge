"""zone_server - minimal zone server for the GE barrack hop.

The client, after BC_STARTGAMEOK, connects here (127.0.0.1:7002) and sends
CZ_START_GAME (cmd 3590, decrypted). We answer ZC_START_GAMEOK (cmd 3591) with
the selected team and keep the connection open, logging everything the client
sends next.

ZC_START_GAMEOK is a PKS_VARSIZE (server->client) packet. Its header is
**36 bytes** in the SHIPPED client (source2 headers say 28 - wrong). PROVEN by
disassembling the client's own handler, `CModeNet::StartGameOK`
(`FUN_004b6183` -> `FUN_004b5b29` returns `packet + 0x24`):

    +0   WORD  command
    +2   WORD  len
    +4   float tick
    +8   BYTE  permission
    +9   BYTE  connectionPos
    +10  char  occupyParty[17]
    +27  BYTE  count
    +28  DWORD serverTime   (client does `serverTime - GetTickCount()`)
    +32  DWORD reserved
    +36  count * START_INFO (+ stance/skill/option lists)

START_INFO is **485 bytes** (0x1e5) in the SHIPPED client - source2 says 364.
PROVEN by the inlined parser `CSession::SetMyPcInfo` (`FUN_005e645a`, reached
from `ExtractMyPcInfo` `FUN_005dde2a`): `memcpy(record, src, 0x1e5)` then it
reads stanceCount@0x1dd=477, skillCount@0x1df=479, optionCount@0x1e1=481, and
walks STANCE_INFO=14B / SKILL_INFO=10B / EQUIP_ITEM_OPTION=12B.

Layout (offsets into one record), each one cross-checked against the client's
own field getters/setters:
    0    OBJHANDLE handle                (FUN_005dfcd2 returns record+4 = name)
    4    char name[17]
    21   APPEARANCE_PC appearance (395B)  (job getter FUN_005dfcbd = record+0x27)
       +0   Name[17]
       +17  Gender
       +18  Job (DWORD; getter reads a dword, so bytes +19..+21 must be 0)
       +68  Equipment[21] 12-byte entries (equipment getter = record+0x59=89)
            (proof: crash FUN_007aa74b walks record+0x59, 12-byte stride, 21x)
    416  WORLDPOS initialPos (3 x long: x, y, cell)  (getter FUN_005dfce7)
    428  BASIC_STAT basicStat (49B)
       +0  BYTE LV          (getter FUN_005e057a case 2 reads byte @0xa64)
       +1  double HP        (FUN_005e0638 reads qword @0xa65)
       +9  double maxHP     (FUN_005e0656 reads qword @0xa6d)
       +17 WORD SP          (FUN_005e057a case 0 reads word @0xa75)
       +19 WORD maxSP       (FUN_005e057a case 1 reads word @0xa77)
       +21 int64 EXP        (case 3 reads 2 dwords @0xa79/@0xa7d)
       +29 int64 nextEXP    (case 8 reads 2 dwords @0xa81/@0xa85)
       +37 WORD STR,CON,DEX,INTE,AGI,CHA (FUN_005e63fe writes 6 words @0xa89..)
    477  WORD stanceCount
    479  WORD skillCount
    481  int  optionCount
    =485

NOTE: the shipped START_INFO has NO bmStat / battleStat / resistanceStat (unlike
source2). Those bytes are not there, which is why the total is 485 and not 532.

Run:  python zone_server.py            (spawns the team the barrack saved)
      python zone_server.py 2          (force the first N characters, for testing)
"""
import ctypes
import datetime
import json
import math
import os
import socket
import struct
import sys
import threading
import time

import ge_crypt
import ge_item_props
import ge_locale
import mission_data
import skill_data
import stance_data
import world
from ge_paths import CONFIG, LOGS, OUT, GAME_ROOT

HOST = "127.0.0.1"
PORT = 7002
HOLD_SECONDS = 300
# The log is written from several connection threads, so the writes are
# serialised.
LOG_LOCK = threading.Lock()

CMD_CZ_START_GAME = 3590
CMD_ZC_START_GAMEOK = 3591
CMD_CZ_GAME_READY = 3592
CMD_CZ_MOVE_PC = 3598
# Town quick-move. The client's "Warp (Alt+F9)" panel (ui/uixml/town.xml) has 4
# buttons; it draws their captions itself from datatable_town.ies and sends this
# on a press, so the only missing piece was the server side. Layout: 10 byte
# security header + int movetype (1..4) = 14. Id from the client's own table.
CMD_CZ_REQ_TOWN_MOVE = 3277
# Server -> client movement. `CNormalNet::MovePC` calls
# gemove::ActorMove(handle, spd, NULL, &dest, tick, reason) for ANY handle,
# including the local PC, so sending this back is what actually drives the
# character when the client is not moving it by itself.
CMD_ZC_MOVE_PC = 3548
CMD_ZC_STOP_PC = 3549
# Stance change. The client's packet table (cmd_table.txt) has
# CZ_CHANGE_STANCE = 3625 (size 15: 10B security header + BYTE charIndex + DWORD
# stanceType) and ZC_CHANGE_STANCE = 3554 (size 10: cmd + handle + stanceType).
CMD_CZ_CHANGE_STANCE = 3625
CMD_ZC_CHANGE_STANCE = 3554
# The in-game system menu: CZ_SYSMENU = 3595 (10B header + BYTE selection + bool)
# with SYSMENU_BARRACK = 0 (geZCTypes.h). The server answers with a 2-byte
# ZC_MOVE_BARRACK = 3597, which makes the client switch back to barrack mode.
CMD_CZ_SYSMENU = 3595
CMD_ZC_MOVE_BARRACK = 3597
CMD_ZC_LOGOUT_OK = 3784
SYSMENU_BARRACK = 0
MOVE_SPEED_DEFAULT = 415.0    # CFieldChar default m_moveSpeed (FieldChar.cpp:67)
# The stance the leader is actually in. START_INFO carries the character's real
# stance list now, so the client picks the first stance of the first job condition
# matching the equipped weapons (GetStanceByCondition) - load_players runs the same
# rule server-side. Until then this is the client's own empty-list fallback.
START_STANCE = int(os.environ.get("GE_STANCE_ID", stance_data.GM_STANCE))
FORCE_STANCE = "GE_STANCE_ID" in os.environ
MOVE_SPEED_OVERRIDE = float(os.environ.get("GE_MOVE_SPEED", "0") or 0)
# Send the character's real STANCE_INFO list in START_INFO. Set GE_SEND_STANCES=0
# to fall back to the old behaviour (zero stances = the client's GM_STANCE), e.g.
# if a stance list ever upsets the client.
SEND_STANCES = os.environ.get("GE_SEND_STANCES", "1") != "0"
# Same idea for the SKILL_INFO records after the stance block (the skills a
# character's stances refer to). GE_SEND_SKILLS=0 sends none.
SEND_SKILLS = os.environ.get("GE_SEND_SKILLS", "1") != "0"
# Stance experience tables, filled from the DB in load_players. They only decide
# the level/skill points inside each STANCE_INFO record.
STANCE_XP_TYPES = {}
STANCE_LEVELS = {}
# MOVE_REASON (source2 geShared/include/geShared.h):
#   MR_MOVE, MR_ATTACK, MR_SKILL, MR_PICK, MR_DIALOG
# The reason says what the movement IS. The original picks it from the
# character's action state (PlayerChar.cpp BroadcastStartMovement): the state
# that chases a clicked NPC (AS_TRIGGER_CHASE) sends MR_DIALOG.
MR_MOVE = 0
MR_ATTACK = 1
MR_SKILL = 2
MR_PICK = 3
MR_DIALOG = 4
# After CZ_GAME_READY the client holds its loading screen until we send this
# (source2 CCommander::OnGameReady does exactly this). Id read from the shipped
# client's own packet table: 3149, fixed size 2 (command only).
CMD_ZC_ENTER_LEGACY_OBJECTS = 3149
# The list packets that UNLOCK what START_INFO carried. Every entry the client
# builds from START_INFO starts `isLocked = true`, and the stance/skill window
# only draws unlocked entries (UISkillListBox.cpp:170), so these must follow or
# the window stays empty. Ids read from the shipped client's table; both are
# variable length (cmd+len header).
CMD_ZC_SKILL_LIST = 3621
CMD_ZC_STANCE_LIST = 3626
# Post-entry sequence (source2 Commander_FSM.cpp SS_READY order:
# SendStartGameOK, SendCommanderLockInfo, SendCommanderPoint, SendQuestInfo,
# SendInventoryItemList, ...).
# Ids read from the shipped client's own packet table (dump_zone.py), structs
# from source2 geShared/include/geZCPacket.h (only used for SIZES - ids always
# from the client):
# ZC_COMMANDER_POINT=3744 size 6, ZC_UPDATE_TABLE=3747 varsize,
# ZC_INVENTORY_ITEM_LIST=3601, ZC_INVENTORY_ITEM_OPTION_LIST=3603,
# ZC_INVENTORY_ITEM_SOCKET_LIST=3604, ZC_INVENTORY_ITEM_PROPERTY_LIST=3606,
# ZC_REINFORCE_INFO_LIST=3855, ZC_CHANGE_INV_MAXWEIGHT=3143 size 6,
# ZC_QUEST_INFO=3872, ZC_QUEST_STAGE_INFO=3874,
# ZC_COMMWARP_LIST=3634, ZC_COMMANDER_PET_LIST=3199.
# PKS_ZC_COMMANDER_POINT = PKS_DEFAULT(command 2) + DWORD point = 6.
# PKS_ZC_UPDATE_TABLE = PKS_VARSIZE(command 2 + len 2) + type[20] + xml + NUL.
# Empty inventory-family lists = cmd(2)+len(2)+count(4)+type(4) = 12
# (REINFORCE = cmd+len+type(4)+count(4) = 12). ILT_INVENTORY = 0.
CMD_ZC_COMMANDER_POINT = 3744
CMD_ZC_UPDATE_TABLE = 3747
CMD_ZC_INVENTORY_ITEM_LIST = 3601
CMD_ZC_INVENTORY_ITEM_OPTION_LIST = 3603
CMD_ZC_INVENTORY_ITEM_SOCKET_LIST = 3604
CMD_ZC_INVENTORY_ITEM_PROPERTY_LIST = 3606
CMD_ZC_REINFORCE_INFO_LIST = 3855
CMD_ZC_CHANGE_INV_MAXWEIGHT = 3143
# ZC_UPDATE_PROPERTY_NUMBER: id 3748, client table size 46 = 2 + idSpace[20] +
# property[20] + value(4). FIX packets carry NO length field - its sibling
# ZC_UPDATE_PROPERTY_STRING is registered 74 = 2+20+20+32, which only adds up
# without one. The client applies this to its IES cache and redraws the window
# named by the property (Lock_InventoryCount -> "inventory"), so a script that
# changes a Lock_* property must send it or the UI keeps the old value.
CMD_ZC_UPDATE_PROPERTY_NUMBER = 3748
CMD_ZC_QUEST_INFO = 3872
CMD_ZC_QUEST_STAGE_INFO = 3874
# The mission LIST. A mission here is an Info Center row, not a map: 269 of them
# in `datatable_missionlist`. Both ids register with size 0 (variable), and the
# reply's layout comes from the client's own handler FUN_004ca1e6.
CMD_CZ_REQ_MISSION_LIST_STATUS = 3334
CMD_ZC_RES_MISSION_LIST_STATUS = 3335
# The mission LOBBY. A window that never opens has a script BUILTIN as its entry
# point: `ShowLobby(pc, type, layer)` (393 call sites across 89 click scripts) ->
# `OM_SHOW_LOBBY` -> `CCommander::ShowLobby` (Commander.cpp:661) -> the world's
# room list. The reply below is what makes the client call
# `ui::GetObject("missionlobby")` + `ShowWindow(true)` (handler FUN_004e6ef6).
CMD_ZC_LOBBY_ROOM_LIST = 3705
CMD_CZ_LOBBY_ROOM_LIST = 3710
# Pressing "Create a Room" in the lobby: 3709 asks for the create dialog and 3711
# opens it. `preset` is NOT free text - it is a ClassName in the `MissionPreset`
# table (971 rows, e.g. `mcrafflesia`), and the client does
# `GetClass("MissionPreset", preset)` then returns EARLY when it is unknown.
CMD_CZ_LOBBY_CLICK_CREATE_ROOM = 3709
CMD_ZC_LOBBY_CREATEUI_OPEN = 3711
# Creating the room itself, and the room view the client enters afterwards.
CMD_CZ_LOBBY_CREATE_ROOM = 3708
CMD_ZC_ROOM_INFO = 3712
# Entering a room: 3706 -> 3707 (the room view) + the slot grid and its count.
CMD_CZ_LOBBY_JOIN_ROOM = 3706
CMD_ZC_LOBBY_JOIN_ROOM = 3707
CMD_ZC_ROOM_SLOT_LIST = 3725
CMD_ZC_ROOM_COUNT = 3717
# Leaving: 3719 (carries "also send me the room list"), 3720 tells the room you
# left, 3726 closes the room window with a reason (1 = the master left).
CMD_CZ_ROOM_EXIT = 3719
CMD_ZC_ROOM_EXIT = 3720
CMD_ZC_ROOM_OUT = 3726
# Live sends this on every join/create (17 times in one capture) and we never
# did. `RoomJoin` itself only chats the name, but it is part of the sequence the
# client expects while it is in a room.
CMD_ZC_ROOM_JOIN = 3718
# Mission start: the press (3714) and the "everyone ready" broadcast (3716).
CMD_CZ_ROOM_START_MISSION = 3714
CMD_ZC_ROOM_ALL_READY = 3716
ROOM_LEN = 68
SLOT_LEN = 27
ROOM_SLOTS = 8
ROOMS = {}
_room_seq = [0]
CMD_ZC_COMMWARP_LIST = 3634
CMD_ZC_INVENTORY_CASHITEM_LIST = 3097
CMD_ZC_EQUIP_CASHITEM_LIST = 3099
CMD_ZC_COMMANDER_INIT_INFO = 3184
CMD_ZC_LUMIN_CASHINFO_LIST = 3344
CMD_ZC_EQUIP_LUMIN_CASHINFO_LIST = 3347
CMD_ZC_COMMANDER_PET_LIST = 3199
ILT_INVENTORY = 0
# Item properties are OFF by default. The shape is now believed correct (the
# object carries the original's own class, `Item_Property`/1 - see
# build_inventory_property_object) but it has NOT been tried live since the
# 2026-10-19 crash. Opt in with GE_ITEM_PROPERTIES=1.
SEND_ITEM_PROPERTIES = os.environ.get("GE_ITEM_PROPERTIES", "0") == "1"
# Town NPCs ("trigger objects"). Id/size from the shipped client's own table;
# layout from its handler FUN_004f35a3 / FUN_004f424e (see build_spawn_trigger).
CMD_ZC_SPAWN_STANDINGTRIGGER = 3543
# ZC_SPAWN_STANDINGPC - the packet that tells a client about ANOTHER player.
# The layout was recovered from the CLIENT's own handler (FUN_004f3fbb) and
# cross-checked against the v28 server's sender (CPlayerChar::SendSpawned).
# The two agree byte for byte on the first 30 bytes; the client then reads a
# bigger middle section, and the appearance lands exactly on the end of the
# 395-byte APPEARANCE_PC we already know from START_INFO (306 + 395 = 701 =
# the equip-option count), which is the proof that the offsets are right.
CMD_ZC_SPAWN_STANDINGPC = 3520
CMD_ZC_LEAVE_PC = 3525
SP_PC_TOTAL = 705          # the whole packet, with no equip options
SP_PC_HANDLE = 4
SP_PC_DIR = 8
SP_PC_CLASS = 9           # the character's ClassID (word)
SP_PC_WCLASS = 11         # the weapon's ClassID (word)
SP_PC_POS = 17            # x, y, cell
SP_PC_STANCE = 30         # the stance the character is in
SP_PC_HP = 34             # double
SP_PC_MAXHP = 42          # double
SP_PC_BUFFMASK = 50       # 256 bytes, and it must be ZERO
SP_PC_APPEARANCE = 306    # 395 bytes
SP_PC_APPEARANCE_LEN = 395
# Inside the appearance, the client reads a STRING at +341 and uses it as the
# other player's name (it skips the nameplate when the string is empty). That
# is why the other player had no name over their head.
SP_PC_APPEARANCE_NAME = 341
SP_PC_APPEARANCE_NAME_LEN = 17
SP_PC_OPTIONS = 701       # the equip-option count
# One equip option is 12 bytes: {DWORD type; DWORD value; DWORD spot}.
SP_PC_OPTION_SIZE = 12
# The POSTURE (the animation state) is a DWORD. The v28 server mapped the
# game's internal posture enum through a small switch: 0x1f->1, 0x20->2,
# 0x21->3, 0x22->5, and EVERYTHING ELSE -> 0. So 0 is not "standing", it is
# "unknown", and the client falls back to an arbitrary animation - which is
# how the other player ended up running on the spot. 1 is the first mapped
# value, and the first posture of the enum: standing.
SP_PC_POSTURE = 13
# The posture (the animation state) is a DWORD, and the v28 server mapped the
# game's internal posture enum through a small switch: 0x1f->1, 0x20->2,
# 0x21->3, 0x22->5, and EVERYTHING ELSE -> 0. So 0 is not "standing", it is
# "unknown", and the client falls back to an arbitrary animation.
# Which of 1/2/3/5 is "standing" has not been pinned down from the client, so
# it is a switch: try GE_PC_POSTURE=2 (or 3, or 5) and see which one makes the
# other player stand still.
POSTURE_STANDING = int(os.environ.get("GE_PC_POSTURE", "1") or 1)
# ZC_LEAVE_PC: cmd(2) + handle(4) + reason(2) = 8 bytes (the client reads the
# handle at +2 and a word at +6).
LEAVE_PC_REASON = 3
CMD_ZC_ENTER_STANDINGTRIGGER = 3544
CMD_CZ_LEGACY_OBJECTS_ENTERED = 3150
# Clicking a town NPC. The client only sends this for a trigger whose row in
# `datatable_npc.ies` has Click="TRUE" (CNormalView::OnLBtnDownFocusedMonster
# checks pTgtObj->GetDialogOnClick() != MON_CLICK_NO before sending). 1593 of
# the table's 1751 rows are TRUE. pack(1) layout: 10 byte security header
# + BYTE charIndex + BYTE longdistance + DWORD trigger handle = 16.
CMD_CZ_CLICK_TRIGGER = 3679# The reply that opens the standard dialog box. PKS_ZC_DIALOG_OK is a VAR-size
# packet: cmd(2) + len(2) + BYTE type + DWORD handle + text. `len` is the WHOLE
# packet size (like the other VAR packets) and the client reads the text from
# `sizeof(PKS_ZC_DIALOG_OK)` = 9 - gePacketBase.h puts every packet struct in
# `#pragma pack(1)`, so there is no padding to allow for. type 1 =
# WITH_FADE_IN, and `handle` = the clicked NPC: the client fades the camera to
# whatever object that handle names.
CMD_ZC_DIALOG_OK = 3660
# The CHOICE dialog: server -> client 3663 (var-size), client -> server 3657
# (12 bytes = 10 header + charIndex + select). `data\cmd_table.txt`.
CMD_ZC_DIALOG_SELECT = 3663
CMD_CZ_DIALOG_SELECT = 3657
# The NPC shop window. Id read from the SHIPPED client's own packet table (its
# names are in id order around here: 3679 CZ_CLICK_TRIGGER, 3680
# ZC_DIALOG_NPC_TRADE, 3681 ZC_DIALOG_NPC_CUSTOM_TRADE), size 0 = var-size.
CMD_ZC_DIALOG_NPC_TRADE = 3680
CMD_ZC_DIALOG_NPC_CUSTOM_TRADE = 3681
CUSTOM_SHOP_NORMAL = 0
# SHOP_SELLCOIN_TYPE (geShared/include/geShared.h:49): SHOP_VIS = 0, SHOP_FESO = 1.
# What a normal shop uses; the client uses it to price the items it is sent.
SHOP_VIS = 0
# SHOP_TYPE (geZCTypes.h): NORMAL=0, CUSTOM=1, WORLDPVP=2, PROPERTY=3, CUSTOM_DB=4.
# A custom shop charges the (price item, count) pair the SERVER put in the
# dialog, so its CZ_BUYITEM carries this type and NO vis is involved.
SHOP_TYPE_CUSTOM = 1
DLG_WITH_FADE_IN = 1
# The dialog above is MODAL: WITH_FADE_IN makes the client fade the camera onto
# the NPC, hide the UI and hand control to the dialog (CNormalMode::DialogBegin
# -> SUBMODE_DIALOG). Nothing local leaves that state - pressing OK only sends
# CZ_ACK_DIALOG (3656, 10B header + BYTE charIndex + bool next = 12) and the
# client waits for us. The reply that gets the player back is ZC_DIALOG_CLOSE
# (3665, command only), whose handler is exactly
# `CNormalMode::DialogEnd()` -> SUBMODE_FADE_TO_GAME -> normal camera + UI.
# Without it the client stays focused on the NPC forever.
CMD_CZ_ACK_DIALOG = 3656
CMD_ZC_DIALOG_CLOSE = 3665
# ZC_CHAT: a character SPEAKS. `UseBalloon` is what makes the client draw the
# speech balloon over the actor (`geutil::Chat`), which is how an NPC talks
# without a dialog. The shipped server sends it with UseBalloon = true for
# `OM_CHAT` (FieldChar.cpp:1010); the balloon-specific packet (3553) belongs to
# the `BalloonChat` builtin instead.
CMD_ZC_CHAT = 3552
CPL_USER = 0
# ZC_PLAY_NPC_CLICK_VOICE: the NPC's "click" voice. The live server sends it when
# the player clicks an NPC (capture `liveclick4`, 2026-10-21): a 6-byte packet
# holding the CLICKED NPC's handle. Id and size from the client's own packet
# table: 3515, fixed 6 bytes.
CMD_ZC_PLAY_NPC_CLICK_VOICE = 3515
# CZ_BUYITEM: 12-byte security header + DWORD shopType + DWORD record count, then
# 12-byte BUY_ITEM records. MEASURED against a working server: the record's first
# DWORD is a client-side pointer (junk on the wire); the QUANTITY is a WORD at +4
# and the shop-list INDEX is the DWORD at +8. The original uses the index
# (geShopTable::GetItemByIndex) and prices from the item row, never the packet.
# CZ_BUYITEM: 12-byte security header + DWORD shopType + DWORD count + BYTE flag,
# then `count` records of 12 bytes {DWORD type (client pointer - IGNORE); DWORD
# count; DWORD index}. The single flag byte is what made a 13-byte per-record read
# work for ONE item (len 33) and fall apart for six (len 93 = 20 + 1 + 6*12).
# CZ_SELLITEM: same shape as CZ_BUYITEM. 12-byte security header + DWORD count +
# ONE flag byte, then `count` * 8 bytes {DWORD invIndex; DWORD count}. The
# reference capture showed a 32-byte packet for ONE record: 12 + 4 + 1 + 8 = 25
# bytes of real data, padded by Blowfish to 32. The old "9-byte record" reading
# is the same trap the BUY packet had (there the byte-before-records was proved
# per-PACKET by a 4-item cart, and SELL_ITEM in the shared header is 8 bytes:
# {DWORD invIndex; DWORD count}). invIndex is 1..N as sent in
# ZC_INVENTORY_ITEM_LIST, so it indexes CUR.last_inv, not the DB.
CMD_CZ_BUYITEM = 3672
CMD_CZ_SELLITEM = 3673
# PKS_CZ_USEITEM_TARGET (0xe43). Used by double-click / use on a target.
# Layout measured from the live client (sender FUN_00506eb1, confirmed against
# captured bytes; offsets include the 4-byte frame header):
#   +0  WORD  cmd (0xe43)
#   +2  WORD  len
#   +4  WORD  0
#   +6  DWORD tick (changes every use)
#   +0xa BYTE kind
#   +0xb DWORD invIndex   <- 1..N, indexes CUR.last_inv like the buy/sell packets
#   +0xf DWORD (constant per target)
#   +0x13 BYTE flag
CMD_CZ_USEITEM_TARGET = 3651
# ZC_REMOVEITEM (client table size 15 = cmd + 13) is what makes an OPEN shop
# window refresh its "my items" counts: its handler ends with
# `UISession()->UpdateTradeInfo()`, while a bulk ZC_INVENTORY_ITEM_LIST does not.
# `count` is the REMAINING total, not the amount taken (GENormalItem.cpp
# RemoveItem -> ChangeItemCount(invIndex, itemCnt)). Source2 and the shipped
# table agree on the size, so the layout is the plain one:
#   cmd(2) invIndex(4) count(4) removeType(1) autoRule(4)
CMD_ZC_REMOVEITEM = 3608
# The enum starts with an UNNAMED 0 (geZCTypes.h), so SELL is 1, not 0, and
# CUSTOM_SHOP is 16 (MARKET_BUY is 15 - sending that was a silent no-op).
REMOVE_ITEM = 0
REMOVE_ITEM_SELL = 1
REMOVE_ITEM_BUY = 2
REMOVE_ITEM_CUSTOM_SHOP = 16
AUTORULE_NONE = 0               # decoded to 170; the client drops out-of-range
# ZC_ADDITEM (3607, 44 bytes). Layout proven from the SHIPPED handler
# (decompiled CNormalNet::AddItem: expireTime at +0x1a, WaitSecond at +0x27):
#   cmd(2) len(2) ItemType(4) InvIndex(4) Count(4) PType(1) GUID(8) addType(1)
#   expireTime(4) optionCount(4) socketCount(4) isReinforce(1) WaitSecond(1)
#   autoRule(4)
# `count` is the NEW TOTAL of that stack (the reference server sends 4 after a
# buy of 3 into a stack of 1).
CMD_ZC_ADDITEM = 3607
ADD_ITEM = 0
ADD_ITEM_BUY = 1
ADD_ITEM_SELL = 2
ADD_ITEM_CUSTOM_SHOP_BUY = 12
# ZC_MESSAGE (3596) is how the ORIGINAL server says things the client will not
# print itself, e.g. the money moved by a purchase or sale. type/level 0 make
# the client call NormalUI()->SysMsg(text) (GEPacket.cpp packet::ServerMsg), the
# plain system line; other levels pop modal boxes.
CMD_ZC_MESSAGE = 3596
MESSAGE_TYPE_GENERAL = 0
MESSAGE_LEVEL_GENERAL = 0
BUY_HEADER = 21                 # 12 security + shopType + count + the flag byte
BUY_ITEM_SIZE = 12
SELL_HEADER = 17                # 12 security + count + the flag byte
SELL_ITEM_SIZE = 8
VIS_CLASS = 30000               # g_visItem; the sell path must never sell money
# How close the player must be for a click to be answered. The click packet is
# NOT the thing that decides this: in the real server CPlayerChar::
# OnNotifyClickTrigger only sets action state AS_TRIGGER_CHASE, the player's FSM
# walks them to the NPC, and the dialog runs when they arrive. The test it uses
# is CFieldObject::IsNearObject, `distance(pc, npc) - (pcRadius + npcRadius)
# <= range`, and every number in it is hardcoded, not read from a table:
#   * radius = the "domain" radius from geShapeTable (ShapeTable.cpp):
#     S=50, M=100, L=200, XL=400 (CFieldChar::GetDomainRadius returns the shape's
#     own size, m_shapeSize). A player is always Size "S", and 1748 of the
#     table's 1751 NPC rows are Size "S" too, so 50 + 50.
#   * range = FieldObject.h DISTANCE_TO_CHAR = 100, x5 for a "longdistance" click.
# The FSM keeps TWO of these numbers apart, and mixing them up is easy:
#   m_npcClickDistance = 100 (or 500) is passed to IsNearObject,
#   which folds the radii in -> 200 between CENTRES (600 long),
#   and `m_npcClickDistance - 10` is passed to GetDomainOuterPos, which then
#   ADDS a radius itself -> a chase stops 50 + 90 = 140 from the NPC centre.
NPCD_RADIUS = {"S": 50, "M": 100, "L": 200, "XL": 400}   # geShapeTable
NPC_CLICK_DISTANCE = 100                     # FieldObject.h DISTANCE_TO_CHAR
NPC_CLICK_DISTANCE_LONG = NPC_CLICK_DISTANCE * 5
# What those become between CENTRES, with both 50-unit "S" radii folded in.
NPC_CLICK_RANGE = NPC_CLICK_DISTANCE + 2 * NPCD_RADIUS["S"]              # 200
NPC_CLICK_RANGE_LONG = NPC_CLICK_DISTANCE_LONG + 2 * NPCD_RADIUS["S"]    # 600
# PKS_CZ_CLICK_TRIGGER: the trigger handle sits after the 10 byte header, the
# charIndex and the bool longdistance.
TR_CLICK_HANDLE = 12
# A clicked NPC is REMEMBERED until the player arrives, because the click is not
# answered on the spot: the original sets AS_TRIGGER_CHASE, the FSM walks the PC
# to the NPC, and the trigger script runs on arrival. The CLIENT does NOT walk to
# a clicked trigger - proved by the log on 2026-10-16, a CZ_CLICK_TRIGGER arrives
# with no CZ_MOVE_PC of its own - so the SERVER has to walk it, exactly like
# AS_TRIGGER_CHASE setting MS_MOVE. A click whose walk never brings the player
# into range (unreachable NPC, or the player walked off somewhere else) is
# dropped after CLICK_STALL_MS, so nothing is remembered forever.
CLICK_STALL_MS = 2000
# How much farther from the clicked NPC a client-reported walk must END before
# it counts as "the player clicked the ground" (which cancels the click) rather
# than one of the client's own path steps toward the NPC (which keeps it). Path
# steps can swing wide around a corner, so it needs a margin.
GROUND_CANCEL_MARGIN = 100
# The chase is server-driven, so bound it: a click may spend at most this many
# walk packets, and they may follow each other no faster than CHASE_RETRY_MS (a
# zero-length walk ends in the same tick, which would otherwise loop forever).
CHASE_TRIES = 8
CHASE_RETRY_MS = 1000
# A chase packet RE-TARGETS the actor, so re-sending one while the character is
# already walking to the NPC makes it stutter and look uncontrollable. Re-chase
# only when the walk has stalled: never sooner than this after the previous
# chase, and not at all while the distance to the NPC is still improving by more
# than CHASE_PROGRESS units since that chase.
CHASE_COOLDOWN_MS = 2500
CHASE_PROGRESS = 50
# How long a chase pose may go without the client reporting a single step before
# we treat it as refused. The client reports its own progress about once a second
# while it walks, so two seconds of silence after we asked for a walk means it is
# not walking. The fallback is the target's own position (the shape the live
# server sends), which the client will always accept even though the actor then
# overlaps the NPC.
CHASE_REFUSED_MS = 2000
# Longest straight leg handed to the client in one ZC_MOVE_PC. The client walks a
# server move as one straight line and does not follow the route we computed, so
# a long route is delivered as consecutive legs of at most this length. ~700 is
# also the step size the client reports for its own walks.
# GE_CHASE_HOP=0 (the default) sends the whole route as ONE move, which is what
# the live server does (captured 2026-10-22: a click produces ONE ZC_MOVE_PC to
# the NPC's own position). A positive value splits the route into legs of that
# length, which is janky (the actor stops between legs) but survives a route the
# client refuses to walk as a whole. Kept as an escape hatch for such a target.
CHASE_HOP = int(os.environ.get("GE_CHASE_HOP", "0") or 0)
# Step used to back the chase destination away from the NPC when the ideal
# point is not walkable for a player-sized body (see chase_ring_distances).
CHASE_RING_STEP = 10
# How long a chase move may go unanswered before we decide the client is not
# translating the actor and the walk is dead. Only a CZ_MOVE_PC from the client
# proves it is really moving; the client's own AI reports its steps about a
# second after the click, and a redirect needs a moment longer, so allow a few.
CHASE_WALK_MS = 8000
# Short recv() timeout. A timeout is the "update tick" that answers a remembered
# click when the walk ends - no packet arrives at that exact moment. The session
# still ends after HOLD_SECONDS of silence.
POLL_SECONDS = 0.2
# Crash bisect (2026-10-16): full 209-item list crashed the client at
# VA 0x6da12a (item lookup). All 193 class ids exist in the disk XML, so the
# suspects are money (30000/30001, special UI) and high ids. Excluded for now;
# reintroduce in small groups once entry is clean.
INV_EXCLUDE = frozenset([
    30000, 30001,
    125330, 125338, 157034, 171013, 171014, 250036,
    189003, 189011, 189012, 189013, 189016, 189021, 189024, 189027, 189030,
    189092, 189112, 189235, 189288,
])
# Bisect step 2: send only the first N items. If a tiny list still crashes,
# the RECORD LAYOUT is wrong, not the data. None = no limit.
# (Result: 5 items crashed too -> layout was wrong: records are 24 bytes,
# see build_inventory_item_list. Limit off for the full-list retest.)
INV_LIMIT = None
MAX_TELEPORT_TYPE = 4           # geShared.h
DEFAULT_MAX_WEIGHT = 50000      # Lock_WeightLimit default (datatable_commanderlock.xml)

# ---------------------------------------------------------------------------
# SPAWN POSITION (this is subtle - read the whole comment)
# ---------------------------------------------------------------------------
# START_INFO.initialPos is a WORLDPOS{x, y, cell}. The DB stores the character
# position as a 3D point (XPOS,YPOS,ZPOS = x, y, z-HEIGHT); the real server turns
# that point into a WORLDPOS with the map's path mesh
# (CWorld::Get3DPointToPosLocaly -> CGround::GetCell). `cell` is a navmesh cell
# INDEX, not a coordinate and not the height. If we send the DB z as the cell, the
# client loads the WRONG terrain region and the actor is placed with the wrong
# ground height -> character under the map, black/broken ground.
#
# The navmesh itself IS available: the map's `.tok` mesh (shipped with the
# client) plus its own pathengine.dll answer the query through
# `navmesh/pathtool.exe` (32-bit; see npc_data.py). The cell below was harvested
# from the client's own CZ_MOVE_PC before that worked; it is now only a fallback
# for maps with no mesh on disk. Override at runtime with `zone_spawn.txt`
# ("x y cell").
MAP_SPAWN = {
    10: (5779, 303, 149),     # reb_basic / Cite de Reboldoeux
}
SPAWN_FILE = os.path.join(CONFIG, "zone_spawn.txt")

# ---------------------------------------------------------------------------
# TOWN NPCs
# ---------------------------------------------------------------------------
# npcgen_<map>.ies places each town NPC at a 3D point (PosX/Y/Z), but the spawn
# packet wants WORLDPOS{x, y, cell}: the cell picks a navmesh triangle and the
# client reads the ground height from it, so a wrong cell drops the NPC through
# the map. npc_data.py resolves the points with the CLIENT'S OWN path engine
# (the same call the real server makes in CWorld::InitTriggerObject), and maps
# the npcgen `ClassType` string to the client's "NPC" table ClassID.
#
# npcgen rows marked Hide="TRUE" are skipped, like the real server does.
# Which table/mesh a zone needs comes from `datatable_map.ies`, so no per-map
# list is needed here - see npc_data.zone_npcs.
NPC_HANDLE_BASE = 0x40000000
_npc_cache = {}


def zone_npcs(zone_id):
    """Per-NPC dicts for a zone (see npc_data.zone_npcs); [] if unknown."""
    if zone_id not in _npc_cache:
        npcs = []
        try:
            import npc_data
            npcs = npc_data.zone_npcs(zone_id)
            log("  town NPCs zone %s: %d resolved" % (zone_id, len(npcs)))
        except Exception as exc:
            log("  !! could not load town NPCs for zone %s: %s" % (zone_id, exc))
        _npc_cache[zone_id] = npcs
    return _npc_cache[zone_id]


_town_cache = {}


def town_move(zone_id, index):
    """WORLDPOS{x, y, cell} for the client's town button `index`; None if none."""
    key = (zone_id, index)
    if key not in _town_cache:
        pos = None
        try:
            import npc_data
            pos = npc_data.town_move(zone_id, index)
        except Exception as exc:
            log("  !! town move lookup failed: %s" % exc)
        _town_cache[key] = pos
    return _town_cache[key]

PARTY_NAME_LEN = 17
CHAR_NAME_LEN = 17
COMMANDER_NAME_LEN = 17
EQUIP_SPOT_LAST = 21
EQUIP_ENTRY_STRIDE = 12

# ---- ZC_START_GAMEOK header (36 bytes; see module docstring) ----
HDR_TICK = 4
HDR_PERMISSION = 8
HDR_CONNECTION_POS = 9
HDR_OCCUPY_PARTY = 10
HDR_COUNT = 27
HDR_SERVER_TIME = 28
HDR_RESERVED = 32
HDR_SIZE = 36

# ---- START_INFO (485 bytes) ----
SI_SIZE = 485
SI_HANDLE = 0
SI_NAME = 4

SI_APPEARANCE = 21
AP_NAME = 0
AP_GENDER = 17
AP_JOB = 18
AP_HEADTYPE = 22
AP_EQUIPMENT = 68

SI_INITPOS = 416              # x, y, cell (3 x long)

SI_BASICSTAT = 428
BS_LV = 0
BS_HP = 1                     # double
BS_MAXHP = 9                  # double
BS_SP = 17                    # WORD
BS_MAXSP = 19                 # WORD
BS_EXP = 21                   # int64
BS_NEXTEXP = 29               # int64
BS_STATS = 37                 # STR,CON,DEX,INTE,AGI,CHA (6 x WORD)

SI_STANCECOUNT = 477
SI_SKILLCOUNT = 479
SI_OPTIONCOUNT = 481

TEAM_FILE = os.path.join(OUT, "zone_team.json")

# Equipment "None" class ids, same list the barrack uses. (The zone's appearance
# DOES need the real ids: a 0 there is an invalid class, and the client can look
# the model up to nothing.)
try:
    from ge_barrack_server import EQUIP_NONE
except Exception:
    EQUIP_NONE = [0] * EQUIP_SPOT_LAST
# The weapon hands: the slots whose "nothing equipped" class is 10000 in the
# barrack's table - slots 4 and 5 (left hand, right hand). The WEARITEM rows
# use the same numbering. Spot 0 is the HEAD, not a weapon.
EQUIP_WEAPON_SPOTS = (4, 5)


def cstr(s, n):
    b = s.encode("latin-1", "replace")[:n - 1]
    return b + b"\0" * (n - len(b))


# Class base stats, one entry per job id, extracted from the client's own
# datatable_job.ies (data\job_stats.json; 413 classes). The old save stores no
# allocated points, so the class base is everything we have - confirmed against
# Grandies (job 72, CON 90) where the HP formula below reproduces the DB value
# exactly.
try:
    _DATA = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "data")
    with open(os.path.join(_DATA, "job_stats.json"), encoding="utf-8") as _fh:
        JOB_STATS = json.load(_fh)
except Exception:
    JOB_STATS = {}

STAT_KEYS = ("STR", "CON", "DEX", "INT", "AGI", "CHA")   # client order at +37


def character_stats(c):
    """(stats, level, max_hp) for one character record.

    level: ge_tables.level_of(exp). The DB's LIFE column is NOT the level
           (Grandies has LIFE 99 but is level 60).
    stats: the job's class base stats. GE's scp layer only adds equipment,
           mastery and promotion bonuses on top of these.
    max_hp: the game's own formula, ge\\script\\pc\\get_status_pc.scp SCR_Get_MHP:
              CON*10 + Lv*CON  (+ promotion*1500, + CON-100 bonus*200,
                                 + equipment/sets/mastery - not sourced yet)
    """
    job = int(c.get("job", 0) or 0)
    base = JOB_STATS.get(str(job), {})
    try:
        import ge_tables
        lv = ge_tables.level_of(int(c.get("exp", 0) or 0))
    except Exception:
        lv = int(c.get("life", 1) or 1)
    stats = dict((k, int(base.get(k, 0) or 0)) for k in STAT_KEYS)
    max_hp = stats["CON"] * 10 + lv * stats["CON"]
    return stats, lv, float(max_hp)


# Item use effects, keyed by class id: {scp, arg1, arg2}. Extracted from the
# client's datatable_item_*.ies (data\item_effects.json). The scp name is the
# function the ORIGINAL server ran on use, e.g. SCP_USE_ITEM_HPUP.
try:
    with open(os.path.join(_DATA, "item_effects.json"), encoding="utf-8") as _fh:
        ITEM_EFFECTS = json.load(_fh)
except Exception:
    ITEM_EFFECTS = {}

CMD_ZC_ADD_HP = 3557          # PKS_ZC_ADD_HP
# GE_HEAL=0 keeps the server-side healing (DB + log) but sends no packet, so a
# bad layout can never wedge the client again while we iterate on it.
HEAL_ENABLED = os.environ.get("GE_HEAL", "1") != "0"


def build_add_hp(handle, current_hp, max_delta=0.0, flag=0):
    """ZC_ADD_HP. **Fixed-size packet: 23 bytes, NO length field** (size 0x17 in
    the client's table). Payload starts at +2:

        +0x00 WORD   cmd (0x0de5)
        +0x02 DWORD  HANDLE of the actor whose HP changes
        +0x06 double CURRENT HP      <- proved live: sending 6300 here made the
                                        bar read 6300/6300
        +0x0e double extra           <- 0 for an ordinary heal
        +0x16 BYTE   flag            (0 for items; the floating +N is a skill)

    Verified shape against a reference-server capture:
        E5 0D | 88 1A 0F 00 | <double> | <double> | flag

    History: (1) a length at +2 made the client read it as the handle and drop
    the packet; (2) sending the MAXIMUM at +6 made the bar jump to full, because
    +6 is the current value.
    """
    body = bytearray(23)
    struct.pack_into("<H", body, 0, CMD_ZC_ADD_HP)
    struct.pack_into("<I", body, 2, int(handle) & 0xFFFFFFFF)
    struct.pack_into("<d", body, 6, float(current_hp))
    struct.pack_into("<d", body, 14, float(max_delta))
    body[22] = flag & 0xFF
    return bytes(body)


def heal_pc(conn, amount):
    """AddHP for the leader, mirroring ge\\script\\item\\item.scp
    SCP_USE_ITEM_HPUP: AddHP(self, arg1), clamped at the computed maximum.

    Not implemented: the PiercedWound2 / Deathfire debuff halves arg1, and the
    equipment/mastery/set bonuses to the maximum.
    """
    import ge_db
    row = ge_db.get_character_row(CUR.handle)
    if not row:
        log("    !! heal: character %s not found" % CUR.handle)
        return
    _stats, _lv, max_hp = character_stats(row)
    old_hp = float(row["hp"])
    hp = min(old_hp + amount, max_hp)
    delta = hp - old_hp
    ge_db.set_character_hp(CUR.handle, hp)
    if HEAL_ENABLED and delta:
        conn.sendall(build_add_hp(CUR.handle, hp, 0.0))
        log("    heal +%d -> %.0f / %.0f (delta %.0f)" % (amount, hp, max_hp, delta))
    else:
        log("    heal +%d -> %.0f / %.0f (no packet: GE_HEAL=0 or no change)"
            % (amount, hp, max_hp))


def _get_tick_count():
    """Windows ms-since-boot; the client subtracts its own GetTickCount from the
    serverTime field, and both processes share the same boot clock."""
    try:
        return ctypes.windll.kernel32.GetTickCount() & 0xFFFFFFFF
    except Exception:
        return 0


def _ms():
    """Monotonic milliseconds, for timing a walk (see pc_position)."""
    return _get_tick_count()


def build_start_info(c, commander_name="", handle=1, pos=(0, 0, 0)):
    """One 485-byte START_INFO record. c = a dict from ge_db.get_characters."""
    b = bytearray(SI_SIZE)
    struct.pack_into("<I", b, SI_HANDLE, handle & 0xFFFFFFFF)
    b[SI_NAME:SI_NAME + CHAR_NAME_LEN] = cstr(c["name"], CHAR_NAME_LEN)

    ap = SI_APPEARANCE
    b[ap + AP_NAME:ap + AP_NAME + CHAR_NAME_LEN] = cstr(c["name"], CHAR_NAME_LEN)
    b[ap + AP_GENDER] = 1 if c.get("gender") else 0
    # Job is read as a DWORD by FUN_005dfcbd; the 3 bytes after must be 0.
    struct.pack_into("<I", b, ap + AP_JOB, int(c.get("job", 0)) & 0xFF)
    b[ap + AP_HEADTYPE] = int(c.get("headtype", 0)) & 0xFF

    equip = c.get("equip", {})
    for spot in range(EQUIP_SPOT_LAST):
        cls = equip.get(spot) or EQUIP_NONE[spot] or 0
        struct.pack_into("<I", b, ap + AP_EQUIPMENT + spot * EQUIP_ENTRY_STRIDE,
                         int(cls) & 0xFFFFFFFF)

    struct.pack_into("<iii", b, SI_INITPOS,
                     int(pos[0]), int(pos[1]), int(pos[2]))

    bs = SI_BASICSTAT
    sp = int(c.get("sp", 0) or 0) & 0xFFFF
    stats, lv, max_hp = character_stats(c)
    hp = min(float(c.get("hp", 1) or 1), max_hp)
    b[bs + BS_LV] = lv & 0xFF
    struct.pack_into("<d", b, bs + BS_HP, hp)
    struct.pack_into("<d", b, bs + BS_MAXHP, max_hp)
    struct.pack_into("<H", b, bs + BS_SP, sp)
    struct.pack_into("<H", b, bs + BS_MAXSP, sp)
    struct.pack_into("<q", b, bs + BS_EXP, int(c.get("exp", 0) or 0))
    struct.pack_into("<q", b, bs + BS_NEXTEXP, 0)
    # 6 x WORD at +37 in client order STR, CON, DEX, INT, AGI, CHA
    # (FUN_005e63fe). These were left at zero, which is why Character Info
    # showed every stat as 0 no matter what the DB said.
    for _i, _k in enumerate(STAT_KEYS):
        struct.pack_into("<H", b, bs + BS_STATS + _i * 2, stats[_k] & 0xFFFF)

    # The character's REAL stance list, packed 14-byte STANCE_INFO records right
    # after START_INFO (GeSession::SetMyPcInfo walks them; the client then picks
    # its default stance from them by job + equipped weapon). Empty on any data
    # problem, which is exactly the old behaviour (one fallback stance).
    stance_bytes = c.get("stance_bytes")
    if stance_bytes is None:
        stance_bytes = b""
        if SEND_STANCES:
            try:
                stance_bytes = stance_data.build_stance_records(
                    int(c.get("job", 0)), c.get("stance_rows"),
                    STANCE_XP_TYPES, STANCE_LEVELS)
            except Exception as exc:
                log("  !! stance block failed for %r: %s" % (c.get("name"), exc))
    struct.pack_into("<H", b, SI_STANCECOUNT, len(stance_bytes) // 14)
    # CPlayerChar::GetAllSkills: the skills its stances name, 10-byte records
    # {DWORD type; WORD level; DWORD exp} (exp is the REMAINING exp).
    skill_bytes = c.get("skill_bytes")
    if skill_bytes is None:
        skill_bytes = b""
        if SEND_SKILLS:
            try:
                skill_bytes = skill_data.build_skill_records(
                    int(c.get("job", 0)), c.get("skill_rows"))
            except Exception as exc:
                log("  !! skill block failed for %r: %s" % (c.get("name"), exc))
    struct.pack_into("<H", b, SI_SKILLCOUNT, len(skill_bytes) // 10)
    struct.pack_into("<i", b, SI_OPTIONCOUNT, 0)
    assert len(b) == SI_SIZE
    return bytes(b) + stance_bytes + skill_bytes


def build_startgameok(pcs=None, tick=0.0, permission=0, connection_pos=0,
                      occupy_party=""):
    pcs = pcs or []
    body = struct.pack("<fBB", tick, permission, connection_pos)
    body += cstr(occupy_party, PARTY_NAME_LEN)
    body += struct.pack("<B", len(pcs))
    body += struct.pack("<II", _get_tick_count(), 0)      # serverTime, reserved
    for p in pcs:
        body += p
    total = 4 + len(body)
    assert total == HDR_SIZE + sum(len(p) for p in pcs)
    return struct.pack("<HH", CMD_ZC_START_GAMEOK, total) + body


def move_speed():
    """The speed to send with ZC_MOVE_PC, and the one the arrival clock assumes.

    The original overwrites m_moveSpeed per STANCE when a move starts
    (`m_moveSpeed = GetMoveSPD()`, PlayerChar_FSM.cpp:224), and a click-move is a
    RUN, so the number is the stance's RSPD (WSPD is the walk). CUR.stance is
    the stance the client will put the leader in: the first stance of the first
    job condition matching the equipped weapons (see stance_data.active_stance and
    load_players), or GM_STANCE when nothing matches - the client's own fallback.
    Both the packet AND pc_start_walk's ETA use this one number; drift between them
    makes the click/arrival clock wrong.

    GE_STANCE_ID picks a different stance, GE_MOVE_SPEED overrides the number
    outright - both only for A/B testing.
    """
    if MOVE_SPEED_OVERRIDE > 0:
        return MOVE_SPEED_OVERRIDE
    return stance_data.run_speed(CUR.stance or START_STANCE, MOVE_SPEED_DEFAULT)


def build_leave_pc(handle, reason=LEAVE_PC_REASON):
    """ZC_LEAVE_PC: the player left the zone. 8 bytes."""
    return (struct.pack("<H", CMD_ZC_LEAVE_PC)
            + struct.pack("<I", handle & 0xFFFFFFFF)
            + struct.pack("<H", reason & 0xFFFF))


def build_spawn_standingpc(c, handle, pos, direction=0, stance=0, hp=0.0, max_hp=0.0,
                           class_id=0, weapon_class_id=0, posture=POSTURE_STANDING,
                           options=None):
    """ZC_SPAWN_STANDINGPC: tell a client about another player.

    The layout comes from the CLIENT's own handler (FUN_004f3fbb) and the v28
    server's sender (CPlayerChar::SendSpawned, which writes the basic info
    through FUN_007bdce0). The two agree exactly:

      +0  WORD  command 3520
      +2  WORD  length (the WHOLE packet, including these 2 bytes)
      +4  DWORD handle
      +8  BYTE  direction
      +9  WORD  the character's ClassID (the v28 sender reads the "ClassID"
                 property of the character object)
      +11 WORD  the weapon's ClassID
      +13 DWORD ?
      +17 DWORD x, +21 y, +25 cell
      +29 BYTE  ?
      +30 DWORD the stance
      +34 double hp
      +42 double max hp
      +50 256 bytes of BUFF MASK (not a name! the v28 build had 128 bytes
                 there; the shipped client has 256, and the appearance starts
                 at +306 = 30 + 4 + 8 + 8 + 256)
      +306 APPEARANCE_PC (395 bytes) - the name is inside it, at its +0
      +701 DWORD the equip-option count
    """
    b = bytearray(SP_PC_TOTAL)
    struct.pack_into("<H", b, 0, CMD_ZC_SPAWN_STANDINGPC)
    # The length covers the whole packet, INCLUDING these 2 bytes - the v28
    # sender wrote it last as (end - start).
    struct.pack_into("<H", b, 2, SP_PC_TOTAL)
    struct.pack_into("<I", b, SP_PC_HANDLE, handle & 0xFFFFFFFF)
    b[SP_PC_DIR] = direction & 0xFF
    struct.pack_into("<H", b, SP_PC_CLASS, class_id & 0xFFFF)
    struct.pack_into("<H", b, SP_PC_WCLASS, weapon_class_id & 0xFFFF)
    # The posture: the animation state. It must be one of the MAPPED values
    # (1, 2, 3, 5) - 0 means "unknown" and the client invents an animation.
    struct.pack_into("<I", b, SP_PC_POSTURE, posture & 0xFFFFFFFF)
    struct.pack_into("<iii", b, SP_PC_POS,
                     int(pos[0]), int(pos[1]), int(pos[2]))
    struct.pack_into("<I", b, SP_PC_STANCE, stance & 0xFFFFFFFF)
    struct.pack_into("<d", b, SP_PC_HP, float(hp))
    struct.pack_into("<d", b, SP_PC_MAXHP, float(max_hp))
    # +50..+305 is the BUFF MASK and must be ZERO. Putting anything else there
    # makes the client think the other player has a pile of buffs - it draws
    # them standing in a glowing buff circle.
    # The appearance: the SAME bytes the START_INFO record carries, so the
    # other player looks exactly like they do on their own screen. The name
    # is inside it, at its +0 - and the client reads a SECOND string at the
    # appearance's +341 and uses it as the nameplate, so it is filled too.
    try:
        rec = build_start_info(c, handle=handle, pos=pos)
        ap = bytearray(rec[SI_APPEARANCE:SI_APPEARANCE + SP_PC_APPEARANCE_LEN])
        ap[SP_PC_APPEARANCE_NAME:SP_PC_APPEARANCE_NAME
           + SP_PC_APPEARANCE_NAME_LEN] = cstr(c["name"], SP_PC_APPEARANCE_NAME_LEN)
        b[SP_PC_APPEARANCE:SP_PC_APPEARANCE + SP_PC_APPEARANCE_LEN] = ap
    except Exception as exc:
        log("  !! appearance for %r failed: %s" % (c.get("name"), exc))
    # The equip options: the client reads them (the count at +701 and the
    # records after it), and they are per equipped item.
    options = options or []
    struct.pack_into("<I", b, SP_PC_OPTIONS, len(options))
    off = SP_PC_OPTIONS + 4
    for o in options:
        if off + SP_PC_OPTION_SIZE > len(b):
            break
        struct.pack_into("<III", b, off, int(o["type"]) & 0xFFFFFFFF,
                         int(o["value"]) & 0xFFFFFFFF,
                         int(o["spot"]) & 0xFFFFFFFF)
        off += SP_PC_OPTION_SIZE
    return bytes(b)


# --------------------------------------------------------------------------
# the players in this zone
# --------------------------------------------------------------------------
# Each record: {"conn": the socket, "handle": the character id, "char": the
# character dict, "pos": where they are}. The zone tells every client about
# every other client, so the players can SEE each other - the original did it
# with ZC_SPAWN_STANDINGPC, and we recovered that packet's layout from the
# client's own handler.
ZONE_MEMBERS = []
ZONE_MEMBERS_LOCK = threading.Lock()


def add_member(conn, handle, c, pos):
    rec = {"conn": conn, "handle": handle, "char": c, "pos": pos}
    with ZONE_MEMBERS_LOCK:
        ZONE_MEMBERS.append(rec)
    return rec


def remove_member(conn):
    with ZONE_MEMBERS_LOCK:
        rec = next((m for m in ZONE_MEMBERS if m["conn"] is conn), None)
        if rec is not None:
            ZONE_MEMBERS.remove(rec)
    return rec


def members():
    with ZONE_MEMBERS_LOCK:
        return list(ZONE_MEMBERS)


def broadcast(packet, skip_conn=None):
    """Send a packet to everyone in the zone except one socket."""
    for m in members():
        if skip_conn is not None and m["conn"] is skip_conn:
            continue
        try:
            m["conn"].sendall(packet)
        except Exception as exc:
            log("  !! broadcast to %s failed: %s" % (m["handle"], exc))


def pc_spawn_fields(c):
    """The values the spawn packet needs about another player.

    (stance, hp, max_hp, class_id, weapon_class_id)

    The v28 sender filled the equivalent fields from the character object:
    the character's ClassID, the weapon's ClassID, the stance, the HP and the
    max HP.
    """
    job = int(c.get("job", 0) or 0)
    equip = c.get("equip", {}) or {}
    try:
        _stats, _lv, max_hp = character_stats(c)
    except Exception:
        max_hp = float(c.get("hp", 1) or 1)
    hp = min(float(c.get("hp", max_hp) or max_hp), max_hp)
    try:
        stance = stance_data.active_stance(job, equip) or 0
    except Exception:
        stance = 0
    # The weapon's class id: the item in one of the weapon hands, or 0 when
    # nothing is equipped there. It must be the WEAPON's class - the first
    # worn item is usually the HEAD, and telling the client that the
    # character holds a head-class weapon breaks its model.
    weapon_class_id = 0
    for spot in EQUIP_WEAPON_SPOTS:
        cls = int(equip.get(spot) or 0)
        if cls:
            weapon_class_id = cls
            break
    return stance, hp, max_hp, job, weapon_class_id


def pc_spawn_kwargs(c):
    """The keyword arguments for build_spawn_standingpc about this character."""
    stance, hp, max_hp, class_id, weapon_class_id = pc_spawn_fields(c)
    options = []
    try:
        import ge_db
        options = ge_db.get_char_equip_options(c.get("character_id"))
    except Exception as exc:
        log("  !! equip options for %r failed: %s" % (c.get("name"), exc))
    return {"stance": stance, "hp": hp, "max_hp": max_hp,
            "class_id": class_id, "weapon_class_id": weapon_class_id,
            "options": options}


def announce_players(new_rec):
    """Tell everybody about everybody: the new player about the others, and
    the others about the new player."""
    for m in members():
        if m is new_rec:
            continue
        try:
            new_rec["conn"].sendall(
                build_spawn_standingpc(m["char"], m["handle"], m["pos"],
                                       **pc_spawn_kwargs(m["char"])))
            log("  SENT ZC_SPAWN_STANDINGPC (other) handle=%d name=%r to the new player"
                % (m["handle"], m["char"].get("name")))
        except Exception as exc:
            log("  !! spawn of %s failed: %s" % (m["handle"], exc))
        try:
            m["conn"].sendall(
                build_spawn_standingpc(new_rec["char"], new_rec["handle"],
                                       new_rec["pos"],
                                       **pc_spawn_kwargs(new_rec["char"])))
            log("  SENT ZC_SPAWN_STANDINGPC handle=%d name=%r to the others"
                % (new_rec["handle"], new_rec["char"].get("name")))
        except Exception as exc:
            log("  !! spawn of %s failed: %s" % (new_rec["handle"], exc))


def stop_others_if_walk_over():
    """Tell the other players that this player's walk is over.

    A remote actor's animation is driven ENTIRELY by the move packets: the
    client animates the actor toward the destination and stops when the
    interpolation ends. But the client sends a CZ_MOVE_PC per PATH STEP, and
    every step restarts the interpolation from the actor's logical position -
    which only updates when a walk completes. So the actor can be left walking
    (running) forever, and it never arrives.

    The original server owns the movement and ends it with a ZC_STOP_PC: the
    client's StopPC handler does `Stop(); SetPosition(pos)`, so it stops the
    animation AND puts the actor where it belongs. We have that packet
    (22 bytes) and the zone already knows when a walk ends (the ETA in
    CUR.move), so we send it.
    """
    if CUR.move is None or CUR.stop_sent or not CUR.handle:
        return
    now = _ms()
    if now < CUR.move[3]:
        return
    CUR.stop_sent = True
    pos = pc_position(now) or CUR.move[1]
    for m in members():
        if m["conn"] is CUR.conn:
            continue
        try:
            m["conn"].sendall(build_stop_pc(CUR.handle, pos))
            log("  SENT ZC_STOP_PC handle=%d -> (%d, %d, %d)"
                % (CUR.handle, pos[0], pos[1], pos[2]))
        except Exception as exc:
            log("  !! stop broadcast failed: %s" % exc)


def build_move_pc(handle, pos, spd=None, reason=MR_MOVE):
    """PKS_ZC_MOVE_PC: command(2) handle(4) tick(4) WORLDPOS(3x4) spd(4) reason(4)."""
    if spd is None:
        spd = move_speed()
    pk = struct.pack("<HIIiiifi", CMD_ZC_MOVE_PC, handle & 0xFFFFFFFF,
                     _get_tick_count(), int(pos[0]), int(pos[1]), int(pos[2]),
                     float(spd), int(reason))
    assert len(pk) == 30
    return pk


def build_stance_list(handle, stance_bytes):
    """PKS_ZC_STANCELIST: cmd(2) len(2) handle(4) count(2) + N*STANCE_INFO(14).

    The client derives N from `len` and only UNLOCKS the stances we name that it
    already holds from START_INFO, so send the same records again.
    """
    count = len(stance_bytes) // 14
    body = struct.pack("<IH", handle & 0xFFFFFFFF, count) + stance_bytes
    return struct.pack("<HH", CMD_ZC_STANCE_LIST, 4 + len(body)) + body


def build_skill_list(handle, skill_bytes):
    """PKS_ZC_SKILLLIST: cmd(2) len(2) handle(4) count(2) bmCount(2)
    + count*SKILL_INFO(10) + bmCount*BUFF_MODIFIER_SKILL_INFO(6).

    Same unlock-only semantics as the stance list. bmCount is NOT optional: the
    skill row's level is drawn as `pInfo->level + GetBmSkillInfo(...)->level`
    (UISkillListBox::DrawSkillLevel) and GetBmSkillInfo returns NULL for a skill
    with no buff-modifier record, so bmCount=0 CRASHES the client the moment an
    unlocked skill is drawn. Send one {type, level=0} record per skill.
    """
    count = len(skill_bytes) // 10
    bm = bytearray()
    for i in range(count):
        skill_type = struct.unpack_from("<I", skill_bytes, i * 10)[0]
        bm += struct.pack("<IH", skill_type, 0)
    body = (struct.pack("<IHH", handle & 0xFFFFFFFF, count, len(bm) // 6)
            + skill_bytes + bytes(bm))
    return struct.pack("<HH", CMD_ZC_SKILL_LIST, 4 + len(body)) + body


def build_mission_list(entries):
    """PKS_ZC_RES_MISSION_LIST_STATUS (3335): cmd(2) len(2) count(4) + N*8 bytes.

    Layout from the client's OWN handler `FUN_004ca1e6`: it attaches a CBinary
    with the length at +2, SKIPS 8 bytes, then reads `count` records of two
    DWORDs. The first DWORD is resolved through the client's mission table and
    the record is DROPPED when that lookup fails - so every id must be a real
    `datatable_missionlist` ClassID, or the panel silently shows fewer rows.
    The second DWORD is stored with the entry; its meaning is not proven yet,
    so it is 0.

    `len` covers the whole packet, like every other ZC here.
    """
    body = bytearray()
    for mission_id, flag in entries:
        body += struct.pack("<II", int(mission_id) & 0xFFFFFFFF,
                            int(flag) & 0xFFFFFFFF)
    return (struct.pack("<HHI", CMD_ZC_RES_MISSION_LIST_STATUS,
                        8 + len(body), len(entries)) + bytes(body))


def lobby_checksum(data):
    """The client's own checksum (`FUN_004e8d67`): even index XOR, odd index ADD.

    Several lobby/room packets carry this field and the client VERIFIES it: it
    saves the value, zeroes the field, recomputes over the packet's own length
    word and returns without touching the UI when they differ. A wrong checksum
    therefore looks exactly like "the client ignored the packet".
    """
    acc = 0
    for i, byte in enumerate(data):
        acc = (acc ^ byte) if (i & 1) == 0 else (acc + byte)
    return acc & 0xFFFFFFFF


def build_lobby_room_list(account_id, rooms=(), enable_create=True):
    """PKS_ZC_LOBBY_ROOM_LIST (3705): opens the mission-lobby window.

    Layout from the client's own handler `FUN_004e6ef6`:
    `cmd(2) len(2) accountID(4) checksum(4) roomCount(2) enableCreate(1)` and
    then `roomCount` records of **68 bytes** each (name at +0x00, master at
    +0x20, isPrivate at +0x31, curPlayer/maxPlayer at +0x32/+0x33, mission id
    WORD at +0x37).

    `roomCount = 0` still SHOWS the window (the handler calls ShowWindow
    unconditionally), so the window can be proved before any room exists. The
    record writer is deliberately not implemented until rooms are.
    """
    total = 15 + 68 * len(rooms)
    b = bytearray(total)
    struct.pack_into("<H", b, 0, CMD_ZC_LOBBY_ROOM_LIST)
    struct.pack_into("<H", b, 2, total)
    struct.pack_into("<I", b, 4, int(account_id or 0) & 0xFFFFFFFF)
    struct.pack_into("<I", b, 8, 0)          # checksum: over the whole packet
    struct.pack_into("<H", b, 12, len(rooms))
    b[14] = 1 if enable_create else 0
    off = 15
    for rec in rooms:
        b[off:off + ROOM_LEN] = rec
        off += ROOM_LEN
    struct.pack_into("<I", b, 8, lobby_checksum(bytes(b)))
    return bytes(b)


def build_lobby_create_ui(preset):
    """PKS_ZC_LOBBY_CREATEUI_OPEN (3711): opens the lobby's create-room dialog.

    **FIXED size, no length word**: `cmd(2) + preset[32]` = 34 bytes, because
    `PKS_DEFAULT` is the fixed base and `LOBBY_TYPE_LEN` is **32**
    (`geShared.h:263`). Writing a length at +2 (the habit from the variable-size
    packets) shifts the preset by two bytes, the client's
    `GetClass("MissionPreset", preset)` fails, and it returns in silence -
    measured here: the dialog simply never appeared.

    The shipped sender is one line (`CCommander::OnLobbyClickCreateRoom`,
    `Commander_Packet.cpp:2069`): `strncpy_s(out.preset, m_lobbyType, _TRUNCATE)`.
    `preset` must be a ClassName in `MissionPreset` (971 rows), and the row's
    `UiFrame` (e.g. `mcwithnolv`) names the window that gets shown.
    """
    b = bytearray(34)
    struct.pack_into("<H", b, 0, CMD_ZC_LOBBY_CREATEUI_OPEN)
    raw = str(preset or "").encode("ascii", "replace")[:31]
    b[2:2 + len(raw)] = raw
    return bytes(b)


def build_room_record(name, master, mission_id, room_id, cur=0, max_players=8,
                      is_private=0, level_limited=0, min_level=1, max_level=255,
                      remain_seconds=1800):
    """One 68-byte ROOM record, at the offsets the CLIENT reads.

    Authority is the client's list handler `FUN_004e6ef6`: name +0x00 (32 bytes),
    master +0x20 (`COMMANDER_NAME_LEN` = 17), isPrivate +0x31, curPlayer +0x32,
    maxPlayer +0x33, isLevelLimited +0x34, minLevel +0x35, maxLevel +0x36,
    missionId +0x37 (WORD), id +0x39 (WORD), remainTime +0x3B (DWORD),
    masterMaxLevel +0x3F. The tail to 68 bytes stays zero.
    """
    b = bytearray(ROOM_LEN)

    def _put(off, size, text):
        raw = str(text or "").encode("cp949", "replace")[:size - 1]
        b[off:off + len(raw)] = raw

    _put(0x00, 32, name)
    _put(0x20, COMMANDER_NAME_LEN, master)
    b[0x31] = 1 if is_private else 0
    b[0x32] = cur & 0xFF
    b[0x33] = max_players & 0xFF
    b[0x34] = 1 if level_limited else 0
    b[0x35] = min_level & 0xFF
    b[0x36] = max_level & 0xFF
    struct.pack_into("<H", b, 0x37, mission_id & 0xFFFF)
    # `id` is at +0x3D in THIS client's 68-byte record, not +0x39: the probe
    # (write a different value at each candidate, read back what the client sends
    # in CZ_LOBBY_JOIN_ROOM) answered "position 3". So the extra 4 bytes over the
    # shipped 64-byte struct sit between missionId and id.
    struct.pack_into("<H", b, 0x3D, room_id & 0xFFFF)
    # remainTime is AFTER the id (0x3F..0x42) and is in MILLISECONDS: the live
    # record holds 1_800_000 for a 30-minute room. Writing plain seconds (1800)
    # makes a 1.8-second timer, which shows 00:00 and immediately fires the
    # client's "Time left until game start : 1" chat line.
    struct.pack_into("<I", b, 0x3F, (remain_seconds * 1000) & 0xFFFFFFFF)
    return bytes(b)


def unprobe_room_id(sent):
    """Map a probed room id back to the real one (see build_room_record).

    Temporary: exists only while the id offset is unknown.
    """
    if sent in ROOMS:
        return sent
    for _n in (1, 2, 3, 4, 5):
        if sent - 0x10 * _n in ROOMS:
            log("  probe: client read the room id at position %d" % _n)
            return sent - 0x10 * _n
    return sent


def build_room_info(record):
    """PKS_ZC_ROOM_INFO (3712): FIXED 70 = `cmd(2) + ROOM(68)`, payload at +2.

    `PKS_ZC_ROOM_INFO` is `PKS_DEFAULT`, i.e. the FIXED base - no length word.
    Writing one is exactly the mistake that kept the create dialog invisible.
    """
    b = bytearray(2 + ROOM_LEN)
    struct.pack_into("<H", b, 0, CMD_ZC_ROOM_INFO)
    b[2:2 + ROOM_LEN] = record
    return bytes(b)


def _my_name():
    """The name the CLIENT believes it has.

    `GetCommanderInfo().name` comes from the character/commander record we send in
    START_INFO (`cm["name"]`), NOT from the start-game packet's `id`. The room's
    leader test is `StrIsSame(master, that name)`, so filling `master` from the
    other string silently demoted the leader to a member ("Ready/Cancel" instead
    of "Mission Start"). Measured here: DB `Arcis` vs start packet `Arcanine`.
    """
    return (getattr(CUR, "commander_name", "") or
            _my_name())


def _room_token():
    """Who this player is, inside the room tables.

    One zone process serves several players, so a room's members are keyed by the
    commander id; the fallback keeps a single-player test working without one.
    """
    return int(getattr(CUR, "commander_id", 0) or 0) or id(CUR)


def _room_records():
    """Every room as a 68-byte record, with its CURRENT player count.

    The count is `len(members)`, never a constant: measuring the first version
    showed four rooms each claiming `1/8` for a single player.
    """
    out = []
    for room_id in sorted(ROOMS):
        room = ROOMS[room_id]
        out.append(build_room_record(room["name"], room["master"],
                                     room["mission"], room_id,
                                     cur=len(room["members"]),
                                     max_players=room["max"]))
    return out


def _leave_room():
    """Take the current player out of their room; drop the room when it empties.

    A player is in ONE room at a time, so a second "Create a Room" must free the
    first. Without this every press left an orphan that still claimed a player -
    exactly what the lobby showed.
    """
    room_id = getattr(CUR, "room_id", 0) or 0
    CUR.room_id = 0
    room = ROOMS.get(room_id)
    if not room:
        return
    room["members"].discard(_room_token())
    fam = _my_name()
    if fam in (room.get("names") or []):
        room["names"].remove(fam)
    if not room["members"]:
        del ROOMS[room_id]
        return
    # Leadership passes DOWN the list: if the master left and members remain, the
    # next member owns the room. Live does this silently - no notice, no lobby.
    if room.get("master") == fam and room["names"]:
        room["master"] = room["names"][0]
        log("  room %s: leadership passes to %r" % (room_id, room["master"]))


def build_room_info(record, cmd=None):
    """PKS_ZC_ROOM_INFO (3712) / PKS_ZC_LOBBY_JOIN_ROOM (3707): FIXED 70.

    `cmd(2) + ROOM(68)`, payload at +2 - both structs are `PKS_DEFAULT` (the
    fixed base), so there is NO length word. Writing one is exactly the mistake
    that kept the create dialog invisible.
    """
    b = bytearray(2 + ROOM_LEN)
    struct.pack_into("<H", b, 0, cmd or CMD_ZC_ROOM_INFO)
    b[2:2 + ROOM_LEN] = record
    return bytes(b)


def build_slot_record(state=0, name="", team=0, slot_index=0, leader=0,
                      max_level=0):
    """One 27-byte ROOM_SLOT, at the offsets the CLIENT reads.

    state DWORD +0x00 (0 OPEN, 1 CLOSED, 2 OCCUPIED), name +0x04 (17 bytes),
    teamIndex +0x15 (21), slotIndex +0x16 (22), isLeader +0x17, maxLevel +0x18,
    then two spare bytes: the client divides by 27 (`(len - 6) / 0x1b` in
    `FUN_004e86d0`) and takes the array from `packet + 6` (`FUN_004e6ba0`).
    """
    b = bytearray(SLOT_LEN)
    struct.pack_into("<I", b, 0, state & 0xFFFFFFFF)
    raw = str(name or "").encode("cp949", "replace")[:16]
    b[4:4 + len(raw)] = raw
    b[21] = team & 0xFF
    b[22] = slot_index & 0xFF
    b[23] = 1 if leader else 0
    b[24] = max_level & 0xFF
    # Live sets these two for the occupied slot (`... 01 96 01 01`); the client's
    # slot-list handler reads +25/+26. Zeroing them is why our creator's row
    # stayed EDITABLE instead of being locked/highlighted like the live one.
    b[25] = 1 if leader else 0
    b[26] = 1 if leader else 0
    return bytes(b)


def build_room_slots(room_id, members=(), max_players=8):
    """PKS_ZC_ROOM_SLOT_LIST (3725): header 6 + `max` slots of 27.

    Header, from the LIVE capture: `cmd(2) len(2) roomId(1) maxPlayers(1)` - the
    max is a BYTE at +5, NOT the room record (that is why our room showed
    `Players 1/0`). Live sends exactly `max` slots (5 for a 5-player room), with
    the rest of the 30-box grid left OPEN by the client itself.
    """
    count = max(1, int(max_players or 1))
    names = list(members)
    slots = []
    for i in range(count):
        if i < len(names):
            slots.append(build_slot_record(state=2, name=names[i], team=0,
                                           slot_index=i,
                                           leader=1 if i == 0 else 0))
        else:
            slots.append(build_slot_record(state=0, team=0, slot_index=i))
    body = b"".join(slots)
    b = bytearray(6 + len(body))
    struct.pack_into("<H", b, 0, CMD_ZC_ROOM_SLOT_LIST)
    struct.pack_into("<H", b, 2, 6 + len(body))
    b[4] = room_id & 0xFF
    b[5] = count & 0xFF
    b[6:] = body
    return bytes(b)


def build_room_all_ready(ready=1):
    """PKS_ZC_ROOM_ALL_READY (3716): fixed 3 = `cmd(2) + BYTE`.

    Live sends this from the lobby when the room's members are ready (2x in the
    capture). Our mission-start slice sends it as the acknowledgement that the
    master pressed Start; the actual mission entry is not implemented yet.
    """
    return struct.pack("<HB", CMD_ZC_ROOM_ALL_READY, 1 if ready else 0)


def build_room_count(count):
    """PKS_ZC_ROOM_COUNT (3717): fixed 3 = `cmd(2) + BYTE count`."""
    return struct.pack("<HB", CMD_ZC_ROOM_COUNT, count & 0xFF)


def build_room_join(name):
    """PKS_ZC_ROOM_JOIN (3718): fixed 19 = `cmd(2) + commanderName[17]`.

    The live server sends one for EVERY join (17 in a single capture). Our client's
    handler only chats the name (`RoomJoin`), so this is sent to reproduce the
    sequence the room expects - the Exit gate is still unexplained.
    """
    b = bytearray(2 + COMMANDER_NAME_LEN)
    struct.pack_into("<H", b, 0, CMD_ZC_ROOM_JOIN)
    raw = str(name or "").encode("cp949", "replace")[:COMMANDER_NAME_LEN - 1]
    b[2:2 + len(raw)] = raw
    return bytes(b)


def build_room_exit(name):
    """PKS_ZC_ROOM_EXIT (3720): fixed 19 = `cmd(2) + commanderName[17]`.

    The client only uses it to clear its own room slot when the name matches
    (`RoomExit`, `GENormalNet_Mission.cpp`).
    """
    b = bytearray(2 + COMMANDER_NAME_LEN)
    struct.pack_into("<H", b, 0, CMD_ZC_ROOM_EXIT)
    raw = str(name or "").encode("cp949", "replace")[:COMMANDER_NAME_LEN - 1]
    b[2:2 + len(raw)] = raw
    return bytes(b)


def build_room_out(reason=1, room_type=0):
    """PKS_ZC_ROOM_OUT (3726): fixed 10 = `cmd(2) + DWORD roomType + DWORD reason`.

    This is the packet that CLOSES the room window: `RoomOut` hides the UI and
    branches on `reason` (`MOR_NONE` 0, `MOR_MASTER_OUT` 1).
    """
    return struct.pack("<HII", CMD_ZC_ROOM_OUT, room_type & 0xFFFFFFFF,
                       reason & 0xFFFFFFFF)


def mission_list_entries():
    """`[(mission_id, 0), ...]` from the client's own table; [] on failure."""
    try:
        return [(cid, 0) for cid, _row in mission_data.mission_entries()]
    except Exception as exc:
        log("  !! mission list unavailable: %s" % exc)
        return []


def build_change_stance(handle, stance):
    """PKS_ZC_CHANGE_STANCE: command(2) handle(4) stanceType(4) = 10 bytes.

    From the client's own table (size 10), not source2's 12: this packet class has
    no length word. CNormalNet::ChangeStance -> SetStance(handle, type, true) for
    the local PC too, which is what actually changes the character's stance.
    """
    pk = struct.pack("<HII", CMD_ZC_CHANGE_STANCE, handle & 0xFFFFFFFF,
                     int(stance) & 0xFFFFFFFF)
    assert len(pk) == 10
    return pk


def build_move_barrack():
    """PKS_ZC_MOVE_BARRACK: command(2) only = 2 bytes (client table size 2)."""
    return struct.pack("<H", CMD_ZC_MOVE_BARRACK)


def build_logout_ok():
    """PKS_ZC_LOGOUT_OK: command(2) only = 2 bytes (client table size 2).

    NOT optional for the barrack trip: the client only acts on the
    ZC_MOVE_BARRACK switch target once `CNormalMode::m_isAbleToLogout` is true,
    and ONLY the ZC_LOGOUT_OK handler sets it (GENormalNet::LogoutOK ->
    NormalMode()->EnableLogout()). Without it the client waits forever, then the
    socket close drops it at the login screen.
    """
    return struct.pack("<H", CMD_ZC_LOGOUT_OK)


def build_stop_pc(handle, pos):
    """PKS_ZC_STOP_PC: command(2) handle(4) tick(4) WORLDPOS(3x4) = 22.

    The client's StopPC handler calls gemove::ActorStop, which does
    `Stop(); SetPosition(pos)` - it sets the position outright. That is what
    turns a move into a jump, so a town quick-move is sent as move + stop.
    """
    pk = struct.pack("<HIIiii", CMD_ZC_STOP_PC, handle & 0xFFFFFFFF,
                     _get_tick_count(), int(pos[0]), int(pos[1]), int(pos[2]))
    assert len(pk) == 22
    return pk


def build_dialog_ok(handle, text, dlg_type=DLG_WITH_FADE_IN):
    """PKS_ZC_DIALOG_OK: command(2) len(2) type(1) handle(4) text + NUL.

    The client's TriggerDialogOk handler shows `text` in the standard dialog
    box and calls FillDialogText(text, packet, packet->len, sizeof(struct)),
    so the text must start at byte 9 and `len` must be the whole packet size
    including the terminator.
    """
    body = text.encode("latin-1", "replace") + b"\0"
    pk = bytearray(9 + len(body))
    struct.pack_into("<H", pk, 0, CMD_ZC_DIALOG_OK)
    struct.pack_into("<H", pk, 2, len(pk))
    pk[4] = dlg_type & 0xFF
    struct.pack_into("<I", pk, 5, handle & 0xFFFFFFFF)
    pk[9:] = body
    return bytes(pk)


def build_dialog_close():
    """PKS_ZC_DIALOG_CLOSE: command only (2 bytes). Ends the modal dialog mode."""
    pk = struct.pack("<H", CMD_ZC_DIALOG_CLOSE)
    assert len(pk) == 2
    return pk


def build_npc_click_voice(handle):
    """PKS_ZC_PLAY_NPC_CLICK_VOICE: command(2) + DWORD handle = 6 bytes.

    The live server sends this when the player clicks an NPC - the NPC's own
    "click" voice. Proved on the live (2026-10-21, capture `liveclick4`): a
    6-byte packet with the CLICKED NPC's handle, sent right after the dialog
    opens (and once more for a nearby NPC before it). The id and the size come
    from the client's own packet table: 3515, fixed 6 bytes.
    """
    pk = struct.pack("<HI", CMD_ZC_PLAY_NPC_CLICK_VOICE, handle & 0xFFFFFFFF)
    assert len(pk) == 6
    return pk


def build_commander_point(point):
    """PKS_ZC_COMMANDER_POINT: command(2) + DWORD point = 6 bytes."""
    pk = struct.pack("<HI", CMD_ZC_COMMANDER_POINT, int(point) & 0xFFFFFFFF)
    assert len(pk) == 6
    return pk


def build_zone_update_table(idspace="Lock", props=None):
    """PKS_ZC_UPDATE_TABLE: command(2) + len(2) + type[20] + xml + NUL.

    Same shape as the barrack's BC_UPDATE_TABLE (cmd 32), which the client
    already accepts - only the command id differs (3747)."""
    if props is None:
        try:
            from ge_barrack_server import load_lock_props
            props = load_lock_props()
        except Exception:
            props = []
    attrs = "".join('%s = "%s" ' % (k, v) for k, v in props)
    xml = '<idspace id="%s"> <Class ClassID="1" ClassName="%s" %s/></idspace> \n' \
        % (idspace, idspace, attrs)
    body = xml.encode("latin-1", "replace") + b"\0"
    total = 24 + len(body)                  # cmd(2) + len(2) + type[20] + xml + NUL
    return (struct.pack("<HH", CMD_ZC_UPDATE_TABLE, total)
            + cstr(idspace, 20) + body)


def _empty_count_type(cmd):
    """Empty inventory-family list: cmd(2)+len(2)+count(4)+type(4) = 12."""
    return struct.pack("<HHII", cmd, 12, 0, ILT_INVENTORY)


def build_remove_item(inv_index, count, remove_type=0):
    """PKS_ZC_REMOVEITEM: cmd(2) invIndex(4) count(4) removeType(1) autoRule(4).

    `count` is the NEW TOTAL left in that bag row (0 = the row is gone), which is
    what the client's RemoveItem -> ChangeItemCount expects. Sending this before
    the refreshed list is what updates an OPEN shop window: the handler ends with
    UpdateTradeInfo(), the bulk list does not.
    """
    return struct.pack("<HIIBi", CMD_ZC_REMOVEITEM,
                       int(inv_index) & 0xFFFFFFFF,
                       int(count) & 0xFFFFFFFF,
                       int(remove_type) & 0xFF, AUTORULE_NONE)


def plan_consumption(inv, costs):
    """[{class: total}] -> [(wire invIndex, newCount)] for the rows it eats.

    `inv` is the exact list the client holds (CUR.last_inv) and the DB reads stacks
    oldest-first, so both sides consume the same rows. Only the new total per row
    goes on the wire.
    """
    left = dict((int(k), int(v)) for k, v in costs.items())
    out = []
    for idx, row in enumerate(inv or [], 1):
        need = left.get(row["type"], 0)
        if need <= 0 or row["count"] <= 0:
            continue
        take = min(need, row["count"])
        left[row["type"]] = need - take
        out.append((idx, row["count"] - take))
    return out


def inv_remove(inv, spans):
    """Mirror ZC_REMOVEITEM on our copy of the client's list: set new totals.

    A row that hits 0 is KEPT as a hole, because the client keeps its invIndex
    key too; deleting here would renumber and every later packet would point at
    the wrong item.
    """
    for idx, remain in spans:
        if 0 < idx <= len(inv):
            inv[idx - 1]["count"] = remain


def inv_add(inv, item_id, item_class, count):
    """Mirror ZC_ADDITEM: update the row if the client has it, else append."""
    for i, row in enumerate(inv or []):
        if row["item_id"] == item_id:
            row["count"] = count
            return i + 1
    inv.append({"item_id": item_id, "type": item_class, "count": count})
    return len(inv)


def build_add_item(item_type, inv_index, count, add_type=0):
    """PKS_ZC_ADDITEM (44 bytes) - "this item now exists / now has N".

    Only used where the ORIGINAL server uses it (after a transaction), never
    instead of the entry-time inventory list. Sending it keeps the client's bag
    layout untouched, which a bulk list does not.
    """
    return struct.pack(
        "<HHIIIBQBIIIBBI",
        CMD_ZC_ADDITEM & 0xFFFF, 44,
        int(item_type) & 0xFFFFFFFF,
        int(inv_index) & 0xFFFFFFFF,
        int(count) & 0xFFFFFFFF,
        0,                      # PType (protection)
        0,                      # GUID: the handler never reads it
        int(add_type) & 0xFF,
        0xFFFFFFFF,             # expireTime: -1 = permanent (reference value)
        0, 0,                   # optionCount, socketCount
        0, 0,                   # isReinforce, WaitSecond
        AUTORULE_NONE)


def build_system_message(text):
    """ZC_MESSAGE (3596): cmd(2) len(2) type(4) level(1) tm(36) text + NUL.

    `tm` is the 9-int C runtime struct, and the packet is packed (geZCPacket.h
    is inside #pragma pack(1)), so it is 36 bytes with no padding.
    """
    t = time.localtime()
    tm = struct.pack("<9i", t.tm_sec, t.tm_min, t.tm_hour, t.tm_mday,
                     t.tm_mon - 1, t.tm_year - 1900, t.tm_wday, t.tm_yday,
                     t.tm_isdst)
    body = (struct.pack("<IB", MESSAGE_TYPE_GENERAL, MESSAGE_LEVEL_GENERAL)
            + tm + text.encode("cp949", "replace") + b"\0")
    return struct.pack("<HH", CMD_ZC_MESSAGE, 4 + len(body)) + body


def build_inventory_item_list(items=None):
    """PKS_ZC_INVENTORY_ITEM_LIST: cmd(2)+len(2)+count(4)+type(4) + N*24.

    The shipped client's records are 24 bytes, NOT source2's 13/16. PROVEN
    from the client's own walker `FUN_007aa28c` (stride `ADD EDI,0x18`) plus
    the crash: with 16-byte records the 2nd lookup key came out as 0x2 (our
    2nd record's invIndex read at the wrong offset) -> null table lookup ->
    AV at 0x6da12a `MOV EAX,[EAX+0x27c]`. Fields the walker reads: DWORD
    type@0 (item-table key), DWORD@4, DWORD@8, BYTE@12, DWORD@16, DWORD@20.
    First four match source2's {type,count,invIndex,protect}; the trailing 8
    were appended in the shipped build (zeros here). invIndex runs 1..N.
    With no items this is the 12-byte empty shape."""
    items = items or []
    body = struct.pack("<II", len(items), ILT_INVENTORY)
    for idx, it in enumerate(items, 1):
        body += struct.pack("<IIIBxxxQ", int(it["type"]) & 0xFFFFFFFF,
                            int(it["count"]) & 0xFFFFFFFF,
                            idx & 0xFFFFFFFF, 0, 0)
    total = 4 + len(body)
    return struct.pack("<HH", CMD_ZC_INVENTORY_ITEM_LIST, total) + body


def build_inventory_option_list(opts=None):
    """PKS_ZC_INVENTORY_ITEM_OPTION_LIST: cmd+len+count+type + N*12.

    INVENTORY_ITEM_OPTION = {DWORD type; DWORD value; DWORD invIndex}, all
    DWORDs so no packing trap. Client walks sizeof (ExtractInvItemOptionList).
    Empty = 12-byte shape."""
    opts = opts or []
    body = struct.pack("<II", len(opts), ILT_INVENTORY)
    for o in opts:
        body += struct.pack("<III", int(o["type"]) & 0xFFFFFFFF,
                            int(o["value"]) & 0xFFFFFFFF,
                            int(o["inv"]) & 0xFFFFFFFF)
    total = 4 + len(body)
    return struct.pack("<HH", CMD_ZC_INVENTORY_ITEM_OPTION_LIST, total) + body


def build_inventory_socket_list():
    return _empty_count_type(CMD_ZC_INVENTORY_ITEM_SOCKET_LIST)


def _prop_is_number(value):
    r"""Does the original IES library call this value a NUMBER?

    `imcIES\include\IESInternal.cpp:6 IsNumberValue` + `:69 GetValueType`: the
    value is split on spaces and must be ONE token, then optional '-' and only
    digits and at most one '.'. So "1" and "-3.5" are numbers; "" is not (no
    token), and neither is "120071 , 12," (several tokens).
    """
    tok = (value or "").split()
    if len(tok) != 1:
        return False
    t = tok[0]
    if t.startswith("-"):
        t = t[1:]
    if not t:
        return False
    return t.replace(".", "", 1).isdigit() and t.count(".") <= 1


# The imcIES::CObject wire form for an item's properties now lives in
# `ge_item_props.py`, because the BARRACK server needs the same serializer.
# The names below are the same as before, so nothing else changes.
ITEM_PROPERTY_TYPES = ge_item_props.ITEM_PROPERTY_TYPES
ITEM_PROPERTY_IDSPACE = ge_item_props.ITEM_PROPERTY_IDSPACE
ITEM_PROPERTY_CLASSID = ge_item_props.ITEM_PROPERTY_CLASSID
item_property_type = ge_item_props.item_property_type
build_item_property_object = ge_item_props.build_item_property_object


def build_item_property_object(props):
    r"""imcIES::CObject wire form for one item's properties.

    Layout (`imcIES\source\Object.cpp:607 Serialize` / `:672 Extract`):
        int classCount; int propertyCount;
        classCount x { NUL-terminated idspace; DWORD ClassID }
        propertyCount x { BYTE keyLen; char key[keyLen]; char type; value }
    `type` 0 = NUMBER (double), 1 = STRING, 2 = CALCULATED (string on the wire).
    keyLen/valueLen are `strlen+1` and INCLUDE the NUL.

    **NUMBER is a DOUBLE (8 bytes), not a float.** The shipped source
    (`imcIES\source\Object.cpp:648`) writes `sizeof(float)`, but the CLIENT is a
    newer build and reads 8 bytes - disassembly of its own extractor
    (`FUN_0059cde9`, the function containing the 0x19ce2d crash) does
    `PUSH 0x8; CALL Read(8); MOVSD XMM0, qword ptr [EAX]`. Writing 4 bytes
    desyncs the reader, and the damage shows up in the NEXT record's header,
    not at the value: `Read(4)` runs off the end, returns null, and the class
    loop crashes at `PUSH [EAX]` (`ge.exe+0x19ce2d`). That is why a class-less
    payload and a correctly-classed payload both died at the same address.

    The class is the one the ORIGINAL server puts on an inventory item:
    `imcIES::CreateObject("Item_Property", 1)` (`InvItem.cpp:267` and `:1265`),
    i.e. idspace `Item_Property`, ClassID `1` (`typedef unsigned int ClassID`,
    `imcIES.h:22`, so 4 bytes on the wire). `ZServerLib\Inventory.cpp:466`
    serializes that object as-is.

    A class is NOT optional on the wire. `classCount = 0` was tried on
    2026-10-19 and CRASHED the live client (`0xc0000005` at `ge.exe+0x19ce2d`,
    `"Can't Find BaseClass : %s:%d"`): the client's `CObject::Extract` compares
    the incoming class against its own class table and the caller then uses a
    null. Do not send an object without a resolvable class.
    """
    out = struct.pack("<ii", 1, len(props))
    out += ITEM_PROPERTY_IDSPACE + b"\0" + struct.pack("<I", ITEM_PROPERTY_CLASSID)
    for p in props:
        key = (p["property"] or "").encode("latin-1", "replace")[:255]
        kind = item_property_type(p["property"], p["value"])
        if kind == "number":
            try:
                val = float(p["value"])
            except (TypeError, ValueError):
                val = 0.0
            out += struct.pack("<B", len(key) + 1) + key + b"\0"
            out += struct.pack("<B", 0)          # PROPERTY_ENTRY::NUMBER
            out += struct.pack("<d", val)        # DOUBLE - see the docstring
        else:
            val = (p["value"] or "").encode("latin-1", "replace")[:255]
            out += struct.pack("<B", len(key) + 1) + key + b"\0"
            out += struct.pack("<B", 1)          # PROPERTY_ENTRY::STRING
            out += struct.pack("<B", len(val) + 1) + val + b"\0"
    return out


def build_inventory_property_list(items=None):
    r"""PKS_ZC_INVENTORY_ITEM_PROPERTY_LIST: cmd+len+count+type+flag + N records.

    **13-byte header** (client handler `FUN_004b5f48`, id `0xe16` reached from
    the dispatcher's `switch(param_1 - 0xd7d)` case `0x99`):
      +0 WORD cmd, +2 WORD len, +4 DWORD count, +8 DWORD type, +0xc BYTE flag.
    The handler attaches the packet to a `CBinary` and skips exactly 13 bytes,
    so records start at offset 13. The flag byte is the only field our earlier
    12-byte header was missing; without it every record shifts by one, the
    class/property counts become garbage, `Extract` reads past the buffer and
    the client dies at `ge.exe+0x19ce2d`.

    Record = `int invIndex; BYTE exist;` then the object when exist is 1
    (`FUN_0067dfdd`: ReadInto 4, ReadInto 1, and on 1 -> new object -> vtable
    slot 0x80 = `CObject::Extract`). A 0 record is just those 5 bytes.
    NOTE: this LIST packet has a count; the single-record packet
    `ZC_INVENTORY_ITEM_PROPERTY` has none.

    `items` entries are {"inv": <wire index 1..N>, "props": [db rows]}.
    Empty = just the 13-byte header.

    Flag: 0 = keep what is already stored. 1 = clear first (`FUN_007a7f70`
    walks the existing entries and releases them). We send 0 for the first
    live test because clearing is not needed on an empty list.
    """
    items = items or []
    # Header is **13** bytes, not 12: count(4) + type(4) + flag(1).
    # The handler reads `len` at +2, count at +4, type at +8, flag at +0xc, then
    # skips exactly 13 bytes before the record loop. Omitting the flag byte
    # shifted every record by one -> garbage class/property counts -> read past
    # the buffer -> crash in CObject::Extract (ge.exe+0x19ce2d).
    body = struct.pack("<IIB", len(items), ILT_INVENTORY, 0)
    for it in items:
        props = it.get("props") or []
        body += struct.pack("<IB", int(it["inv"]) & 0xFFFFFFFF, 1 if props else 0)
        if props:
            body += build_item_property_object(props)
    total = 4 + len(body)
    return struct.pack("<HH", CMD_ZC_INVENTORY_ITEM_PROPERTY_LIST, total) + body


def build_reinforce_list(items=None):
    """PKS_ZC_REINFORCE_INFO_LIST: cmd+len+type(4)+count(4) + N*5.

    Record = REINFORCE_INFO = {DWORD itemIndex; BYTE accumCount;} = **5 bytes**
    (source2 geZCTypes.h is `#pragma pack(1)`, so no padding). The client walks
    `pReInforceBuf += sizeof(REINFORCE_INFO)` (UISession.cpp
    `ItemList::ExtractReInforceInfo`), and it dedups by itemIndex
    (`GetReInforceInfoIndex`). With 8-byte records the 2nd record's itemIndex
    reads from the middle of our bytes -> invalid id, so only the 1st item ever
    showed +6 and swapping the order flipped which one. 5-byte records align
    both. Empty = 12 bytes."""
    items = items or []
    body = struct.pack("<II", ILT_INVENTORY, len(items))
    for it in items:
        body += struct.pack("<IB", int(it["inv"]) & 0xFFFFFFFF,
                            int(it["count"]) & 0xFF)
    total = 4 + len(body)
    return struct.pack("<HH", CMD_ZC_REINFORCE_INFO_LIST, total) + body


def build_update_property_number(name, value, id_space="Lock"):
    """PKS_ZC_UPDATE_PROPERTY_NUMBER: command(2)+idSpace[20]+property[20]+value(4).

    The client sets its own IES cache from this and redraws the window that
    property drives (`Lock_InventoryCount` -> "inventory"), so a script's
    property change is invisible to the player without it.
    """
    pk = struct.pack("<H", CMD_ZC_UPDATE_PROPERTY_NUMBER)
    pk += id_space.encode("latin-1")[:19].ljust(20, b"\0")
    pk += str(name).encode("latin-1")[:19].ljust(20, b"\0")
    pk += struct.pack("<I", int(value) & 0xFFFFFFFF)
    assert len(pk) == 46, len(pk)
    return pk


def build_lock_properties(commander_id=None):
    """[(name, packet)] for the commander's Lock_* properties.

    Sent at zone entry so the client's IES starts from the DB value. Only ~14
    exist, so this is well under a kilobyte - and without it the client keeps
    its table default (248 slots here) however many the character bought.
    """
    cid = (commander_id if commander_id is not None
           else getattr(CUR, "commander_id", 0))
    out = []
    if not cid:
        return out
    try:
        import ge_db
        for name, value in sorted(ge_db.get_commander_quest_states(cid).items()):
            if name.startswith("Lock_"):
                out.append((name, build_update_property_number(name, value)))
    except Exception as exc:
        log("  !! could not build the Lock_* properties: %s" % exc)
    return out


def build_max_weight(weight=DEFAULT_MAX_WEIGHT):
    """PKS_ZC_CHANGE_INV_MAXWEIGHT: command(2) + DWORD weight = 6."""
    pk = struct.pack("<HI", CMD_ZC_CHANGE_INV_MAXWEIGHT,
                     int(weight) & 0xFFFFFFFF)
    assert len(pk) == 6
    return pk


QUEST_STATE_UNAVAILABLE = 0xC8     # what the live server sends for "not reached"


def quest_state_overrides():
    """index -> state, from `config\\quest_state.txt` (one `<index> <state>` a line).

    This exists to prove the POSITIONAL mapping: write `0 0` and the client
    should offer the first quest in `datatable_questinfo.ies` ("Begin Pioneer").
    An absent or empty file leaves every quest "not reached", which is the
    shipped default. Real per-player state will replace this later - the file
    is a knob, not the design.
    """
    import ge_paths
    path = os.path.join(ge_paths.CONFIG, "quest_state.txt")
    out = {}
    try:
        with open(path, encoding="utf-8") as fh:
            for line in fh:
                line = line.split("#", 1)[0].strip()
                if not line:
                    continue
                parts = line.split()
                if len(parts) >= 2:
                    try:
                        out[int(parts[0])] = int(parts[1], 0)
                    except ValueError:
                        print("  quest_state.txt: ignoring %r" % line)
    except OSError:
        pass
    return out


_QUEST_CLASS_NAMES = None
_QUEST_CHECKUP = None


def quest_class_names():
    """Ordered list of quest ClassName values in datatable_questinfo.ies.

    The ZC_QUEST_INFO byte array is indexed by the quest class list's own
    order, and each byte is read from the commander's IES property named after
    the class (e.g. 'Quest_00001'). So this list is the bridge between the
    array position and the stored state. Parsed once from the converted XML
    and cached.
    """
    global _QUEST_CLASS_NAMES
    if _QUEST_CLASS_NAMES is not None:
        return _QUEST_CLASS_NAMES
    path = ros.path.join(GAME_ROOT, "ge", "xml", "datatable_questinfo.xml")
    out = []
    try:
        import xml.etree.ElementTree as ET
        root = ET.parse(path).getroot()
        out = [c.get("ClassName") for c in root.findall("Class")
               if c.get("ClassName")]
    except Exception:
        out = []
    _QUEST_CLASS_NAMES = out
    return _QUEST_CLASS_NAMES


def quest_checkup_index():
    global _QUEST_CHECKUP
    if _QUEST_CHECKUP is not None:
        return _QUEST_CHECKUP
    import xml.etree.ElementTree as ET
    out = {}
    path = os.path.join(GAME_ROOT, "ge", "xml", "datatable_questcheckup.xml")
    try:
        root = ET.parse(path).getroot()
        for c in root.findall("Class"):
            qn = c.get("QuestName")
            st = c.get("Stage")
            cid = c.get("ClassID")
            if not qn or not st or not cid:
                continue
            try:
                key = (qn, int(st))
                cid = int(cid)
            except (TypeError, ValueError):
                continue
            tms = [c.get("TargetMonster%d" % i) for i in (1, 2, 3)]
            tms = [t if t and t != "None" else None for t in tms]
            out[key] = (cid, tms)
    except Exception:
        out = {}
    _QUEST_CHECKUP = out
    return _QUEST_CHECKUP


def quest_count():
    """How many quests the CLIENT's class list holds.

    The packet below is ONE BYTE PER QUEST in that exact order, so this number
    is the whole payload. It is read from the table's header rather than
    hardcoded, because it moves when the client's data moves: the live server
    sends 4352 bytes while our table has 4130 rows (it is one content step
    ahead, and the extra rows are invisible to this client).
    """
    import npc_data
    path = os.path.join(npc_data.IES_DIR, "datatable_questinfo.ies")
    try:
        with open(path, "rb") as fh:
            head = fh.read(96)
    except OSError:
        return 0
    if head[:5] != b"Quest":            # that file IS the Quest class list
        return 0
    # +82 is the row count (WORD, little-endian); +84 is the COLUMN count (64),
    # which is easy to mistake for it - ask us how we know.
    return struct.unpack_from("<H", head, 82)[0]


def build_quest_info(commander_id=None):
    """PKS_ZC_QUEST_INFO: cmd(2)+len(2)+one byte per quest.

    There are NO ids in this packet - the byte's POSITION is the quest, so both
    sides must walk the same class list in the same order (see
    history\\quests.md, measured from the live server). States seen live:
    0 available, 1 in progress, 100 completed, 0xC8 not reached.

    Real state now comes from the commander's IES properties in the DB
    (COMMANDER_PROPERTY joined to MASTER_PROPERTY): each quest class name
    (e.g. 'Quest_00001') is a property whose value is the state byte. The
    file knob (config\\quest_state.txt) still overrides on top, for testing.
    """
    count = quest_count()
    states = bytearray([QUEST_STATE_UNAVAILABLE]) * count
    cid = commander_id if commander_id is not None else getattr(CUR, "commander_id", 0)
    if cid:
        try:
            import ge_db
            props = ge_db.get_commander_quest_states(cid)
            names = quest_class_names()
            for i in range(min(count, len(names))):
                name = names[i]
                if name in props:
                    states[i] = props[name] & 0xFF
        except Exception:
            pass
    for idx, state in quest_state_overrides().items():
        if 0 <= idx < count:
            states[idx] = state & 0xFF
    body = struct.pack("<HH", CMD_ZC_QUEST_INFO, 4 + count)
    return body + bytes(states)


def build_quest_stage_info(commander_id=None):
    cid = commander_id if commander_id is not None else getattr(CUR, "commander_id", 0)
    records = bytearray()
    if cid:
        try:
            import ge_db
            rows = ge_db.get_quest_state_rows(cid)
            groups = {}
            for name, stage, obj, count in rows:
                groups.setdefault((name, stage), []).append((obj, count))
            qc = quest_checkup_index()
            for (name, stage), objs in groups.items():
                info = qc.get((name, stage))
                if not info:
                    continue
                type_id, tms = info
                kc = [0, 0, 0]
                for idx, tm in enumerate(tms):
                    if tm is None:
                        continue
                    for obj, count in objs:
                        if obj == tm:
                            kc[idx] = count & 0xFFFF
                rec = struct.pack("<I", type_id) + bytes([0, 0x7f, 0, 0, 0, 0])
                rec += struct.pack("<H", kc[0] if kc else 0)
                records += rec
        except Exception:
            records = bytearray()
    count = len(records) // 12
    if count == 0:
        return struct.pack("<HHHB", CMD_ZC_QUEST_STAGE_INFO, 7, 0, 1)
    body = struct.pack("<HHHB", CMD_ZC_QUEST_STAGE_INFO, 7 + len(records), count, 1) + bytes(records)
    return body



def build_inventory_cashitem_list():
    """PKS_ZC_INVENTORY_CASHITEM_LIST: cmd+len+count(DWORD). Empty (count=0)."""
    return struct.pack("<HHI", CMD_ZC_INVENTORY_CASHITEM_LIST, 8, 0)


def build_equip_cashitem_list(char_index=0):
    """PKS_ZC_EQUIP_CASHITEM_LIST: cmd+len+charIndex(BYTE)+pad(3)+count(DWORD). Empty."""
    return (struct.pack("<HH", CMD_ZC_EQUIP_CASHITEM_LIST, 12)
            + bytes([char_index & 0xFF]) + b"\x00" * 3
            + struct.pack("<I", 0))


def build_lumin_cashinfo_list():
    """PKS_ZC_LUMIN_CASHINFO_LIST: cmd+len+count(DWORD). Empty (count=0)."""
    return struct.pack("<HHI", CMD_ZC_LUMIN_CASHINFO_LIST, 8, 0)


def build_equip_lumin_cashinfo_list(char_index=0):
    """PKS_ZC_EQUIP_LUMIN_CASHINFO_LIST: cmd+len+charIndex(BYTE)+pad(3)+count(DWORD). Empty."""
    return (struct.pack("<HH", CMD_ZC_EQUIP_LUMIN_CASHINFO_LIST, 12)
            + bytes([char_index & 0xFF]) + b"\x00" * 3
            + struct.pack("<I", 0))


def build_commander_init_info():
    """PKS_ZC_COMMANDER_INIT_INFO: PKS_VARSIZE(4)+4 bools+3 BYTEs+WP_GLOBAL_PROP(1)+CLIQUEWAR_STATE(2)+VarConfCount(1)."""
    # UseSecondPassword, NewPartyBoardMemo, IsWorldPVPServer, RecvSquadEffect
    # WarnReinf, WarnAR, WarnDR, WPGlobalProp(NowPVPPlaying), cliqueWarState(2 bools), VarConfCount
    body = struct.pack("<HHBBBBBBB", CMD_ZC_COMMANDER_INIT_INFO, 15,
                      0, 0, 0, 0, 0, 0, 0)
    body += bytes([0])  # WP_GLOBAL_PROP.NowPVPPlaying
    body += bytes([0, 0])  # CLIQUEWAR_STATE: isEnterable, isPlayable
    body += bytes([0])  # VarConfCount
    return body


def build_commwarp_list(teltype):
    """PKS_ZC_COMMWARP_LIST empty: cmd+len+teltype(1)+curCount(1) = 6."""
    return struct.pack("<HHBB", CMD_ZC_COMMWARP_LIST, 6,
                       int(teltype) & 0xFF, 0)


def build_pet_list():
    """PKS_ZC_COMMANDER_PET_LIST empty: cmd+len+count(WORD) = 6."""
    return struct.pack("<HHH", CMD_ZC_COMMANDER_PET_LIST, 6, 0)


# ---------------------------------------------------------------------------
# Town NPCs (trigger objects)
# ---------------------------------------------------------------------------
# PKS_ZC_SPAWN_STANDINGTRIGGER is a FIXED 109-byte packet in the shipped client
# (its own packet table says 109). The layout below is NOT source2's - it was
# read out of the client's handlers FUN_004f35a3 (Enter) and FUN_004f424e
# (Spawn) in ge.exe:
#     LEA EAX,[EDI+0x24]      -> appearance.name
#     LEA ESI,[EDI+0x1c]      -> &appearance
#     MOV EAX,[EDI+0xf/0x13/0x17] -> pos.x / pos.y / pos.cell
#     MOVZX EAX,byte [EDI+0x1b]   -> direction
#     LEA EAX,[EDI+0x2]           -> &handle
# So: handle@2, pos{x,y,cell}@15, direction@27, APPEARANCE_TRIGGER@28.
# Bytes 6..14 are dead in this build (the handler never reads them) - zero them.
# APPEARANCE_TRIGGER = DWORD type; DWORD range; char name[65]; DWORD genId;
# DWORD pcShape = 81 bytes (confirmed: ZC_UPDATED_TRIGGERAPPEARANCE is 87 =
# cmd(2) + handle(4) + appearance(81)).
TR_HANDLE = 2
TR_UNKNOWN = 6               # 9 bytes the shipped handler ignores
TR_POS = 15                  # x, y, cell (3 x i32)
TR_DIRECTION = 27
TR_APPEARANCE = 28
TR_SIZE = 109

AP_TR_TYPE = 0               # DWORD: ClassID in the "NPC" table
AP_TR_RANGE = 4              # DWORD (the client reads it as a float)
AP_TR_NAME = 8               # char[65]
AP_TR_GENID = 73             # DWORD: NPCGen_<world> ClassID (used for the caption)
AP_TR_PCSHAPE = 77           # DWORD
AP_TR_SIZE = 81

NPC_NAME_LEN = 65


def build_spawn_trigger(handle, pos, direction, npc_type, gen_id=0,
                        name="", npc_range=0, pc_shape=0):
    """One 109-byte ZC_SPAWN_STANDINGTRIGGER (a town NPC).

    pos = WORLDPOS{x, y, CELL}. `cell` is a navmesh cell index, NOT a height -
    the client turns it into a ground position (CWorld::ConvertPosition asserts
    the cell is valid), so a wrong cell is either a wrong height or an assert.
    `npc_type` = ClassID in the client's "NPC" table (datatable_npc.ies).
    `name` is the caption drawn under the NPC; send it (see `npc_caption`) because
    the client's own fallback lookup - NPCGen table's Name for `gen_id` - is only
    correct when our npcgen rows and the client's are the same version.
    """
    b = bytearray(TR_SIZE)
    struct.pack_into("<H", b, 0, CMD_ZC_SPAWN_STANDINGTRIGGER)
    struct.pack_into("<I", b, TR_HANDLE, handle & 0xFFFFFFFF)
    struct.pack_into("<iii", b, TR_POS, int(pos[0]), int(pos[1]), int(pos[2]))
    b[TR_DIRECTION] = direction & 0xFF
    a = TR_APPEARANCE
    struct.pack_into("<I", b, a + AP_TR_TYPE, int(npc_type) & 0xFFFFFFFF)
    struct.pack_into("<I", b, a + AP_TR_RANGE, int(npc_range) & 0xFFFFFFFF)
    if name:
        nb = name.encode("latin-1", "replace")[:NPC_NAME_LEN - 1]
        b[a + AP_TR_NAME:a + AP_TR_NAME + len(nb)] = nb
    struct.pack_into("<I", b, a + AP_TR_GENID, int(gen_id) & 0xFFFFFFFF)
    struct.pack_into("<I", b, a + AP_TR_PCSHAPE, int(pc_shape) & 0xFFFFFFFF)
    assert len(b) == TR_SIZE
    return bytes(b)


def log(msg):
    # Several players can be served at once, so the log lines are written under
    # a lock - otherwise two threads can interleave half a line each.
    with LOG_LOCK:
        print("[%s] %s" % (datetime.datetime.now().strftime("%H:%M:%S"), msg),
              flush=True)


def hexs(b):
    return " ".join("%02X" % x for x in b)


def team_file_for(commander_id):
    """The barrack's team handoff file for one player.

    The barrack writes one file per commander, so two players entering at the
    same time cannot overwrite each other's team. The old single file
    (zone_team.json) is still read as a fallback.
    """
    if not commander_id:
        return TEAM_FILE
    return os.path.join(OUT, "zone_team_%s.json" % commander_id)


def _read_team_file():
    """The slots the barrack saved on CB_STARTGAME, or None.

    Two sources, in order:

    1. PostgreSQL (`pending_zone_entry`) - what the GO barrack writes. This is the
       bridge: one row per commander, so two players entering at once cannot
       overwrite each other.
    2. the JSON file - what the PYTHON barrack still writes, i.e. the oracle path.

    The file stays as the fallback on purpose: the Python barrack must keep
    working unchanged, and a PostgreSQL hiccup must cost us the row, not the login.
    """
    data = None
    try:
        import pg_read
        if pg_read.available():
            data = pg_read.pending_zone_entry(CUR.commander_id)
    except Exception as exc:  # driver present but the read failed
        log("  !! pending_zone_entry unreadable (%s) - using the team file" % exc)
    if data is None:
        for path in (team_file_for(CUR.commander_id), TEAM_FILE):
            try:
                with open(path, "r", encoding="utf-8") as f:
                    data = json.load(f)
                break
            except (OSError, ValueError):
                continue
    if data is None:
        return None
    picked = [s for s in data.get("slots", []) if s != 255]
    if not picked and data.get("picked"):
        picked = list(data["picked"])
    if picked:
        return picked
    return None


def _read_continue_pos():
    r"""The position a CONTINUEGAME asked for, or None.

    The original server keeps this server-side: CCommander::GetContGameZoneInfo
    reads GetCharacterSlot()->GetContinuePos() -> m_continueZonePos, which is set
    from character->point.x/y/z (source2\geServer\BarrackServer\Commander.cpp:398
    and CharacterSlot.cpp:126). So the ZONE is the right owner of it - it is the
    process that knows where everyone stands, and the one that writes the position
    down in the first place (save_position on CZ_MOVE_PC and on leave).

    Two sources, in order:

    1. the handoff's `continue_pos` - what the PYTHON barrack writes into the
       JSON file (it reads our CHARACTER_POSITION table and copies it across);
    2. OUR OWN `CHARACTER_POSITION` lookup - for the GO barrack, which only says
       "this is a continue" (`pending_zone_entry.is_continue`) and does not carry
       a position. Before this, a Continue Game started through the Go barrack
       spawned at the map default: the file was absent, `data` was None, and the
       saved position was never consulted even though we were the ones holding it.
    """
    # A `pending_zone_entry` row means the GO barrack served this entry. Its writer
    # never touches the JSON handoff, so any file present is a LEFTOVER from a
    # Python run - and a leftover wins if we read the file first, which is exactly
    # what happened on 2026-10-25: the zone resumed at the Python session's point
    # (4056, 595) instead of the player's real last spot.
    #
    # So: the row decides. If there is one and it says continue, the position comes
    # from the database and the file is not consulted at all.
    try:
        import pg_read
        if pg_read.available():
            entry = pg_read.pending_zone_entry(CUR.commander_id)
            if entry is not None:
                if not entry.get("continue"):
                    return None
                pos = _saved_position_of_team()
                if pos is not None:
                    return pos
                log("  .. continue: no saved position in the database - falling back "
                    "to the map's default spawn")
                return None
    except Exception as exc:
        log("  !! pending_zone_entry unreadable (%s) - using the team file" % exc)

    for path in (team_file_for(CUR.commander_id), TEAM_FILE):
        try:
            with open(path, "r", encoding="utf-8") as f:
                data = json.load(f)
        except (OSError, ValueError):
            continue
        if not data.get("continue"):
            continue
        pos = data.get("continue_pos")
        if pos and len(pos) == 3:
            return tuple(pos)

    # No file, or a file without a position: ask the database we already write.
    return _saved_position_of_team()


def _saved_position_of_team():
    r"""The saved point of the first picked character that has one, or None.

    `CHARACTER_POSITION` is written by this process (save_position), so a player
    who has moved at all has a row. A character that has never moved has none -
    then Continue falls back to the map's default spawn, which is honest.

    NOTE the call order: load_players() asks for this BEFORE it assigns CUR.team,
    so relying on CUR.team alone returned None and every Continue silently fell
    back to the map spawn (measured 2026-10-25: "no saved position in the
    database" while the row was right there). When CUR.team is empty we load the
    commander's characters ourselves and PREFER the slots the player picked - which
    is what the original does (CCommander::GetContGameZoneInfo reads the picked
    character slot).
    """
    try:
        import ge_db as _db
    except Exception:
        return None

    members = [m for m in (CUR.team or []) if m.get("character_id")]
    if not members:
        try:
            chars = _db.get_characters(CUR.commander_id) or []
        except Exception as exc:
            log("  !! could not read the commander's characters: %s" % exc)
            return None
        try:
            picked = set(_read_team_file() or [])
        except Exception:
            picked = set()
        members = ([c for c in chars if c.get("slot") in picked] +
                   [c for c in chars if c.get("slot") not in picked])

    for member in members:
        cid = member.get("character_id")
        if not cid:
            continue
        try:
            got = _db.get_character_position(cid)
        except Exception as exc:
            log("  !! could not read the saved position of %s: %s" % (cid, exc))
            continue
        if not got:
            continue
        x, y, cell = got[0], got[1], got[2]
        map_id = got[3] if len(got) > 3 else 0
        # A position from ANOTHER map cannot be resumed here; the cell would mean
        # nothing. Say so and let the default spawn win.
        if map_id and CUR.map_id and int(map_id) != int(CUR.map_id):
            log("  .. saved position of %s is on map %s, not %s - ignoring it"
                % (cid, map_id, CUR.map_id))
            continue
        log("  .. continue: saved position of %s is (%s, %s, cell=%s)"
            % (cid, x, y, cell))
        return (int(x), int(y), int(cell))
    return None


def _continue_spawn(cont_pos):
    r"""The (x, y, cell) to spawn at for a CONTINUEGAME, or None.

    The saved position ALREADY carries the navmesh cell: the client sent it in
    CZ_MOVE_PC and the zone stored it as-is (ge_db.set_character_position).
    So there is nothing to reverse-look up - the cell is the wire's own
    coordinate.

    The cell is re-validated with the client's path engine, because a cell
    from a DIFFERENT map is meaningless here: if the character moved map, the
    saved cell belongs to the old map's navmesh and the point engine will
    find no ground for it. None means "cannot resume there", and the caller
    falls back to the map spawn - and says so in the log, because a silent
    fallback is a bug that hides itself.
    """
    x, y, cell = cont_pos
    try:
        # A local import, like the other npc_data users in this file: it
        # loads the client's path engine, which is slow to import. Without it
        # this raised NameError, the `except` below swallowed it, and EVERY
        # "continue" fell back to the map spawn with a confusing message.
        import npc_data
        got = npc_data.resolve_cells([(x, y, 0)], h_range=500, v_range=300)
    except Exception as exc:
        log("  continue: could not validate the saved cell %s for (%s, %s) (%s) "
            "- using the map spawn" % (cell, x, y, exc))
        return None
    if not got or got[0] is None:
        log("  continue: the saved cell %s has no ground in THIS map - "
            "using the map spawn" % (cell,))
        return None
    log("  continue: resuming at (%s, %s) cell %s" % (x, y, cell))
    return (x, y, cell)


_ZONE_POINTS = {}


def _zone_points():
    """The per-map landing table, `data\\zone_points.json`, or {} if unreadable.

    ONE source for BOTH servers: `src\\emit_zone_points.py` resolves a map's
    spawn point to a navmesh CELL with the client's own path engine, so a map
    with no entry here has no known landing spot - and guessing one drops the
    team through the ground or crashes the client inside PathEngine.

    Loaded once, on the first spawn, and never at import time: a zone that
    starts for map 10 must not pay for reading a 460 KB file it may not need.
    """
    if not _ZONE_POINTS:
        import json
        import ge_paths
        path = os.path.join(ge_paths.DATA, "zone_points.json")
        try:
            with open(path, "r", encoding="utf-8") as f:
                _ZONE_POINTS.update(json.load(f).get("maps") or {})
        except (OSError, ValueError) as exc:
            log("  spawn: could not read %s (%s) - falling back to MAP_SPAWN"
                % (path, exc))
    return _ZONE_POINTS


def resolve_spawn(zone_id):
    """WORLDPOS{x,y,cell} to spawn the team at, or None to use the DB point."""
    # 1) explicit override file "x y cell"
    try:
        with open(SPAWN_FILE, "r", encoding="utf-8") as f:
            parts = f.read().split()
        if len(parts) >= 3:
            pos = tuple(int(float(p)) for p in parts[:3])
            log("  spawn override %s -> %s" % (SPAWN_FILE, pos))
            return pos
    except (OSError, ValueError):
        pass
    # 2) the per-map table both servers share. A null cell means the path
    #    engine found no ground near the point, which is NOT "aim at 0":
    #    fall through and let the log say the map has no spawn.
    row = _zone_points().get(str(zone_id)) or {}
    spawn = row.get("spawn") or {}
    if spawn.get("x") is not None and spawn.get("cell") is not None:
        pos = (int(spawn["x"]), int(spawn["y"]), int(spawn["cell"]))
        log("  spawn: map %s (%s) -> %s from zone_points.json (%s)"
            % (zone_id, row.get("name"), pos, spawn.get("source")))
        return pos
    # 3) the harvested map spawn
    if zone_id in MAP_SPAWN:
        return MAP_SPAWN[zone_id]
    return None


def npc_caption(npc):
    """The label under a town NPC, as plain text.

    The client draws an EMPTY `name` by looking the caption up in its OWN
    `NPCGen_<world>` table, keyed by the `gen_id` we send. That only works while
    our npcgen rows and the client's agree. They used not to: `npcgen_reb_basic`
    had 83 rows in the v32 data and 119 in the 2026 Andromida one, only 46 of
    the old ids still existed there, and NONE of those 46 even kept the same
    Name value - so 37 NPCs came up with no label at all and the rest would
    get whatever name the new table now holds for a reused id.

    Since 2026-10-22 our rows ARE the client's rows (R36): `ge\ies` was
    refreshed to the client's own generation, so the ids and the captions
    agree by construction. We still send the caption ourselves rather than
    leaving it empty, because that is what makes the label right on a client
    whose dictionary differs from ours.

    `npcgen` stores the caption as a locale token (`<$>64889</>`), which
    `ge_locale` resolves against the dictionary; a row with no Name at all
    sends an empty caption, so the client's own lookup still runs for it
    exactly as before.
    """
    text = (npc.get("name") or "").strip()
    if text and text != "None":
        try:
            import ge_locale
            text = ge_locale.resolve(text).strip()
        except Exception as exc:
            log("  !! ge_locale failed on npc name %r: %s" % (text, exc))
    if not text or text.startswith("<$>"):
        # No Name in our data (or a token the v32 dictionary cannot resolve):
        # send an empty caption so the client keeps its own lookup, rather than
        # inventing a label from the ClassType.
        return ""
    return text


def build_npc_packets(zone_id):
    """The ZC_SPAWN_STANDINGTRIGGER packets for the map's town NPCs."""
    out = []
    for i, npc in enumerate(zone_npcs(zone_id)):
        handle = NPC_HANDLE_BASE + i
        pk = build_spawn_trigger(handle, (npc["x"], npc["y"], npc["cell"]),
                                 npc["direction"], npc["npc_type"],
                                 gen_id=npc["gen_id"],
                                 name=npc_caption(npc))
        out.append(("ZC_SPAWN_STANDINGTRIGGER", pk))
    return out


def npc_handle_index(handle):
    """NPC index inside zone_npcs() for a click handle; None if not one of ours."""
    if not (NPC_HANDLE_BASE <= handle < NPC_HANDLE_BASE + len(zone_npcs(ZONE_ID))):
        return None
    return handle - NPC_HANDLE_BASE


# The shop window. Turn this OFF if it misbehaves in game. (It was briefly off on
# 2026-10-16 after a disconnect on OK - but that turned out to be a plain bug here:
# the OK handler assigned CUR.pending_shop without declaring it global, so the
# UnboundLocalError killed the session. The packet itself is accepted: the client
# opened the window from it, just with no items, which is the id problem
# described below.)
SHOP_TRADE_ENABLED = True
# The custom-shop window (3681) is NOT enabled yet: the shipped build's header is
# not the 80 bytes source2 declares (opening it crashed the client at
# VA 0x5ad488 - an item lookup with a garbage id, i.e. the records start at the
# wrong offset). These are EXCHANGE shops anyway (CLAUD: a Crecemento weapon for
# ShinyCrystal), so nothing can be bought until the trade flow exists.
# GE_CUSTOM_SHOPS=1 turns the window on for experiments.
CUSTOM_SHOPS_ENABLED = os.environ.get("GE_CUSTOM_SHOPS", "1") != "0"

# Which struct the trade packet uses. MEASURED facts, in order:
#   * source2's packed form (13 bytes: type@5, count@9, items@13) is WRONG for this
#     client - it opened the trade window with BOTH sides empty.
#   * v28's ZoneServer_Release.exe (2019, unpacked, keeps its asserts) shows
#     `Allocate(sizeof(PKS_ZC_DIALOG_NPC_TRADE))` compiling to `PUSH 16`, i.e. the
#     UNPACKED form: cmd(2) len(2) isCash(1) pad(3) type(4) count(4), items@16.
#   * the 16-byte form populated the trade window's PLAYER side and left the SHOP
#     side empty - and that is informative: type@8 must be right (the player's list
#     is built from `normalTradeType`), but if `count` were also right the shop side
#     would show SHIFTED items (our ids read 4 bytes late are still valid ids), not
#     nothing. So `count` is NOT at +12: the newer build has extra DWORD(s) between
#     type and count, and that is what the `NormalItem_Tax` shop type in the NPC
#     scripts points at (note SHOP_SELLCOIN_TYPE even has an unused third value).
# Layouts, `GE_TRADE_LAYOUT`:
#   unpacked16 - cmd len isCash pad3 type count            items@16  (v28's size)
#   unpacked20 - ... type EXTRA count                      items@20  (DEFAULT, test)
#   unpacked24 - ... type EXTRA EXTRA count                items@24
#   packed     - source2's 13-byte form, kept only for A/B
TRADE_LAYOUT = os.environ.get("GE_TRADE_LAYOUT", "group40")

_EXTRA_DWORDS = {"unpacked16": 0, "unpacked20": 1, "unpacked24": 2}

HERE = os.path.dirname(os.path.abspath(__file__))
LAYOUT_FILE = os.path.join(CONFIG, "trade_layout.txt")


def _trade_layout():
    """The layout in use, re-read per packet.

    Read from `trade_layout.txt` first so a layout can be switched WITHOUT
    restarting the server - which also means without disconnecting the player, who
    then only has to click the NPC again instead of relogging (the barrack costs
    ~57 s). Falls back to GE_TRADE_LAYOUT / the default.
    """
    try:
        with open(LAYOUT_FILE, "r", encoding="utf-8") as fh:
            v = fh.read().strip()
        if v:
            return v
    except OSError:
        pass
    return TRADE_LAYOUT


def build_trade_dialog(items, group=None):
    """PKS_ZC_DIALOG_NPC_TRADE: the NPC shop window.

    GROUND TRUTH, captured 2026-10-17 from the live Andromida server with pktmon
    (`group40`, the DEFAULT): the packet carries the shop GROUP NAME and NO item ids
    at all. Two samples, both 40 bytes:

        cmd=3680 len=40 | 00 00 01 00 00 00 00 | "ETC_LEONARDO"   | zeros
        cmd=3680 len=40 | 00 00 01 00 00 00 00 | "BLACKSMITH_SHOP"| zeros

    So the newer server names the shop group and the CLIENT resolves the stock from
    its own shop table. That is why every attempt to send an item LIST failed: the
    client was not reading a list, and `items` is unused in this layout. Our groups
    exist in the client's own table (measured: ETC_LEONARDO 13/13, REB_ITEMSHOP
    51/51, BLACKSMITH_SHOP 26/26, PERIOD_COSTUMESHOP7 15/15 of our ids present).

    Other layouts are kept for reference only:
      unpacked16/20/24 - the v28 (2019) shape: cmd len isCash pad3 type count, items
      packed           - source2's 13-byte form
    """
    items = [int(i) for i in items]
    n = len(items)
    lay = _trade_layout()
    if lay == "group40":
        # Byte-for-byte the captured packet: 7-byte prefix, the group name, NUL, then
        # zero fill to the observed total of 40 bytes.
        name = (group or "").encode("latin-1", "replace")[:28]
        body = b"\x00\x00\x01\x00\x00\x00\x00" + name + b"\0"
        if len(body) < 36:
            body += b"\0" * (36 - len(body))
        return struct.pack("<HH", CMD_ZC_DIALOG_NPC_TRADE, 4 + len(body)) + body
    if lay == "packed":
        body = struct.pack("<BiI", 0, int(SHOP_VIS), n)
    else:
        extra = _EXTRA_DWORDS.get(lay, 1)
        body = (struct.pack("<B3xi", 0, int(SHOP_VIS))
                + b"\0" * (4 * extra)
                + struct.pack("<I", n))
    for it in items:
        body += struct.pack("<I", it)
    total = 4 + len(body)                      # PKS_VARSIZE is command(2)+len(2)
    return struct.pack("<HH", CMD_ZC_DIALOG_NPC_TRADE, total) + body


def build_custom_trade_dialog(entries):
    """PKS_ZC_DIALOG_NPC_CUSTOM_TRADE (3681): a custom shop's item list.

    MEASURED against a live reference server (2026-10-17): 80-byte header then
    **33-byte** records, not source2's 20:
        cmd(2) len(2) normalTradeType(4) sellCount(4) CustomShopType(4)
        PropName[64]
        sellCount * {DWORD BuyItem; DWORD PriceItem[2]; int Count[2]; char pad[13]}
    The reference's own packet for CLAUD was 773 bytes = 80 + 21*33, with ids
    7201/21450 in the first record. Source2's 20-byte stride is what crashed the
    client (records landed 13 bytes apart from where it read them).
    """
    # CUSTOM_SHOP_INFO order is {BuyItem, PriceItem[0], PriceItem[1], Count[0],
    # Count[1]} while npc_data returns (sell, price1, count1, price2, count2).
    recs = b"".join(struct.pack("<IIiii", int(sell), int(p1), int(p2),
                                 int(c1), int(c2)) + b"\0" * 13
                    for sell, p1, c1, p2, c2 in entries)
    head = (struct.pack("<IIi", int(SHOP_VIS), len(entries), CUSTOM_SHOP_NORMAL)
            + b"\0" * 64)
    total = 4 + len(head) + len(recs)
    return (struct.pack("<HH", CMD_ZC_DIALOG_NPC_CUSTOM_TRADE, total)
            + head + recs)


def _lobby_request():
    """The lobby a SCRIPT asked for: `(type, layer, allowCreate)` or None.

    Drained, like `_trade_shop`: the click that asked for it has already run and
    the next click must not reopen the window.
    """
    host = _host()
    events = getattr(host, "lobby_events", None) or []
    host.lobby_events = []
    return events[-1] if events else None


def _trade_shop():
    """The window a SCRIPT asked for, in the same shape `npc_shop_items` builds.

    Drained, not peeked: the branch that asked for it has already run, and the
    next click must not reopen it. This is the piece a menu NPC needs - for
    Leonardo the shop is option 3, so deciding it by scanning the file would open
    the window before the player has chosen anything.
    """
    host = _host()
    events = getattr(host, "trade_events", None) or []
    host.trade_events = []
    if not events:
        return None
    kind, group = events[-1]
    if kind == "warehouse":
        log("  !! the script asked for the warehouse - not implemented yet, "
            "nothing sent")
        return None
    try:
        import npc_data
    except Exception as exc:
        log("  !! trade lookup failed: %s" % exc)
        return None
    if kind == "custom":
        if not CUSTOM_SHOPS_ENABLED:
            log("  custom shop %r: window disabled (GE_CUSTOM_SHOPS=0)" % group)
            return None
        entries = npc_data.load_custom_shop(group)
        if not entries:
            log("  !! custom shop %r has no rows in datatable_custom_shop.xml"
                % group)
            return None
        return {"kind": "custom", "group": group, "entries": entries, "items": []}
    return {"kind": "normal", "group": group,
            "items": npc_data.load_shop_items().get(group) or []}


def send_trade_window(conn, shop):
    """Open the window a script asked for, the way the ORIGINAL does it.

    `ShowNPCTradeDlg` in the shipped server sends the trade packet straight out
    (`PlayerCharFunc.cpp:615`), so the player sees the shop the moment they pick
    that option - no "press OK first" step. What it does NOT do is leave the
    dialog dangling; we close it here, which is the part the first attempt
    missed (the client stayed in dialog mode with no box to press OK on).
    """
    if shop["kind"] == "custom":
        pk = build_custom_trade_dialog(shop["entries"])
        CUR.last_shop = {"group": shop["group"], "kind": "custom",
                         "items": [e[0] for e in shop["entries"]],
                         "entries": list(shop["entries"])}
        log("  SENT ZC_DIALOG_NPC_CUSTOM_TRADE group=%s rows=%d bytes=%d "
            "(script branch, window straight away)"
            % (shop["group"], len(shop["entries"]), len(pk)))
    else:
        pk = build_trade_dialog(shop["items"], shop["group"])
        CUR.last_shop = {"group": shop["group"], "items": list(shop["items"])}
        log("  SENT ZC_DIALOG_NPC_TRADE group=%s items=%d bytes=%d "
            "(script branch, window straight away)"
            % (shop["group"], len(shop["items"]), len(pk)))
    conn.sendall(pk)


def npc_shop_items(handle):
    """The shop this NPC opens, or None when it is not a shop.

    Two kinds, two packets:
      * normal  - `ShowNPCTradeDlg(self, pc, "<GROUP>", type)`: send the group
        name; the client resolves stock and prices from its own table.
      * custom  - `ShowCustomNPCTradeDlg(self, pc, "<GROUP>")`: the server feeds
        the items. They come from server_xml/datatable_custom_shop.xml, which is
        exactly the table CShopTable::AddCustomProperty reads (GroupName,
        SellClassID, PriceClassID/2, Count/2), and go out as 20-byte
        CUSTOM_SHOP_INFO records (BuyItem, PriceItem[2], Count[2]).
    """
    idx = npc_handle_index(handle)
    if idx is None:
        return None
    if not SHOP_TRADE_ENABLED:
        return None
    row = zone_npcs(ZONE_ID)[idx]
    try:
        import npc_script
        import npc_data
        trade = npc_script.trade_for(row.get("script") or "")
        if not trade or not trade[1]:
            return None
        if trade[0] == "ShowCustomNPCTradeDlg":
            if not CUSTOM_SHOPS_ENABLED:
                log("  custom shop %r: window disabled (GE_CUSTOM_SHOPS=0), "
                    "answering with the dialog only" % trade[1])
                return None
            entries = npc_data.load_custom_shop(trade[1])
            if not entries:
                log("  !! custom shop %r has no rows in datatable_custom_shop.xml"
                    % trade[1])
                return None
            return {"kind": "custom", "group": trade[1],
                    "entries": entries, "items": []}
        if trade[0] != "ShowNPCTradeDlg":
            return None
        # The shop GROUP name is what the client needs now (see `group40` in
        # build_trade_dialog); the id list is kept for the older layouts and logging.
        return {"kind": "normal", "group": trade[1],
                "items": npc_data.load_shop_items().get(trade[1]) or []}
    except Exception as exc:
        log("  !! shop lookup failed on %r: %s" % (row.get("script"), exc))
        return None


def _host():
    """One Lua runtime for the whole process; scripts are global there."""
    global _script_host
    if _script_host is None:
        import script_host
        _script_host = script_host.ScriptHost(zone_id=ZONE_ID)
    return _script_host


def build_chat(handle, text, use_balloon=1):
    """PKS_ZC_CHAT: cmd(2) len(2) handle(4) permission(1) UseBalloon(1)
    BalloonColor(4) useChatUI(1), then the NUL-terminated text.

    The structs are `#pragma pack(1)` (`geZCPacket.h:948`), so the header is
    exactly 15 bytes and the text starts at 15 - which is what the client's
    reader assumes (`packetBuffer + sizeof(PKS_ZC_CHAT)`,
    `GENormalNet.cpp:1018`). The text is a locale TOKEN on purpose: the client
    runs `imc::ParseLocalize` on it itself.
    """
    body = struct.pack("<IBBIB", handle & 0xFFFFFFFF, CPL_USER,
                       1 if use_balloon else 0, 0, 1)
    body += str(text).encode("latin-1", "replace") + b"\0"
    return struct.pack("<HH", CMD_ZC_CHAT, 4 + len(body)) + body


def flush_chat_events(conn):
    """Send the lines a script made an NPC speak (`ChatNPC`).

    Queued rather than sent inside Lua: the branch decides whether the NPC
    speaks at all, and a line must not arrive before the dialog the player is
    still reading. `UseBalloon` is on, which is the only thing that makes the
    client draw the balloon over the actor.
    """
    host = _host()
    events = getattr(host, "chat_events", None)
    if not events:
        return
    host.chat_events = []
    for handle, text in events:
        if not handle:
            log("  !! ChatNPC with no handle ignored: %r" % (text,))
            continue
        conn.sendall(build_chat(handle, text))
        log("  SENT ZC_CHAT handle=0x%08x text=%r" % (handle, text))


def build_dialog_select(handle, prompt, options, dlg_type=DLG_WITH_FADE_IN):
    """PKS_ZC_DIALOG_SELECT: type(1) count(1) handle(4) then NUL-terminated text.

    Read off the CLIENT's own handler (`GENormalNet_Action.cpp`,
    `TriggerDialogSelect`): the first string is the prompt, then `count-1`
    option strings, each NUL-terminated.
    """
    body = struct.pack("<BBI", dlg_type & 0xFF, 1 + len(options),
                       handle & 0xFFFFFFFF)
    for text in [prompt] + list(options):
        body += str(text).encode("latin-1", "replace") + b"\0"
    return struct.pack("<HH", CMD_ZC_DIALOG_SELECT, 4 + len(body)) + body


def script_click(handle):
    """Start the NPC's script. The result may hold a QUESTION for the player."""
    idx = npc_handle_index(handle)
    if idx is None or not SCRIPT_ENGINE:
        return None
    row = zone_npcs(ZONE_ID)[idx]
    script = row.get("script") or ""
    if not script:
        return None
    try:
        host = _host()
        host.set_commander(getattr(CUR, "commander_id", 0) or DEFAULT_COMMANDER)
        host.commander_name = getattr(CUR, "family_name", None)
        # `GetPos(pc)` must answer the real spot, and `GetPos(self)` the NPC's.
        host.position = CUR.pos
        return host.start_click(script, self_pos=(row.get("x3d"),
                                                  row.get("y3d"),
                                                  row.get("z3d")),
                                self_handle=handle)
    except Exception as exc:
        log("  !! script %s failed: %s" % (script, exc))
        return None


def resume_script(conn, choice):
    """The player picked an option: let the script continue down that branch."""
    ps = getattr(CUR, "pending_script", None)
    if not ps:
        log("  CZ_DIALOG_SELECT with no script waiting (ignored)")
        return
    CUR.pending_script = None
    host = _host()
    res = host.resume_click(ps["co"], choice, ps["seen"])
    if not res["ok"]:
        log("  !! script resume failed: %s" % res["error"])
        conn.sendall(build_dialog_close())
        return
    # the branch the player chose may have committed a transaction
    flush_item_events(conn)
    flush_property_events(conn)
    flush_quest_events(conn)
    flush_setpos_events(conn)
    flush_chat_events(conn)
    if not res["done"]:
        q = res["question"]
        CUR.pending_script = {"co": res["co"], "handle": ps["handle"],
                              "seen": len(host.calls)}
        prompt = ge_locale.resolve_for_client(q["prompt"])
        options = [ge_locale.resolve_for_client(o) for o in q["options"]]
        conn.sendall(build_dialog_select(ps["handle"], prompt, options,
                                         q.get("fade", DLG_WITH_FADE_IN)))
        log("  SENT ZC_DIALOG_SELECT (next) prompt=%r options=%d"
            % (prompt, len(options)))
        return
    # a branch may end in a WINDOW rather than a line. Open it straight away and
    # send NOTHING else: captured from the live server (2026-10-23), the order is
    # DIALOG_SELECT -> NPC_TRADE -> (player closes the shop) -> DIALOG_CLOSE.
    # It does not send an OK box, and the close comes only after the window.
    shop = _trade_shop()
    if shop:
        send_trade_window(conn, shop)
        return
    # A branch may also ask for the MISSION LOBBY (`ShowLobby`). Same rule: the
    # click decided which lobby, so it is opened after the branch, not inside it.
    lobby = _lobby_request()
    if lobby is not None:
        lobby_type, _layer, create_ok = lobby
        CUR.lobby_type = lobby_type
        conn.sendall(build_lobby_room_list(CUR.account_id, _room_records(), bool(create_ok)))
        log("SENT ZC_LOBBY_ROOM_LIST lobby=%r rooms=0 create=%d (from ShowLobby)"
            % (lobby_type, create_ok))
        return
    if res.get("dialog"):
        text = ge_locale.resolve_for_client(res["dialog"])
        conn.sendall(build_dialog_ok(ps["handle"], text))
        log("  SENT ZC_DIALOG_OK (after choice %d) text=%r" % (choice, text))
    else:
        conn.sendall(build_dialog_close())
        log("  SENT ZC_DIALOG_CLOSE (script ended after choice %d)" % choice)


SCRIPT_ENGINE = os.environ.get("GE_SCRIPT", "1") != "0"   # on; 0 = off
_script_host = None


def script_dialog(script_name):
    """Run the NPC's own `SCR_CLICK_` script and take the line it asked for.

    On by default, and it is the ONLY source of the line - the old static
    reader is gone (`DECISIONS.md`). `GE_SCRIPT=0` turns it off for an A/B
    comparison. The builtins are stubs and record what the script wanted, but
    the line a script passes to `ShowOkDlg`/`ShowSelDlg` is the real one, which
    is the part a player sees.

    A script that raises or runs too long falls back to the old answer rather
    than taking the zone thread down with it.
    """
    if not SCRIPT_ENGINE or not script_name:
        return None
    try:
        res = _host().run_click(script_name)
        if res["error"]:
            log("  !! script %s: %s" % (script_name, res["error"]))
            return None
        if res["dialog"]:
            log("  script %s -> %r (%d calls)"
                % (script_name, res["dialog"], len(res["calls"])))
            return res["dialog"]
    except Exception as exc:
        log("  !! script host failed on %r: %s" % (script_name, exc))
    return None


def flush_item_events(conn):
    """Tell the client what a script's COMMITTED transaction did to the bag.

    Lesson 9 of the skill: a mid-session inventory change must be INCREMENTAL. A
    bulk `ZC_INVENTORY_ITEM_LIST` re-lays every slot (the bag visibly reshuffles)
    and leaves an open shop's counters stale, because only the incremental
    handlers call `UpdateTradeInfo()`.

    `ScriptHost` fills `item_events` only after a commit, so a transaction that
    rolled back sends nothing - which is what keeps the client's bag honest.
    """
    host = _host()
    events = getattr(host, "item_events", None)
    if not events:
        return
    host.item_events = []
    inv = CUR.last_inv if CUR.last_inv is not None else []
    for kind, cls, total, item_id in events:
        if kind == "add":
            idx = inv_add(inv, item_id, cls, total)
            conn.sendall(build_add_item(cls, idx, total, ADD_ITEM))
            log("  SENT ZC_ADDITEM class=%s invIndex=%d newTotal=%d"
                % (cls, idx, total))
        else:
            idx = None
            for i, row in enumerate(inv, 1):
                if row.get("item_id") == item_id:
                    idx = i
                    break
            if idx is None:
                log("  !! ZC_REMOVEITEM skipped: item %s is not in the client's "
                    "list" % item_id)
                continue
            conn.sendall(build_remove_item(idx, total, REMOVE_ITEM))
            inv_remove(inv, [(idx, total)])
            log("  SENT ZC_REMOVEITEM class=%s invIndex=%d newTotal=%d"
                % (cls, idx, total))


def flush_setpos_events(conn):
    """Move the player where a script's `SetPos` asked.

    `SetPos` is an INSTANT placement in the original (`OM_SET_POS` for a player,
    `OM_WARP` for a monster), not a walk: it puts a player on a battle spot, so
    there is no route and no arrival clock. One ZC_MOVE_PC to the new point is
    what the client applies. A monster's SetPos has no owner here (we do not
    simulate monsters yet), so it is logged rather than faked.
    """
    host = _host()
    events = getattr(host, "setpos_events", None)
    if not events:
        return
    host.setpos_events = []
    for is_player, pos in events:
        if not is_player:
            log("  script SetPos on a monster ignored: (%d, %d, %d)" % pos)
            continue
        if CUR.handle is None:
            continue
        CUR.pos = (int(pos[0]), int(pos[1]), int(pos[2]))
        CUR.move = None
        conn.sendall(build_move_pc(CUR.handle, CUR.pos))
        log("  SENT ZC_MOVE_PC (script SetPos) handle=%d -> (%d, %d, %d)"
            % ((CUR.handle,) + CUR.pos))


def flush_property_events(conn):
    """Tell the client when a COMMITTED transaction moved a Lock_* property.

    `Lock_InventoryCount` is the bag's capacity as the CLIENT understands it, and
    the client only learns it from this packet - without it, buying slots changes
    the database and the bag keeps showing its table default (248 here).

    Does NOT clear `prop_events` (flush_quest_events does), so call it FIRST.
    The value is read back from the DB, so it is the committed one, never the
    queued one.
    """
    host = _host()
    changed = [p for p in (getattr(host, "prop_events", None) or [])
               if str(p).startswith("Lock_")]
    if not changed:
        return
    import ge_db
    cid = getattr(CUR, "commander_id", 0) or DEFAULT_COMMANDER
    now = ge_db.get_commander_quest_states(cid)
    for name in changed:
        if name not in now:
            continue
        conn.sendall(build_update_property_number(name, now[name]))
        log("  SENT ZC_UPDATE_PROPERTY_NUMBER %s=%d" % (name, now[name]))


def flush_quest_events(conn):
    """Re-send ZC_QUEST_INFO when a script's COMMITTED transaction moved a quest.

    The panel is one byte per quest, and the server only sent that array at zone
    entry, so a quest accepted mid-session stayed invisible until the next
    relog. `ScriptHost.prop_events` holds the property names a commit really
    wrote; quest state lives in properties named after the quest class, so a
    name from that list means the array changed.
    """
    host = _host()
    props = getattr(host, "prop_events", None) or []
    host.prop_events = []
    if not props:
        return
    if not set(quest_class_names()).intersection(props):
        return
    pk = build_quest_info()
    conn.sendall(pk)
    log("  SENT ZC_QUEST_INFO (%d bytes) after quest %s changed"
        % (len(pk), ", ".join(sorted(set(props)))))


def npc_dialog_text(handle):
    """What a click on this NPC answers with.

    The real server runs the NPC's own script when it is clicked: CTriggerObject's
    script name is the npcgen row's `Script` column and the function is
    `SCR_CLICK_<Script>(self, pc)` (geDataCheck/source/geTestClass.cpp:377 names
    it). Those scripts are Lua with the whole game API behind them, so this port
    does not RUN them - `npc_script` reads the line the script would say for the
    simple cases and passes it through.

    The line is a locale token (`<$>7395</>`), not English: the CLIENT resolves it
    from its own dictionary when it draws the text, exactly as it already does for
    the caption above the NPC. An NPC with no click script, or one whose click
    opens a window this port does not have yet (a shop, the waypoint list), keeps
    the old caption-plus-placeholder answer.

    The token is resolved to English HERE rather than left to the client, because a
    swapped-in client build numbers its dictionary differently - the 2026 Andromida
    client moved the very same string from id 6261 to 10098913 - and an id it does
    not know is drawn as a bare number. See `ge_locale`.
    """
    idx = npc_handle_index(handle)
    if idx is None:
        return None
    row = zone_npcs(ZONE_ID)[idx]
    # NO static fallback on purpose: taking "the first dialog line in the file"
    # is the wrong line whenever the script branches on state, and a wrong line
    # reads as a confident lie. If the script cannot answer, say nothing.
    line = script_dialog(row.get("script") or "")
    if not line:
        name = row.get("name") or "..."
        # Both the `<$>locale</>` token and the `{br}` markup are resolved by the
        # text renderer the client already uses for the caption above the NPC:
        # CSayDialog::MakeSayText calls ui::CStaticText::SetTextInfo, the same call
        # the caption balloon makes.
        line = "%s{br}{br}Sorry, I have nothing to say yet." % name
    try:
        import ge_locale
        # `resolve_for_client` keeps a token AS a token when the client can
        # resolve it (the client then shows its OWN wording, exactly like the
        # original server), and falls back to literal text for the rest. The
        # plain `resolve` would turn everything into literal text, which also
        # works but shows the server pack's older wording.
        line = ge_locale.resolve_for_client(line)
    except Exception as exc:
        log("  !! ge_locale failed: %s" % exc)
    return line


def _dist2d(a, b):
    """Horizontal distance between two positions (the mesh is 2D for range)."""
    dx = b[0] - a[0]
    dy = b[1] - a[1]
    return (dx * dx + dy * dy) ** 0.5


def pc_position(now_ms=None):
    """Where the player actually is right now.

    CZ_MOVE_PC is sent when a walk STARTS and carries where it is headed, so
    treating it as "where the player is" puts the player at the far end the
    moment they set off - which is why the dialog used to open mid-walk. The
    real server owns movement and knows the speed, so do the same: keep the
    segment and work out the position from the clock.

    When the walk has a ROUTE (a path-engine polyline), the position is walked
    ALONG that polyline, not along the straight line between its ends. That
    matters: a route around a building is far longer than the chord, so a
    straight-line interpolation puts the player next to the destination long
    before they are - which opened a dialog half way down a curvy walk ("the
    dialog came early for the NPC with complex pathing").
    """
    if CUR.move is None:
        return CUR.pos
    start, dest, t0, eta = CUR.move[0], CUR.move[1], CUR.move[2], CUR.move[3]
    route = CUR.move[4] if len(CUR.move) > 4 else None
    now = _ms() if now_ms is None else now_ms
    if now >= eta or eta <= t0:
        return dest
    if route and len(route) > 1:
        travelled = (now - t0) / 1000.0 * move_speed()
        for a, b in zip(route, route[1:]):
            seg = _dist2d(a, b)
            if travelled <= seg:
                k = (travelled / seg) if seg else 0.0
                return (a[0] + (b[0] - a[0]) * k,
                        a[1] + (b[1] - a[1]) * k,
                        b[2])
            travelled -= seg
        return dest
    k = float(now - t0) / float(eta - t0)
    return (start[0] + (dest[0] - start[0]) * k,
            start[1] + (dest[1] - start[1]) * k,
            dest[2])


def pc_start_walk(dest, reported=False, length=None, route=None):
    """Note that the player is walking to `dest`.

    `reported` says where that came from: True for a CZ_MOVE_PC the CLIENT sent
    (it is really walking the actor), False for a chase move the server invented
    (see try_pending_click's client-walk deadline - the client animates a
    server-pushed move without necessarily translating the actor).

    The walk is anchored on where the player actually IS (pc_position), not on
    the place they were last headed for. That matters because the client
    retargets mid-stride constantly - its CZ_MOVE_PC destinations jump hundreds
    of units a second - while `gemove::ActorMove` only ever gets a destination
    and a speed, and times the walk from the actor's real position. Anchoring on
    the previous destination makes the remaining walk look shorter than it is,
    so the arrival time comes out EARLY and the dialog can then be answered while
    the player is still visibly walking. With the real position as the anchor the
    error is bounded by how far the player can travel between two reports
    (~1 s at the current move speed), instead of by the length of the last segment.

    `length` is how far the walk REALLY is, when the caller knows better than the
    straight line - a path around a building is longer than the line between its
    ends, and a straight-line ETA then says the player has arrived while they are
    still walking.
    """
    now = _ms()
    if reported:
        # The CLIENT is telling us it walks. Anchor on the position it told us
        # about LAST time: that is the last point we know it really stood on,
        # while the interpolated one is a guess that drifts while a walk we
        # invented is in flight (measured: the server thought the player was
        # halfway to the click while the client's own packets put it 2.7k units
        # away). Then remember this destination - it is the next anchor.
        start = CUR.last_client_pos or pc_position(now) or dest
        CUR.last_client_pos = dest
        CUR.last_client_pos_at = now
        # The client says where it is going, so the player IS heading there: the
        # position moves with it.
        CUR.pos = dest
    else:
        # A walk WE invented (a chase to a clicked NPC). The client may or may
        # not follow it, so its destination is NOT where the player is - it is
        # where we would like them to be. Keep CUR.pos on the last spot the
        # CLIENT confirmed, because CUR.pos is what gets written to the database
        # when the player leaves the zone.
        #
        # Measured (2026-10-21): the player stood at (6286, 425), clicked the
        # wagon Leonardo, and left the zone without arriving. The server had
        # chased to the 140-unit ring at (3342, -1498) and had stored THAT as
        # the player's position, so the next login resumed the character in
        # front of the NPC - "my character was right in front of the NPC, its
        # like im teleporting" - even though the player never went there.
        start = pc_position(now) or dest
    trip = length if length else _dist2d(start, dest)
    seconds = trip / move_speed()
    CUR.move = (start, dest, now, now + int(seconds * 1000), route)
    CUR.move_from_client = reported


def pc_walking(now_ms=None):
    """True while the client is still walking the player somewhere.

    Both the original and this port refuse to act on a click mid-stride: while
    the walk runs, CPlayerChar's AS_TRIGGER_CHASE bails out immediately
    (`if (MS_MOVE == GetMoveState()) return 0;`, PlayerChar_FSM.cpp:207) and
    only looks at the distance once the walk has finished. The client walks at
    the speed we send with ZC_MOVE_PC (CNormalNet::MovePC ->
    gemove::ActorMove(handle, spd, ...)), so the segment's own end time is when
    the player really stops.
    """
    if CUR.move is None:
        return False
    now = _ms() if now_ms is None else now_ms
    return now < CUR.move[3]


def npc_click_distance(handle):
    """2D centre-to-centre distance from the player to an NPC, or None.

    The real check subtracts both domain radii from the distance; those radii
    are already folded into NPC_CLICK_RANGE, so compare centre to centre.
    """
    idx = npc_handle_index(handle)
    # The client is silent while it walks a move we sent (it reports steps only
    # for the player's own walks), so the arrival has to be judged from OUR walk
    # clock. See the note in try_pending_click: waiting for a report here is what
    # made "no dialog on arrival" happen.
    pos = pc_position()
    if idx is None or pos is None:
        return None
    npc = zone_npcs(ZONE_ID)[idx]
    return _dist2d(pos, (npc["x"], npc["y"]))


def npc_click_range(longdistance):
    """How near the player must be for the click to be answered."""
    return NPC_CLICK_RANGE_LONG if longdistance else NPC_CLICK_RANGE


def client_walk():
    """True when the walk in flight is one the CLIENT reported.

    A CZ_MOVE_PC the client sent is proof it is walking the actor; a chase move
    this server invented is not (the client animates it without necessarily
    translating - see try_pending_click's walk_deadline). Used to seed a new
    pending click: if the player was already really walking when the NPC was
    clicked, the click can be trusted to answer on a real arrival.
    """
    return bool(CUR.move_from_client and pc_walking())


def remember_click(handle, longdistance):
    """Remember a clicked NPC; a click during a locked walk arms a redirect.

    Mirrors CPlayerChar::OnNotifyClickTrigger (PlayerChar_Message.cpp:468): it
    only sets the trigger target and switches to AS_TRIGGER_CHASE - nothing is
    answered yet.

    What a SECOND click does depends on which walk is running, and the
    difference is the action state:

      * the player is walking because of a ground click (state AS_NONE). The
        new state differs, so ON_ENTER runs and its `SetMoveState(MS_STOP)`
        (PlayerChar_FSM.cpp:192) ends that walk - the click TAKES OVER at once,
        redirecting the character from where it is now (the user: "when in the
        middle of walking, clicking NPC should redirect the character as well...
        right now it waits for the character to reach the destination").
      * the player is already being walked to a clicked NPC (state already
        AS_TRIGGER_CHASE). Setting the state it is ALREADY in does not run
        ON_ENTER again, so nothing stops the walk, and ON_UPDATE only returns
        early while MS_MOVE (line 207). The walk therefore stays LOCKED on the
        first NPC: a click on a different NPC only drops the dialog ("clicking
        another NPC while the character is still moving to the initial NPC will
        just cancel the dialog and not the move itself" - the user). Clicking
        THAT SAME other NPC again is what cancels the walk, which is the user's
        two-click rule: "the first click cancels the dialog and the second
        cancels and redirects the move".
    """
    if CUR.click_lock is not None and not pc_walking():
        CUR.click_lock = None                   # that walk is over; the lock is stale
    if CUR.click_lock is not None:
        if handle == CUR.click_lock["handle"]:
            # Same target as the one we are walking to: the state would do
            # nothing either, and a double click must not lose the dialog.
            CUR.pending_click = {"handle": handle, "long": bool(longdistance),
                             "give_up": _ms() + CLICK_STALL_MS, "tries": 0,
                             "next_move": 0, "chasing": True, "chase_ok": True,
                             "chase_from_start": client_walk(),
                             "client_walk": client_walk(),
                             "walk_deadline": 0, "redirect": False}
            log("  clicked 0x%08x again while walking to it - keeping the click"
                % handle)
        elif handle == CUR.click_lock["armed"]:
            # Second click on the same other NPC: NOW the walk is cancelled and
            # sent to it (the tick freezes the current walk and chases).
            CUR.pending_click = {"handle": handle, "long": bool(longdistance),
                             "give_up": _ms() + CLICK_STALL_MS, "tries": 0,
                             "next_move": 0, "chasing": False, "chase_ok": True,
                             "chase_from_start": client_walk(),
                             "client_walk": client_walk(),
                             "walk_deadline": 0, "redirect": True}
            log("  clicked 0x%08x twice while walking to 0x%08x -> redirecting"
                % (handle, CUR.click_lock["handle"]))
        else:
            CUR.click_lock["armed"] = handle
            CUR.pending_click = None
            log("  clicked 0x%08x while walking to 0x%08x -> dropping the dialog,"
                " keeping the walk (click it again to move there)"
                % (handle, CUR.click_lock["handle"]))
        return
    CUR.pending_click = {"handle": handle, "long": bool(longdistance),
                     "give_up": _ms() + CLICK_STALL_MS,
                     "tries": 0, "next_move": 0, "chasing": False,
                     # `chase_ok` is cleared the moment the CLIENT's own walk
                     # takes over (see note_player_move), so the server never
                     # fights the client's own pathing to the NPC.
                     "chase_ok": True,
                     # Whether the player was ALREADY walking when the NPC was
                     # clicked. Only then can the server chase: a move the server
                     # pushes on its own is animated but not translated, so a
                     # click from a standstill is left to the client's own AI.
                     "chase_from_start": client_walk(),
                     # True once the client has reported a walk for this click,
                     # i.e. the actor is really translating (see client_walk).
                     "client_walk": client_walk(),
                     "walk_deadline": 0,
                     # Walking from a GROUND click: that walk is in state AS_NONE,
                     # so the state DOES change and ON_ENTER's MS_STOP ends it -
                     # the click takes over at once instead of waiting for the
                     # player to arrive. See the tick's redirect handling.
                     "redirect": pc_walking()}
    log("  pending click on 0x%08x (long=%d)%s"
        % (handle, longdistance,
           " - cutting the walk short" if CUR.pending_click["redirect"] else ""))


# How often the character's position is written to the database while they
# walk around. The client reports a move every step, so writing on every move
# would be one pooled query per step per player - measurable with several
# players, pointless for a value only read on the way out. (This interval was
# a hard requirement in the `sqlcmd` era, when a write was a PROCESS spawn at
# ~150 ms. `ge_db` has used pyodbc with a pool since R31, so it is a safety net
# now, not a limit.) The position is kept in memory (CUR.pos) and written:
#   * at most once every SAVE_INTERVAL seconds while moving (a safety net if
#     the client crashes), and
#   * ALWAYS when the player leaves the zone - which is the moment the
#     "Last Point" button reads it, because the player is in the barrack by
#     then.
SAVE_INTERVAL = 5.0


def save_position(character_id, x, y, cell, map_id, force=False):
    """Write the character's position to the database, but not on every step.

    Returns True if it wrote. The write is skipped when the position has not
    moved since the last write and the last write is recent.
    """
    now = time.time()
    last = getattr(CUR, "last_saved_pos", None)
    when = getattr(CUR, "last_saved_at", 0.0)
    pos = (x, y, cell)
    if not force and last == pos and (now - when) < SAVE_INTERVAL:
        return False
    if not force and (now - when) < SAVE_INTERVAL:
        return False
    import ge_db as _db
    _db.set_character_position(character_id, x, y, cell, map_id)
    CUR.last_saved_pos = pos
    CUR.last_saved_at = now
    return True


def note_player_move(dest):
    """A walk the CLIENT started: either its own path step, or a ground click.

    Both arrive as the same packet (CZ_MOVE_PC), so the two have to be told
    apart by WHERE the walk goes:

      * the client's own AI, walking the player to a clicked trigger, reports
        steps that end short of the NPC but always CLOSER to it
        (`CNormalView::OnLBtnDownFocusedTriggerObject`, GENormalView.cpp:968, the
        `Click == TRUE` branch -> `SetMoveTgtPos` + `RunTacticsStateThread`), so
        a destination nearer the NPC keeps the click alive;
      * a GROUND CLICK aimed elsewhere ends FARTHER from the NPC, and that
        CANCELS the interaction, exactly like the original: `DispatchMove` ends
        with `SetActionState(AS_NONE)` (FieldChar.cpp:932) and AS_TRIGGER_CHASE
        is not a state `IsHoldNormalAction()` protects, so the chase stops.

    The menu the user reported was the second case leaking into the first: after
    a ground click the click stayed pending, the tick saw the player standing
    out of range with chases left, and walked them back to the NPC ("even when i
    cancel the path when i click the ground, my character still go back to the
    path", 2026-10-21). Cancelling here removes both the re-chase and any dialog
    that would have opened on the unwanted arrival.
    """
    if CUR.pending_click is not None:
        # The client is walking the actor itself: this click can now be trusted to
        # be answered on a real arrival.
        CUR.pending_click["client_walk"] = True
        handle, longdistance = CUR.pending_click["handle"], CUR.pending_click["long"]
    elif CUR.click_lock is not None:
        handle, longdistance = CUR.click_lock["handle"], CUR.click_lock["long"]
    else:
        return
    idx = npc_handle_index(handle)
    if idx is None:
        CUR.pending_click = None
        CUR.click_lock = None
        return
    npc = zone_npcs(ZONE_ID)[idx]
    to_npc = (npc["x"], npc["y"])
    dist_dest = _dist2d(dest, to_npc)
    if (CUR.pending_click is not None and CUR.pending_click.get("chasing")
            and dist_dest > npc_click_range(longdistance)):
        # We own the walk for this click (a chase packet went out), and the client
        # is now walking somewhere that is NOT the NPC: that is the player
        # clicking the ground, and it CANCELS the interaction, like the original's
        # DispatchMove -> AS_NONE. Measured on the live server: during a
        # click-walk the client sends no moves of its own, so there is nothing to
        # mistake this for. Keeping the click here is what made a ground click a
        # few hundred units out open the dialog a moment later ("the dialog will
        # still open even after i click the ground huh?", 2026-10-22).
        # A destination INSIDE the reach means the walk ENDS at the NPC - that is
        # the client walking it there (or arriving), so the click stays and the
        # dialog opens on the arrival.
        log("  the player clicked the ground (dest %.0f units from 0x%08x) - "
            "cancelling the click, like the original's AS_NONE"
            % (dist_dest, handle))
        CUR.pending_click = None
        CUR.click_lock = None
        return
    npc = zone_npcs(ZONE_ID)[idx]
    to_npc = (npc["x"], npc["y"])
    dist_dest = _dist2d(dest, to_npc)
    if dist_dest <= npc_click_range(longdistance) * 2:
        return                     # ends at the NPC: the client is walking it there
    # How far the player was when this walk STARTED. `pc_start_walk` has already
    # replaced CUR.move by now, and its anchor is that position.
    here = CUR.move[0] if CUR.move else CUR.pos
    dist_now = _dist2d(here, to_npc) if here is not None else None
    if dist_now is not None and dist_dest < dist_now - 1:
        # Heading toward the NPC: one step of the client's own path. Keep the
        # click and refresh its stall clock, so a slow walk is not dropped.
        if CUR.pending_click is not None:
            best = CUR.pending_click.get("best_dist")
            if best is None or dist_dest < best - 1:
                CUR.pending_click["best_dist"] = dist_dest
            CUR.pending_click["give_up"] = _ms() + CLICK_STALL_MS
        log("  the client is closing in on 0x%08x (%.0f units) - keeping "
            "the click and the chase" % (handle, dist_dest))
        return
    if dist_now is not None and dist_dest > dist_now + GROUND_CANCEL_MARGIN:
        log("  walked away from 0x%08x (destination %.0f units from it, was %.0f)"
            " - cancelling the click, like the original's AS_NONE"
            % (handle, dist_dest, dist_now))
        CUR.pending_click = None
        CUR.click_lock = None
        return
    # Neither clearly closer nor clearly away (a step sideways, or our own chase
    # echoed back). Stop chasing, but keep the click: the dialog still opens if
    # the player does arrive. While a leg chain is under way (`goal` set) keep
    # chasing instead - the chain is the only thing moving the actor, and the
    # client's reports during it are its own shuffling, not a decision to leave.
    if (CUR.pending_click is not None and CUR.pending_click["chase_ok"]
            and CUR.pending_click.get("goal") is None):
        CUR.pending_click["chase_ok"] = False
        log("  the client took over the walk (dest %.0f units from 0x%08x)"
            " - keeping the click, not chasing again" % (dist_dest, handle))


def npc_walk_point(handle, longdistance, ring=None):
    """Where AS_TRIGGER_CHASE walks the PC to (CFieldChar::GetDomainOuterPos).

    The original takes the line from the NPC to the PC and puts the destination
    `GetDomainRadius() + dist` out along it, with `dist = m_npcClickDistance - 10`
    (PlayerChar_FSM.cpp:212), then snaps that point to valid ground. The radius is
    the NPC's own shape size (geShapeTable S/M/L/XL); a player is always "S".

    `ring` overrides that sum, which is how chase_to_npc backs the point off when
    the ideal one turns out to be unreachable (see there).
    """
    pos = pc_position()
    idx = npc_handle_index(handle)
    if pos is None or idx is None:
        return None
    npc = zone_npcs(ZONE_ID)[idx]
    dx = npc["x"] - pos[0]
    dy = npc["y"] - pos[1]
    dist = (dx * dx + dy * dy) ** 0.5
    if dist < 1:
        return None
    radius = (ring if ring else
              NPCD_RADIUS.get(npc.get("size", "S"), NPCD_RADIUS["S"])
              + (NPC_CLICK_DISTANCE_LONG if longdistance else
                 NPC_CLICK_DISTANCE) - 10)
    return (npc["x"] - dx / dist * radius, npc["y"] - dy / dist * radius)


def chase_ring_distances(longdistance, npc_size="S"):
    """Candidate distances from the NPC centre to aim the walk at.

    The ideal is the original's `radius + m_npcClickDistance - 10` (140 for an
    "S" NPC). That is NOT always walkable ground for a player-sized body, so
    back off in small steps up to just inside the click gate - any distance
    below `npc_click_range` still satisfies `IsNearObject` on arrival.

    Measured case (2026-10-21): NPC #49's 140-unit ring lands in a pocket no
    50-unit body can enter from the plaza, so the client's path move found no
    route and the character never moved. 180 units out is reachable from the
    same spot and still inside the 200 gate.
    """
    base = (NPCD_RADIUS.get(npc_size, NPCD_RADIUS["S"])
            + (NPC_CLICK_DISTANCE_LONG if longdistance else
               NPC_CLICK_DISTANCE) - 10)
    gate = npc_click_range(longdistance)
    out = []
    d = base
    while d + CHASE_RING_STEP < gate:         # the ideal pose, then outward
        out.append(d)
        d += CHASE_RING_STEP
    d = base - CHASE_RING_STEP
    while d >= 40:                            # ... and INWARD, closer to the NPC
        out.append(d)
        d -= CHASE_RING_STEP
    return out


# Angular offsets (degrees) tried around the clicked NPC when the line straight
# at the player is not walkable, nearest-to-ideal first. NPC #80 (Granma) from
# the plaza has ONLY the far side blocked: its ideal bearing was blocked for
# every distance, while 90..330 degrees all had a route.
CHASE_RING_ANGLES = (0, 30, -30, 60, -60, 90, -90, 120, -120, 150, -150, 180)


def chase_walk_points(handle, longdistance):
    """Every walk point worth trying around a clicked NPC, best first.

    The original aims down ONE line: from the NPC towards the PC, at
    `radius + m_npcClickDistance - 10` (GetDomainOuterPos). That single ray is
    not always walkable for a player-sized body, and there are two ways it
    fails - both seen in game:

      * the point is valid ground but no route reaches it, so backing the point
        OFF along the same ray is enough (NPC #49, the pocket at 140 units);
      * the whole SIDE of the NPC is blocked, so every distance on that ray
        fails and only a different bearing works (NPC #80 `Granma`: her ideal
        bearing had no route at any distance, while almost every other bearing
        did).

    So the ring is swept in angle as well, nearest-to-ideal first, with three
    distances per bearing. Every candidate stays inside the click gate, so
    arriving at any of them satisfies `IsNearObject` exactly like the ideal one.

    Returns [(ring, offset_deg, (x, y))], ideal point first.
    """
    pos = pc_position()
    idx = npc_handle_index(handle)
    if pos is None or idx is None:
        return []
    npc = zone_npcs(ZONE_ID)[idx]
    dx, dy = npc["x"] - pos[0], npc["y"] - pos[1]
    if (dx * dx + dy * dy) < 1:
        return []
    ideal = math.atan2(dy, dx)                # NPC -> player bearing
    dists = chase_ring_distances(longdistance, npc.get("size", "S"))
    out = []
    for ring in dists:                        # the client FSM's own aim
        out.append((ring, 0, (npc["x"] - math.cos(ideal) * ring,
                              npc["y"] - math.sin(ideal) * ring)))
    near, far = dists[0], dists[-1]
    mid = dists[len(dists) // 3]
    for off in CHASE_RING_ANGLES[1:]:
        a = ideal + math.radians(off)
        for ring in dict.fromkeys((near, mid, far)):
            out.append((ring, off, (npc["x"] - math.cos(a) * ring,
                                    npc["y"] - math.sin(a) * ring)))
    # Last resort only: standing ON the NPC looks like the character overlaps it,
    # but it beats refusing to walk when nothing on the ring is reachable.
    out.append((0, 0, (npc["x"], npc["y"])))
    return out


def _hop_along(route, limit):
    """The point `limit` units along a route polyline, and how far that is.

    A server move is walked by the client as ONE straight leg: it does not
    follow the route we computed. So a long route has to be handed over in
    pieces - each piece is a straight line between two route corners, which the
    path engine guarantees is walkable. Returns (point, distance) or (None, 0).
    """
    if not route:
        return None, 0
    if len(route) == 1:
        return route[0], 0
    prev = route[0]
    total = 0.0
    for pt in route[1:]:
        seg = _dist2d(prev, pt)
        if seg <= 0:
            prev = pt
            continue
        if total + seg >= limit:
            t = (limit - total) / seg
            # The cell of the leg's START vertex: the point sits within the
            # first `limit` units of this segment, i.e. on the same ground as
            # its beginning. The caller re-snaps it anyway.
            return ((int(round(prev[0] + (pt[0] - prev[0]) * t)),
                     int(round(prev[1] + (pt[1] - prev[1]) * t)),
                     prev[2]), limit)
        total += seg
        prev = pt
    return route[-1], total


def chase_to_npc(conn, handle, longdistance):
    """Send the ZC_MOVE_PC that walks the player to a clicked NPC.

    The original does this from AS_TRIGGER_CHASE (`SetMoveState(MS_MOVE)` makes
    the server broadcast the move). Here the server is the only thing that can:
    the client sends nothing for a clicked trigger. Returns True if a walk was
    started.

    Two facts shape this, both measured:

      * the destination must be REACHABLE for a player-sized body, so the ring
        around the NPC (`chase_walk_points`) is path-checked and the first
        candidate with a route wins - the ideal point first, so a normal NPC
        walks exactly as before;
      * the client walks a server move as ONE STRAIGHT LEG and does not follow
        our route. Handing it a 3000-unit route with two corners in it made the
        character shuffle against the first hedge and stop ("my character cant
        walk around stuff"), with the route sitting there in our own log.

    So the route is split into legs of at most `CHASE_HOP` units and one leg is
    sent per chase (`_hop_along`); the final point is remembered on the pending
    click as its `goal`, so the next leg keeps aiming at the same place instead
    of re-picking a candidate at a slightly different angle every time.
    """
    if CUR.handle is None:
        return False
    idx = npc_handle_index(handle)
    if idx is None:
        return False
    # Start from our own position model: the client sends nothing while it walks
    # a move we sent, so its last report is frozen at the moment of the click and
    # using it made every retry compute the same path from the same old point.
    here = pc_position() or CUR.last_client_pos
    if here is None:
        return False
    npc = zone_npcs(ZONE_ID)[idx]
    gate = npc_click_range(longdistance)
    try:
        import npc_data
        entry = npc_data.load_map_table().get(ZONE_ID)
        mesh = entry[1] if entry else None
    except Exception as exc:
        mesh = None
        log("  !! map table failed (%s) - chasing without a path check" % exc)
    start = (int(here[0]), int(here[1]), here[2])

    dest, length, route, kept = None, None, None, None
    # The ORIGINAL's own derivation, followed all the way down:
    #   PlayerChar_FSM.cpp:212  target->GetDomainOuterPos(m_pos, moveTo,
    #                           m_npcClickDistance - 10)
    #   FieldObject.cpp:128     point = target + (targetRadius + dist) towards
    #                           the player  (50 + 90 = 140 for an "S" NPC)
    #   Ground.cpp GetValidPos  firstCollision along that line, take the
    #                           intersection, nudge out of it
    # So the stop point is the ideal distance CLAMPED by whatever is beside the
    # NPC - an obstacle shortens the gap instead of making the pose unusable, and
    # if the clamp itself fails the original sends NO move at all.
    ring = chase_ring_distances(longdistance, npc.get("size", "S"))[0]
    pt = npc_walk_point(handle, longdistance, ring)
    if pt is not None and mesh:
        try:
            import npc_data
            got, hit = npc_data.clamp_point(
                (npc["x"], npc["y"], npc["cell"]),
                (int(round(pt[0])), int(round(pt[1]))), mesh)
            kept = (ring, 0, pt)
            if got is None:
                dest = None
            elif hit:
                # The clamp SHORTENED the point, i.e. an obstacle sits right
                # beside the NPC (garden bed, cart, doorway). That is exactly the
                # set of NPCs whose clamped pose the client refuses to walk to -
                # it accepts only the NPC's own position there, which is the
                # overlap. Measured: #80 140 -> 104 (refused), while every NPC the
                # player reported as fine was either clamp-free (140) or much
                # tighter and accepted.
                dest = (npc["x"], npc["y"], npc["cell"])
                log("  clamp hit at 0x%08x (ideal %d) - using its own position "
                    "instead (overlap)" % (handle, ring))
            else:
                dest = got
        except Exception as exc:
            log("  !! clamp failed (%s)" % exc)
            dest = None
    if dest is not None and mesh:
        try:
            import npc_data
            r = npc_data.find_paths(start, [dest], mesh)[0]
            if r:
                length, route = r[1], r[0]
        except Exception as exc:
            log("  !! path check failed (%s)" % exc)
    if dest is None and mesh:
        log("  no placeable walk point for 0x%08x (GetValidPos failed) - sending "
            "no move, like the original" % handle)
        return False
    if dest is None:
        cands = chase_walk_points(handle, longdistance)
        if not cands:
            return False
        dests, kept = [], []
        if mesh:
            try:
                # Snap every candidate to real ground with the client's own path
                # engine (the same step GetDomainOuterPos does with GetValidPos).
                # The NPC's own height is the vertical hint, which is what makes
                # the engine pick the local floor rather than another level.
                goals = npc_data.resolve_cells(
                    [(int(round(pt[0])), int(round(pt[1])),
                      int(npc.get("z3d", 0))) for _, _, pt in cands],
                    mesh, h_range=1000, v_range=300)
                for cand, got in zip(cands, goals):
                    d = got or (int(round(cand[2][0])),
                                int(round(cand[2][1])), npc["cell"])
                    # A snap that jumped is a point on some other piece of
                    # ground; it must still be inside the click gate or the
                    # arrival test would fail after the walk.
                    if _dist2d(d, (npc["x"], npc["y"])) > gate:
                        continue
                    dests.append(d)
                    kept.append(cand)
                routes = npc_data.find_paths(start, dests, mesh)
            except Exception as exc:
                log("  !! path check failed (%s) - chasing without it" % exc)
                dests, kept, routes = [], [], []
        if not dests:
            # No path engine, or nothing snapped inside the gate: fall back to
            # the ideal point on the NPC's own ground, where the NPC stands.
            pt = npc_walk_point(handle, longdistance)
            if pt is None:
                return False
            dest = (int(round(pt[0])), int(round(pt[1])), npc["cell"])
            length, route, kept = None, None, (None, 0, pt)
        else:
            ideal_ring = chase_ring_distances(longdistance, npc.get("size", "S"))[0]
            pick = None
            for i, d in enumerate(dests):
                # ONLY the ideal pose counts as "a gap is possible". Anything
                # else (a shorter or angled point) is a pose the client refuses
                # in silence: we cannot tell it apart from a working walk, so it
                # buys a longer pause and a risk of a dialog in mid-air.
                if kept[i][0] != ideal_ring:
                    continue
                if routes[i] is not None and routes[i][0]:
                    pick = i
                    break
            if pick is None:
                # No gap possible here: walk to the NPC's OWN position. It
                # overlaps the NPC, but it is the shape the live server sends and
                # the one the client always walks to.
                log("  no reachable gap pose for 0x%08x - walking to it directly"
                    % handle)
                dest = (npc["x"], npc["y"], npc["cell"])
                route, length = None, None
                kept = (0, 0, (npc["x"], npc["y"]))
                if CUR.pending_click is not None:
                    CUR.pending_click["overlap"] = True
                    CUR.pending_click["goal"] = dest
            else:
                dest = dests[pick]
                route = routes[pick][0] if routes[pick] else None
                length = routes[pick][1] if routes[pick] else None
                kept = kept[pick]
        # Remember the goal AND the leg length this click started with, so the
        # log reads as a chain and later legs are not throttled to one a second.
        if CUR.pending_click is not None:
            CUR.pending_click["goal"] = dest

    if CHASE_HOP > 0:
        step, step_len = _hop_along(route, CHASE_HOP)
    else:
        step, step_len = None, None       # whole route in one move (GE_CHASE_HOP=0)
    if step is None or step_len <= 0:
        step, step_len = dest, length
    if step is not None and step != dest and mesh:
        # A leg's endpoint is interpolated between two route corners, so it does
        # not carry a usable cell: snap it (the same query the candidate points
        # go through) or the NEXT leg's path query starts from an invalid point
        # and every route comes back empty.
        try:
            import npc_data
            got = npc_data.resolve_cells(
                [(step[0], step[1], int(npc.get("z3d", 0)))], mesh,
                h_range=150, v_range=300)[0]
            if got:
                step = got
        except Exception as exc:
            log("  !! leg not snapped (%s)" % exc)
    if _dist2d(here, (step[0], step[1])) < 1:
        return False
    conn.sendall(build_move_pc(CUR.handle, step, reason=MR_DIALOG))
    pc_start_walk(step, length=step_len, route=route)
    CUR.chase_issued_at = _ms()      # see the start-of-leg choice above
    if CUR.pending_click is not None and CUR.move:
        # Do not chase again until this walk should be over: re-targeting an
        # actor that is already walking makes it stutter, and the cooldown alone
        # is shorter than a long walk.
        CUR.pending_click["next_move"] = max(CUR.pending_click.get("next_move", 0),
                                             CUR.move[3])
    log("  chasing 0x%08x -> (%d, %d, cell=%d) [reason=MR_DIALOG%s gap=%d%s%s]"
        % (handle, step[0], step[1], step[2],
           " ideal=%d" % kept[0] if kept and kept[0] else "",
           int(round(_dist2d(step, (npc["x"], npc["y"])))),
           " leg=%d/%d" % (step_len, length or 0) if length else "",
           " path=%d" % length if length else ""))
    return True



def try_pending_click(conn):
    """Answer a remembered NPC click once the player has arrived.

    Called when the click arrives and on the POLL_SECONDS recv tick, so the
    dialog lands when the walk ends - no packet arrives at that exact moment -
    instead of waiting for the client to happen to re-send the click. The gates
    are the FSM's own, in the same order (PlayerChar_FSM.cpp:188
    AS_TRIGGER_CHASE):
      * while a walk is running nothing happens
        (`if (MS_MOVE == GetMoveState()) return 0;`),
      * once it is over the player must be within m_npcClickDistance
        (`IsNearObject(target, m_npcClickDistance)`) - the range below - and
      * if they are not, the state walks them closer
        (`target->GetDomainOuterPos(...)` -> `SetMoveState(MS_MOVE)`), which is
        the chase below.
    When the player is finally in range it does what AS_TRIGGER does: runs the
    trigger (here: the dialog).
    """
    if CUR.click_lock is not None and not pc_walking():
        # The walk a lock belongs to is over, whatever happened to the click -
        # a lock must never outlive its walk (it would hijack a later click).
        CUR.click_lock = None
    if CUR.pending_click is None:
        return
    handle = CUR.pending_click["handle"]
    longdistance = CUR.pending_click["long"]
    redirected = False
    # NOTE: there used to be a "the client never reported a walk -> give up"
    # clock here. It was wrong: the client sends NO position reports while it
    # walks a move the SERVER sent, so "no reports" is the NORMAL case, and the
    # clock was killing good clicks a few seconds into a perfectly good walk -
    # and then sending ZC_STOP_PC at an estimated anchor, which teleports the
    # actor. The walk's own ETA is the clock; nothing else is needed.
    # NOTE: there used to be a "the client did not answer the pose" check here
    # that switched to the NPC's own position after 2 s. It was written for the
    # ring-pose experiment and is now pure damage: our destination IS the NPC's
    # own position, so it fired 2 s into every good walk, cleared the walk, and
    # the arrival test then read THE DESTINATION we had stored and opened the
    # dialog immediately ("even Leonardo now affected", 2026-10-22).
    if CUR.pending_click["redirect"] and pc_walking():
        # A redirect: either a click during a GROUND walk (the original's state
        # change stops that walk) or the SECOND click on an NPC while walking to
        # another one. Either way the running walk is cancelled - freeze it where
        # the player is now, so the chase below starts from the right place and
        # the walk counts as over; the client turns and walks to the new NPC off
        # the same packet, with no stop packet and no snap.
        here = pc_position()
        if here is not None:
            CUR.pos, CUR.move = here, None
        log("  redirecting the walk to 0x%08x from (%d, %d)"
            % (handle, int(CUR.pos[0]), int(CUR.pos[1])))
        CUR.pending_click["redirect"] = False
        redirected = True
    if pc_walking():
        # Still mid-stride, so keep the click alive; only the stall clock runs
        # while the player is standing still and out of range.
        CUR.pending_click["give_up"] = _ms() + CLICK_STALL_MS
        return
    CUR.click_lock = None          # nothing is walking, so no walk is locked
    dist = npc_click_distance(handle)
    if dist is None or dist <= npc_click_range(longdistance):
        CUR.pending_click = None
        CUR.click_lock = None
        open_npc_dialog(conn, handle)
        return
    # The walk is over but the player is still too far away. Say so, with the
    # numbers: a click that never opens is otherwise invisible, and this is
    # the line that tells you WHY (still walking? out of range? stale ETA?).
    # Once per click, not once per tick - a click that cannot be answered
    # would otherwise print a line five times a second until it gives up.
    if not CUR.pending_click.get("reported_far"):
        CUR.pending_click["reported_far"] = True
        log("  arrival check on 0x%08x: %.0f units away, need <=%d, walk over"
            % (handle, dist, npc_click_range(longdistance)))
    now = _ms()
    # Do NOT re-chase while the walk is getting somewhere. Every chase packet
    # re-targets the actor, so issuing one per second made the character stutter
    # and feel like it could not be cancelled - measured 2026-10-22: FIFTEEN
    # chases in a row to NPC #80 while the character was already walking to her
    # (the client reports its own steps, each report ends our interpolated walk,
    # the tick then saw "standing still, out of range" and chased again). So
    # re-chase only when the walk has STALLED (no closer than the distance at the
    # previous chase) and never sooner than CHASE_COOLDOWN_MS after it.
    last_at = CUR.pending_click.get("chased_at", 0)
    last_dist = CUR.pending_click.get("chased_dist")
    if last_dist is not None and dist is not None and dist < last_dist - CHASE_PROGRESS:
        CUR.pending_click["give_up"] = now + CLICK_STALL_MS
        log("  walk to 0x%08x is closing in (%.0f units, was %.0f) - not chasing"
            " again" % (handle, dist, last_dist))
        return
    if (now - last_at) < CHASE_COOLDOWN_MS:
        CUR.pending_click["give_up"] = now + CLICK_STALL_MS
        return
    if (CUR.pending_click["chase_ok"] and CUR.pending_click["tries"] < CHASE_TRIES
            and now >= CUR.pending_click["next_move"]):
        CUR.pending_click["tries"] += 1
        CUR.pending_click["next_move"] = now + CHASE_RETRY_MS
        if CUR.pending_click["tries"] > 1 and not CUR.pending_click.get("overlap"):
            # The first chase did not bring the player any closer, so the pose it
            # aimed at is one the client will not walk to. Switch to the target's
            # own position (overlapping it) - the live's own fallback shape.
            CUR.pending_click["overlap"] = True
            CUR.pending_click["goal"] = None
            log("  the walk pose did not move the character - falling back to "
                "0x%08x's own position" % handle)
        if chase_to_npc(conn, handle, longdistance):
            CUR.pending_click["chased_at"] = now
            CUR.pending_click["chased_dist"] = dist
            # From here the walk belongs to this click, so a click on another
            # NPC can no longer take it over (see remember_click). The walk is
            # OUR invention, so give the client a moment to prove it is really
            # moving by reporting a step of its own.
            CUR.pending_click["chasing"] = True
            CUR.pending_click["give_up"] = _ms() + CLICK_STALL_MS
            # This walk is OURS until the client reports one for it: only then is
            # an arrival ours to trust (see the deadline at the top).
            CUR.pending_click["client_walk"] = False
            # Give the client until the walk it was told to do should be over
            # (plus a margin) to prove it is really moving; capped, so a very long
            # chase cannot leave the actor animating in place for minutes.
            eta = CUR.move[3] if CUR.move else _ms()
            CUR.pending_click["walk_deadline"] = min(eta + 1000, _ms() + CHASE_WALK_MS)
            # Where the player really is - the anchor the invented walk started
            # from, so a give-up can stop the animation THERE and not at the
            # destination we made up.
            CUR.pending_click["anchor"] = CUR.move[0] if CUR.move else None
            CUR.click_lock = {"handle": handle, "long": bool(longdistance),
                          "armed": None}
            return
        # No walk went out: either there is nowhere walkable to stand near this
        # NPC (chase_to_npc logs why) or the player is already standing on the
        # point. Either way, asking again on the next tick cannot help, so stop
        # chasing and let the arrival check / the stall clock finish the click.
        CUR.pending_click["chase_ok"] = False
    if now >= CUR.pending_click["give_up"]:
        log("  click on 0x%08x gave up: %.0f units away (>%d)"
            % (handle, dist, npc_click_range(longdistance)))
        CUR.pending_click = None
        CUR.click_lock = None


def open_npc_dialog(conn, handle):
    """Answer a click: the NPC's line, then (for a shop) its trade window.

    A shop NPC's script shows a dialog first and offers the shop from it (in the
    original that dialog has Buy/Sell options), so the shop is NOT opened on the
    click: the line goes out now, and the window follows when the player presses
    OK (see CMD_CZ_ACK_DIALOG). That is also what keeps the character in place -
    the dialog is modal.
    """
    started = script_click(handle)
    # Did the NPC ANSWER by speaking? `ChatNPC` sends ZC_CHAT and does not
    # suspend, so a script that only speaks has no dialog line by design.
    spoke = bool(getattr(_host(), "chat_events", None))
    # the script may already have committed a transaction on this very click
    flush_item_events(conn)
    flush_property_events(conn)
    flush_quest_events(conn)
    flush_setpos_events(conn)
    flush_chat_events(conn)
    if started and not started["done"]:
        question = started["question"]
        prompt = ge_locale.resolve_for_client(question["prompt"])
        options = [ge_locale.resolve_for_client(o) for o in question["options"]]
        CUR.pending_script = {"co": started["co"], "handle": handle,
                              "seen": len(_host().calls), "prompt": prompt}
        conn.sendall(build_dialog_select(handle, prompt, options,
                                         question.get("fade", DLG_WITH_FADE_IN)))
        conn.sendall(build_npc_click_voice(handle))
        log("  SENT ZC_DIALOG_SELECT handle=0x%08x prompt=%r options=%r"
            % (handle, prompt, options))
        return True
    text = (started or {}).get("dialog")
    if text:
        # the FIRST reply needs the same locale resolution the resume path does;
        # without it the player reads a raw token like `<$>7454</>`
        text = ge_locale.resolve_for_client(text)
    if not text and spoke:
        # The NPC already answered by SPEAKING (ChatNPC -> ZC_CHAT). A script
        # that only speaks has no dialog line BY DESIGN, and the fallback below
        # would stack a modal "nothing to say" box on top of the balloon.
        log("  click on 0x%08x spoke only - no dialog box" % handle)
        return False
    text = text or npc_dialog_text(handle)
    if text is None:
        log("  !! click on unknown handle 0x%08x" % handle)
        return False
    # A click may ASK FOR A WINDOW instead of talking (`ShowLobby`). The click
    # decided which one, so it is opened here, after the script ran, and the
    # dialog box is skipped - exactly like the shop case below.
    lobby = _lobby_request()
    if lobby is not None:
        lobby_type, _layer, create_ok = lobby
        CUR.lobby_type = lobby_type
        conn.sendall(build_lobby_room_list(CUR.account_id, _room_records(), bool(create_ok)))
        log("SENT ZC_LOBBY_ROOM_LIST lobby=%r rooms=0 create=%d (from click)"
            % (lobby_type, create_ok))
        return True
    shop = _trade_shop() or npc_shop_items(handle)
    if shop:
        CUR.pending_shop = {"handle": handle, "kind": shop["kind"],
                        "group": shop["group"], "items": shop.get("items") or [],
                        "entries": shop.get("entries") or []}
        conn.sendall(build_dialog_ok(handle, text))
        conn.sendall(build_npc_click_voice(handle))
        log("  SENT ZC_DIALOG_OK handle=0x%08x (%s shop %s: %d rows after OK) "
            "text=%r"
            % (handle, shop["kind"], shop["group"],
               len(CUR.pending_shop["entries"] or CUR.pending_shop["items"]), text))
        return True
    conn.sendall(build_dialog_ok(handle, text))
    conn.sendall(build_npc_click_voice(handle))
    log("  SENT ZC_DIALOG_OK handle=0x%08x text=%r" % (handle, text))
    return True


def load_players(n=None):
    """Build START_INFO for the selected team.

    Priority: the barrack's saved team for THIS player (zone_team_<id>.json).
    If `n` is given on the command line it overrides (first n characters) -
    useful for testing.

    The characters are the ones of the player who connected, not of a fixed
    commander: CZ_START_GAME carries the account id, and the account names the
    commander. The original did the same through its SLService.
    """
    cid = CUR.commander_id or DEFAULT_COMMANDER
    try:
        import ge_db
        cm = ge_db.get_commander(cid)
        chars = ge_db.get_characters(cid)
    except Exception as exc:
        log("  !! could not load players: %s" % exc)
        return []
    global STANCE_XP_TYPES, STANCE_LEVELS
    stance_rows = {}
    skill_rows = {}
    try:
        stance_rows = ge_db.get_character_stances(cid)
        skill_rows = ge_db.get_character_skills(cid)
        STANCE_XP_TYPES = ge_db.get_stance_xp_types()
        STANCE_LEVELS = ge_db.get_stance_levels()
    except Exception as exc:
        log("  !! stance tables unavailable (%s); sending level 1 / exp 0" % exc)
    cname = cm["name"] if cm else ""
    CUR.commander_name = cname
    # The map this zone serves. One zone process serves ONE map (the original
    # ran one zone process per map, per channel), so the map is set by the
    # first player who enters an empty zone and then kept.
    global ZONE_ID, ZONE_PLAYERS
    with ZONE_LOCK:
        if ZONE_PLAYERS == 0:
            ZONE_ID = cm["zone_id"] if cm else 0
            try:
                announce_zone(ZONE_ID)
            except Exception as exc:
                log("  !! could not announce zone %s: %s" % (ZONE_ID, exc))
        # Every player in this process is in the same map, so this is set for
        # EVERY player, not only the first one. It was inside the `if` above,
        # so the second player to enter got map id 0 - and the position we
        # save for them was tagged with the wrong map.
        CUR.map_id = ZONE_ID
    zone_id = ZONE_ID
    with ZONE_LOCK:
        if not CUR.entered:
            ZONE_PLAYERS += 1
            CUR.entered = True
    log("  players in this zone: %d (map %s)" % (ZONE_PLAYERS, zone_id))

    if n is not None:
        chosen = chars[:n]
    else:
        picked = _read_team_file()
        by_slot = {c["slot"]: c for c in chars}
        chosen = [by_slot[s] for s in (picked or []) if s in by_slot]
        missing = [s for s in (picked or []) if s not in by_slot]
        if missing:
            log("  !! selected slots not found in DB: %s" % missing)
        if not chosen:
            # No handoff for THIS player (no team file, or it was for someone
            # else). Fall back to this player's first character: a player must
            # never end up in an empty world.
            chosen = chars[:1]
            log("  !! no team for this player - using %r"
                % (chosen[0]["name"] if chosen else None))

    # A CONTINUEGAME handoff asks for the character's SAVED position, which
    # beats the map's default spawn - that is the whole point of "continue".
    cont_pos = None
    saved = _read_continue_pos()
    if saved is not None:
        cont_pos = _continue_spawn(saved)

    fixed = resolve_spawn(zone_id)
    pcs = []
    spawns = []
    CUR.blocks.clear()
    CUR.team = list(chosen)
    for c in chosen:
        if cont_pos is not None:
            # A CONTINUEGAME handoff: the player is put back where they were.
            # The original server does this in CCommander::GetContGameZoneInfo
            # (source2\geServer\BarrackServer\Commander.cpp:398), which reads
            # the character's SAVED point - GetCharacterSlot()->GetContinuePos()
            # -> m_continueZonePos, set from character->point.x/y/z
            # (CharacterSlot.cpp:126). So the position is SERVER-SIDE state
            # keyed by the character, not something the packet carries; the
            # barrack looks it up and hands it over.
            pos = cont_pos
        elif fixed is not None:
            pos = fixed
        else:
            # Fall back to the DB point. NOTE: this uses the z-height as the
            # cell, which is usually WRONG (see MAP_SPAWN) - only a last resort.
            pos = (c.get("xpos", 0), c.get("ypos", 0), c.get("zpos", 0))
        spawns.append(pos)
        c["stance_rows"] = stance_rows.get(c["character_id"], {})
        c["skill_rows"] = skill_rows.get(c["character_id"], {})
        # Build the blocks ONCE: they go into START_INFO and are repeated by the
        # unlock packets after the world-entry handshake.
        c["stance_bytes"] = stance_data.build_stance_records(
            c["job"], c["stance_rows"], STANCE_XP_TYPES, STANCE_LEVELS)
        c["skill_bytes"] = skill_data.build_skill_records(c["job"], c["skill_rows"])
        CUR.blocks[c["character_id"]] = (c["stance_bytes"], c["skill_bytes"])
        pcs.append(build_start_info(c, cname, handle=c["character_id"], pos=pos))
        log("  spawn slot=%d id=%d name=%r job=%d gender=%d head=%d pos=%s"
            % (c["slot"], c["character_id"], c["name"], c["job"], c["gender"],
               c["headtype"], pos))
        log("    stances %s (levels from the DB where present)"
            % (stance_data.job_stances(c["job"]) or "NONE"))
        log("    skills %d from the stances, %d with a DB exp row"
            % (len(stance_data.job_skills(c["job"])), len(c["skill_rows"])))
    if pcs:
        CUR.handle = chosen[0]["character_id"]
        # Where the leader is, so a click can tell "already near the NPC" from
        # "out of range" (see NPC_CLICK_RANGE). A fresh entry has no walk going.
        CUR.pos = spawns[0]
        CUR.last_client_pos = spawns[0]
        CUR.move = None
        # The stance the client will put the leader in, computed by the same rule
        # (job + equipped weapon classTypes). Its RSPD is the move speed we send,
        # so the walk matches the animation. No match = the client's own fallback.
        leader = chosen[0]
        if not FORCE_STANCE:
            if not SEND_STANCES:
                CUR.stance = START_STANCE
            else:
                CUR.stance = (
                    stance_data.active_stance(leader["job"],
                                              leader.get("equip") or {})
                    or stance_data.GM_STANCE)
        log("  leader stance %d %s -> move speed %.1f u/s"
            % (CUR.stance, stance_data.summary(CUR.stance),
               move_speed()))
    return pcs


def parse_cz_start_game(pt):
    # PKS_SECURITY_DEFAULT = command(2) + seq(4) + checksum(4) = 10 bytes
    if len(pt) < 18:
        return None
    session_id, account_id = struct.unpack_from("<II", pt, 10)
    return {"command": struct.unpack_from("<H", pt, 0)[0],
            "session_id": session_id, "account_id": account_id,
            "id": pt[18:18 + 17].split(b"\0", 1)[0].decode("latin-1", "replace")}


def handle(conn, addr):
    # Short recv timeout: a timeout is the "update tick" that lets a remembered
    # NPC click be answered when the walk ends (see try_pending_click). The
    # session still ends after HOLD_SECONDS of silence.
    conn.settimeout(POLL_SECONDS)
    CUR.conn = conn
    log("CONNECT from %s:%d" % addr)
    replied = False
    last_rx = _ms()
    try:
        while True:
            try:
                data = conn.recv(65536)
            except socket.timeout:
                # The tick must not be able to kill the connection. An
                # exception here used to end the thread with NO log line, and
                # the symptom was "the dialog never opened" - the player was
                # still connected, but nothing was ever checked again.
                try:
                    try_pending_click(conn)
                    # End this player's walk for the others when it is over.
                    stop_others_if_walk_over()
                except Exception as exc:
                    log("  !! the update tick failed: %r" % exc)
                    import traceback
                    log("  " + traceback.format_exc().replace("\n", "\n  "))
                if _ms() - last_rx > HOLD_SECONDS * 1000:
                    log("no more data (timeout)")
                    break
                continue
            last_rx = _ms()
            if not data:
                log("client closed")
                break
            log("RECV %d wire bytes: %s" % (len(data), hexs(data[:64])))
            try:
                pt = ge_crypt.decrypt_packet(data)
            except Exception as exc:
                log("  decrypt failed: %s" % exc)
                continue
            log("  plain: %s" % hexs(pt[:64]))
            cmd = struct.unpack_from("<H", pt, 0)[0] if len(pt) >= 2 else -1
            log("  command=%d" % cmd)
            if cmd == CMD_CZ_BUYITEM:
                # Full dump: the log's usual hex is capped at 64 bytes and the
                # multi-record layout is what we are pinning down.
                log("  BUYRAW len=%d lenfield=%d %s"
                    % (len(pt), struct.unpack_from("<H", pt, 10)[0] if len(pt) >= 12 else -1,
                       hexs(pt)))
            if cmd == CMD_CZ_START_GAME:
                info = parse_cz_start_game(pt)
                log("  CZ_START_GAME %s" % info)
                # Who is this? CZ_START_GAME carries the account id, and the
                # account names the commander - so the player gets THEIR
                # characters and THEIR bag, not a fixed one.
                cid = 0
                if info:
                    try:
                        import ge_db
                        cid = ge_db.get_commander_by_account(info.get("account_id")) or 0
                    except Exception as exc:
                        log("  !! could not resolve the account: %s" % exc)
                if not cid:
                    cid = DEFAULT_COMMANDER
                    log("  !! unknown account %s - using commander %d"
                        % (info.get("account_id") if info else "?", cid))
                CUR.account_id = info.get("account_id", 0) if info else 0
                CUR.commander_id = cid
                # The name a script prints as `commander.CommName` - the client's
                # own family name, carried by CZ_START_GAME. Without it a dialog
                # reads "You are {} Family?".
                CUR.family_name = (info.get("id") if info else "") or ""
                log("  player: account=%s commander=%d"
                    % (CUR.account_id, CUR.commander_id))
                pcs = load_players(FORCE_COUNT)
                reply = build_startgameok(pcs)
                conn.sendall(reply)
                replied = True
                log("SENT ZC_START_GAMEOK count=%d len=%d (header=%d, record=%d): %s"
                    % (len(pcs), len(reply), HDR_SIZE, SI_SIZE, hexs(reply[:64])))
                # Post-entry sequence: same order as the real server
                # (SS_READY: StartGameOK, LockInfo, Point, QuestInfo,
                # InventoryList, ...). Batch 1 proven; batch 2 sends EMPTY
                # lists (same as now: no items/quests) to prove the shapes.
                try:
                    import ge_db
                    cm = ge_db.get_commander(CUR.commander_id)
                    point = cm["point"] if cm else 0
                    try:
                        # Wire invIndex runs 1..N in load order; options and
                        # reinforces key off ITEM_ID so resolve them here.
                        full = ge_db.get_inventory_full(CUR.commander_id)
                        if INV_LIMIT:
                            full = full[:INV_LIMIT]
                        inv_items = [{"type": r["type"], "count": r["count"]}
                                     for r in full]
                        CUR.last_inv = full
                        inv_by_id = {}
                        for idx, r in enumerate(full, 1):
                            inv_by_id.setdefault(r["item_id"], idx)
                        keep = set(r["item_id"] for r in full)
                        opts, reinfs = [], []
                        try:
                            for o in ge_db.get_item_options(CUR.commander_id):
                                if o["item_id"] in keep and o["item_id"] in inv_by_id:
                                    opts.append({"type": o["type"],
                                                 "value": o["value"],
                                                 "inv": inv_by_id[o["item_id"]]})
                        except Exception:
                            pass
                        try:
                            for rf in ge_db.get_item_reinforces(CUR.commander_id):
                                if rf["item_id"] in keep and rf["item_id"] in inv_by_id:
                                    reinfs.append({"inv": inv_by_id[rf["item_id"]],
                                                   "count": rf["count"]})
                        except Exception:
                            pass
                        try:
                            props_by_inv = {}
                            for pr in ge_db.get_item_properties(CUR.commander_id):
                                inv = inv_by_id.get(pr["item_id"])
                                if pr["item_id"] in keep and inv is not None:
                                    props_by_inv.setdefault(inv, []).append(pr)
                            props = [{"inv": i, "props": props_by_inv[i]}
                                     for i in sorted(props_by_inv)]
                        except Exception:
                            pass
                    except Exception:
                        inv_items = []
                        opts, reinfs, props = [], [], []
                    log("  inventory rows=%d opts=%d reinfs=%d props=%d"
                        % (len(inv_items), len(opts), len(reinfs), len(props)))
                except Exception:
                    point = 0
                    inv_items = []
                    opts, reinfs, props = [], [], []
                post = [
                    ("ZC_UPDATE_TABLE", build_zone_update_table()),
                    ("ZC_COMMANDER_POINT", build_commander_point(point)),
                    ("ZC_QUEST_INFO", build_quest_info()),
                    ("ZC_INVENTORY_ITEM_LIST",
                     build_inventory_item_list(inv_items)),
                    ("ZC_CHANGE_INV_MAXWEIGHT", build_max_weight()),
                    # The capacity the client draws comes from the IES cache, so
                    # send the stored Lock_* values or it shows its table default.
                    *[("ZC_UPDATE_PROPERTY_NUMBER " + name, pk)
                      for name, pk in build_lock_properties()],
                    ("ZC_INVENTORY_ITEM_OPTION_LIST",
                     build_inventory_option_list(opts)),
                    ("ZC_REINFORCE_INFO_LIST",
                     build_reinforce_list(reinfs)),
                    ("ZC_INVENTORY_ITEM_SOCKET_LIST",
                     build_inventory_socket_list()),
                    ("ZC_INVENTORY_ITEM_PROPERTY_LIST",
                     build_inventory_property_list(props if SEND_ITEM_PROPERTIES else None)),
                    ("ZC_INVENTORY_CASHITEM_LIST", build_inventory_cashitem_list()),
                    ("ZC_EQUIP_CASHITEM_LIST", build_equip_cashitem_list()),
                    ("ZC_LUMIN_CASHINFO_LIST", build_lumin_cashinfo_list()),
                    ("ZC_EQUIP_LUMIN_CASHINFO_LIST", build_equip_lumin_cashinfo_list()),
                    ("ZC_QUEST_STAGE_INFO", build_quest_stage_info()),
                    ("ZC_COMMANDER_INIT_INFO", build_commander_init_info()),
                    ("ZC_COMMANDER_PET_LIST", build_pet_list()),
                ]
                for _t in range(MAX_TELEPORT_TYPE):
                    post.append(("ZC_COMMWARP_LIST",
                                 build_commwarp_list(_t)))
                for name, pk in post:
                    try:
                        conn.sendall(pk)
                        log("SENT %s len=%d" % (name, len(pk)))
                    except Exception as exc:
                        log("  !! %s failed: %s" % (name, exc))
            elif cmd == CMD_CZ_GAME_READY:
                # Finish the world entry: without this the client stays on the
                # "moving to <map>" loading screen even though the map is loaded.
                conn.sendall(struct.pack("<H", CMD_ZC_ENTER_LEGACY_OBJECTS))
                log("SENT ZC_ENTER_LEGACY_OBJECTS cmd=%d" % CMD_ZC_ENTER_LEGACY_OBJECTS)
                # Unlock the stance/skill entries START_INFO carried. Without
                # these the client keeps every entry locked and the stance/skill
                # window draws nothing (see build_stance_list).
                for _h, (_sb, _kb) in CUR.blocks.items():
                    try:
                        if _sb:
                            conn.sendall(build_stance_list(_h, _sb))
                            log("SENT ZC_STANCE_LIST handle=%d stances=%d len=%d"
                                % (_h, len(_sb) // 14, 10 + len(_sb)))
                        if _kb:
                            conn.sendall(build_skill_list(_h, _kb))
                            log("SENT ZC_SKILL_LIST handle=%d skills=%d len=%d"
                                % (_h, len(_kb) // 10, 12 + len(_kb)))
                    except Exception as exc:
                        log("  !! stance/skill list failed: %s" % exc)
                # Town NPCs. Sent right after the world-entry handshake; the
                # client's SpawnStandingTriggerObject handler skips a handle it
                # already has, so re-sending is harmless.
                try:
                    for name, pk in build_npc_packets(ZONE_ID):
                        conn.sendall(pk)
                        log("SENT %s len=%d" % (name, len(pk)))
                except Exception as exc:
                    log("  !! NPC spawn failed: %s" % exc)
                # The mission LIST (Info Center -> Mission). A mission is a list
                # entry, not a map, so this is all "mission support" there is so
                # far: 269 rows, no lobby, no entry.
                _missions = mission_list_entries()
                if _missions:
                    conn.sendall(build_mission_list(_missions))
                    log("SENT ZC_RES_MISSION_LIST_STATUS missions=%d len=%d"
                        % (len(_missions), 8 + 8 * len(_missions)))
                # Now that the world is up, tell everybody about everybody, so
                # the players can SEE each other.
                try:
                    if CUR.handle and CUR.team:
                        rec = add_member(conn, CUR.handle, CUR.team[0],
                                         CUR.pos or (0, 0, 0))
                        announce_players(rec)
                except Exception as exc:
                    log("  !! player announce failed: %s" % exc)
            elif cmd == CMD_CZ_LOBBY_JOIN_ROOM and len(pt) >= 12:
                # PKS_CZ_LOBBY_JOIN_ROOM (fixed 28): 10-byte security header,
                # then WORD room id at +10 (password[16] after it, not checked).
                room_id = unprobe_room_id(struct.unpack_from("<H", pt, 10)[0])
                room = ROOMS.get(room_id)
                if not room:
                    log("  CZ_LOBBY_JOIN_ROOM room=%d: no such room" % room_id)
                else:
                    _leave_room()
                    room["members"].add(_room_token())
                    fam = _my_name()
                    if fam and fam not in room.setdefault("names", []):
                        room["names"].append(fam)
                    CUR.room_id = room_id
                    rec = build_room_record(
                        room["name"], room["master"], room["mission"], room_id,
                        cur=len(room["members"]), max_players=room["max"])
                    conn.sendall(build_room_info(rec, CMD_ZC_LOBBY_JOIN_ROOM))
                    log("SENT ZC_LOBBY_JOIN_ROOM room=%d name=%r players=%d/%d"
                        % (room_id, room["name"], len(room["members"]),
                           room["max"]))
                    conn.sendall(build_room_slots(room_id,
                                                  room.get("names") or [],
                                                  room["max"]))
                    conn.sendall(build_room_join(fam))
                    log("SENT ZC_ROOM_SLOT_LIST + ZC_ROOM_COUNT + ZC_ROOM_JOIN"
                        " room=%d slots=%d name=%r"
                        % (room_id, ROOM_SLOTS, fam))
                    # NO room list here - it would RE-OPEN the lobby the client
                    # just closed (its list handler ends with ShowWindow(true)).
            elif cmd == CMD_CZ_LOBBY_CREATE_ROOM and len(pt) >= 44:
                # PKS_CZ_LOBBY_CREATE_ROOM, fixed 68: 10-byte security header,
                # then name[32] at +10, missionId WORD at +42, password[20] and
                # four flag bytes after it (unused so far).
                name = pt[10:42].split(b"\x00")[0].decode("cp949", "replace")
                mission_id = struct.unpack_from("<H", pt, 42)[0]
                # One room per player: free the previous one first, or every
                # press leaves an orphan that still claims its creator.
                _leave_room()
                _room_seq[0] += 1
                room_id = _room_seq[0] & 0xFFFF
                ROOMS[room_id] = {
                    "name": name or "Room",
                    "master": _my_name(),
                    "mission": mission_id,
                    "max": mission_data.mission_max_player(mission_id),
                    "members": {_room_token()},
                    "names": [_my_name()],
                }
                CUR.room_id = room_id
                rec = build_room_record(
                    ROOMS[room_id]["name"], ROOMS[room_id]["master"],
                    mission_id, room_id,
                    cur=len(ROOMS[room_id]["members"]),
                    max_players=ROOMS[room_id]["max"])
                conn.sendall(build_room_info(rec, CMD_ZC_LOBBY_JOIN_ROOM))
                log("SENT ZC_LOBBY_JOIN_ROOM room=%d name=%r mission=%d (created)"
                    % (room_id, name, mission_id))
                conn.sendall(build_room_slots(room_id,
                                              ROOMS[room_id]["names"],
                                              ROOMS[room_id]["max"]))
                # 3707, not 3712: the client's `LobbyJoinRoom` handler is what
                # CLOSES the lobby and opens the room (`ui::Close("missionlobby")`).
                # Sending only ROOM_INFO leaves the player inside a room on the
                # server while their client still shows the lobby - and the list
                # then never refreshes, which looks like "the room vanished".
                #
                # NO room list here either: its handler ends with
                # `ShowWindow(true)`, so it would re-open the lobby behind the room.
            elif cmd == CMD_CZ_LOBBY_CLICK_CREATE_ROOM:
                # "Create a Room" pressed. Answer with the create dialog, echoing
                # the lobby type the click asked for - that is exactly what the
                # shipped server does (Commander_Packet.cpp:2069).
                conn.sendall(build_lobby_create_ui(CUR.lobby_type))
                log("SENT ZC_LOBBY_CREATEUI_OPEN preset=%r len=34"
                    % (CUR.lobby_type,))
            elif cmd == CMD_CZ_ROOM_START_MISSION:
                # PKS_CZ_ROOM_START_MISSION (fixed 10) = just the security header:
                # the master pressed "Mission Start".
                room = ROOMS.get(CUR.room_id)
                log("CZ_ROOM_START_MISSION room=%s master=%r players=%d"
                    % (CUR.room_id, (room or {}).get("master"),
                       len((room or {}).get("members") or ())))
                conn.sendall(build_room_all_ready(1))
                log("SENT ZC_ROOM_ALL_READY (start acknowledged; mission entry "
                    "not implemented)")
            elif cmd == CMD_CZ_ROOM_EXIT:
                # PKS_CZ_ROOM_EXIT: `bool isGetRoomList` at +10 (after the
                # 10-byte security header) - the client asks for the lobby list
                # in the same breath, because it is going back to it.
                want_list = len(pt) > 10 and pt[10] != 0
                room_id_out = CUR.room_id
                room = ROOMS.get(room_id_out)
                fam = _my_name()
                was_master = bool(room and room.get("master") == fam)
                _leave_room()
                # Leadership PASSES DOWN the list (live, owner 2026-10-24): the
                # master leaving does NOT destroy a room that still has members,
                # and nobody gets a popup. Only the LAST member's exit ends it,
                # and that is silent.
                if room and room.get("names"):
                    room["master"] = room["names"][0]
                    log("  room %s: leadership passes to %r"
                        % (room_id_out, room["master"]))
                # ZC_ROOM_EXIT only: it clears the leaver's own slot. ZC_ROOM_OUT
                # is for the people LEFT BEHIND - its handler shows the "Room
                # Leader is out" box and asks for the lobby list again
                # (`RoomOut` in GENormalNet_Mission.cpp). Sending it to the
                # leaver popped a notice and reopened the lobby; live closes
                # everything silently for the player who pressed Exit.
                conn.sendall(build_room_exit(fam))
                log("SENT ZC_ROOM_EXIT name=%r master=%d rooms_left=%d"
                    % (fam, was_master, len(ROOMS)))
                if want_list:
                    conn.sendall(build_lobby_room_list(CUR.account_id,
                                                       _room_records()))
                    log("SENT ZC_LOBBY_ROOM_LIST rooms=%d (after exit)"
                        % len(ROOMS))
            elif cmd == CMD_CZ_LOBBY_ROOM_LIST:
                # ALWAYS answer, even while inside a room. Live behaviour
                # (screenshot 2026-10-24): the lobby keeps showing the room you
                # are in, both windows can be open at once, and the row vanishes
                # only when the room stops existing (exit, or mission start).
                conn.sendall(build_lobby_room_list(CUR.account_id,
                                                   _room_records()))
                log("SENT ZC_LOBBY_ROOM_LIST lobby=%r rooms=%d (on request)"
                    % (CUR.lobby_type, len(ROOMS)))
            elif cmd == CMD_CZ_REQ_MISSION_LIST_STATUS:
                # The client asks for the mission list (Info Center -> Mission).
                # Answer with every mission we ship: the request's own fields are
                # not understood yet (the client does not hard-code the id, so no
                # capture of it exists either), and ignoring it would leave the
                # panel empty exactly as before.
                entries = mission_list_entries()
                if entries:
                    conn.sendall(build_mission_list(entries))
                    log("SENT ZC_RES_MISSION_LIST_STATUS missions=%d len=%d (on request)"
                        % (len(entries), 8 + 8 * len(entries)))
            elif cmd == CMD_CZ_MOVE_PC and len(pt) >= 23:
                # PKS_CZ_MOVE_PC: 10B header + BYTE charIndex + WORLDPOS(x,y,cell).
                # The client computes `cell` itself, so its moves are a source of
                # VALID navmesh cells (we log them to harvest a spawn).
                x, y, cell = struct.unpack_from("<iii", pt, 11)
                log("  CZ_MOVE_PC char=%d pos=(%d, %d, cell=%d)"
                    % (pt[10], x, y, cell))
                # SAVE the position, so a later CONTINUEGAME can resume here.
                # Without this the "saved position" is whatever the character
                # row was created with, and "continue" can never work - the
                # original server keeps the character's point in the character
                # slot (m_continueZonePos, set from character->point.x/y/z -
                # source2\geServer\BarrackServer\CharacterSlot.cpp:126).
                # The cell is the CLIENT's own coordinate, so it is stored as-is
                # (no reverse-lookup needed) in our own table - the shipped
                # CHARACTERS.ZPOS holds a z-HEIGHT, not a cell.
                try:
                    import ge_db as _db
                    _idx = pt[10]
                    # WHICH character moved. char 0 is the leader; 1 and 2 are the
                    # other members of a Multi Character Control team, and the
                    # client drives ALL THREE itself and reports each one - the
                    # log shows CZ_MOVE_PC char=0, char=1 and char=2 in the same
                    # session. So the handle used to answer, and the row saved,
                    # must be the character that MOVED.
                    _cid = None
                    if 0 <= _idx < len(CUR.team):
                        _cid = CUR.team[_idx].get("character_id")
                        if _cid:
                            # `CUR.map_id`, NOT a bare `zone_id`: there is no
                            # `zone_id` in this scope, so the call raised
                            # NameError, the except swallowed it, and the
                            # position was NEVER saved - "continue" then had
                            # nothing to resume at and always fell back to the
                            # map's default spawn.
                            save_position(_cid, x, y, cell, CUR.map_id)
                except Exception as exc:
                    _idx, _cid = 0, None
                    log("  !! could not save the position: %s" % exc)
                # Leader-only state. A team member's position must NOT drive the
                # leader's walk/click bookkeeping, or a click on an NPC starts
                # chasing from whichever character moved last.
                if not _idx:
                    # A walk starting, not the player's current spot: keep it and
                    # let pc_position work out where they are as it goes.
                    if CUR.pos is None or CUR.pos != (x, y, cell):
                        pc_start_walk((x, y, cell), reported=True)
                    # The client is moving by itself: remember when, so a chase
                    # knows the client's own pathing is still alive.
                    CUR.last_client_move_at = _ms()
                    # A walk the client started does not cancel a remembered click
                    # - the client walks the player to a clicked NPC itself and
                    # reports path steps that end far from it. It only means the
                    # walk is no longer ours (see note_player_move).
                    note_player_move((x, y))
                _moved = _cid or (CUR.handle if not _idx else None)
                if not _moved:
                    log("  .. CZ_MOVE_PC char=%d is not in the team - not echoed" % _idx)
                else:
                    # ECHO EVERY MOVER, including team members. This was tried the
                    # other way round (leader only) and it was WRONG: with no echo
                    # the followers do not move at all, which proves the client is
                    # NOT driving them - `ZC_MOVE_PC` is what moves a character
                    # (CNormalNet::MovePC -> gemove::ActorMove for any handle).
                    # Two of three then followed, so this is the better of the two.
                    conn.sendall(build_move_pc(_moved, (x, y, cell)))
                    log("  SENT ZC_MOVE_PC handle=%d -> (%d, %d, %d)"
                        % (_moved, x, y, cell))
                    # Tell the other players, so they see this one walk, and
                    # remember where they are. The walk is NOT over yet - the
                    # other clients need the stop when it ends, or the actor
                    # animates for ever (see stop_others_if_walk_over).
                    CUR.stop_sent = False
                    for m in members():
                        if m["conn"] is conn:
                            m["pos"] = (x, y, cell)
                            continue
                        try:
                            m["conn"].sendall(build_move_pc(_moved, (x, y, cell)))
                        except Exception as exc:
                            log("  !! move broadcast failed: %s" % exc)
            elif cmd == CMD_CZ_REQ_TOWN_MOVE and len(pt) >= 14:
                # PKS_CZ_REQ_TOWN_MOVE: 10B header + int movetype (1..4).
                movetype = struct.unpack_from("<i", pt, 10)[0]
                log("  CZ_REQ_TOWN_MOVE movetype=%d" % movetype)
                pos = town_move(ZONE_ID, movetype)
                if pos and CUR.handle:
                    # Mirror CCommander::OnReqTownMove: it calls SetPos, which
                    # warps the PC (OM_WARP -> Move() then BroadcastStopMovement).
                    # Move alone would make the client walk there at normal
                    # speed, so the stop packet is what makes it instant.
                    conn.sendall(build_move_pc(CUR.handle, pos))
                    conn.sendall(build_stop_pc(CUR.handle, pos))
                    # A warp, not a walk: no interpolation, the player is there.
                    CUR.pos = pos
                    CUR.last_client_pos = pos
                    CUR.move = None
                    log("  SENT town move -> (%d, %d, %d)" % pos)
                else:
                    log("  !! no town point %d for zone %s" % (movetype, ZONE_ID))
            elif cmd == CMD_CZ_CHANGE_STANCE and len(pt) >= 15:
                # 10B security header + BYTE charIndex + DWORD stanceType.
                char_index = pt[10]
                stance = struct.unpack_from("<I", pt, 11)[0]
                log("  CZ_CHANGE_STANCE char=%d stance=%d" % (char_index, stance))
                target = CUR.team[char_index] if 0 <= char_index < len(CUR.team) else None
                if target and stance in stance_data.job_stances(target["job"]):
                    conn.sendall(build_change_stance(target["character_id"], stance))
                    log("  SENT ZC_CHANGE_STANCE handle=%d stance=%d (%s)"
                        % (target["character_id"], stance,
                           stance_data.summary(stance)))
                    if target["character_id"] == CUR.handle and not FORCE_STANCE:
                        CUR.stance = stance
                        log("  leader speed now %.1f u/s" % move_speed())
                else:
                    # The original ignores a stance the PC does not have.
                    log("  ignored: stance %d is not in that character's list"
                        % stance)
            elif cmd == CMD_CZ_SYSMENU and len(pt) >= 12:
                # 10B security header + BYTE selection + bool. SYSMENU_BARRACK=0.
                selection = pt[10]
                log("  CZ_SYSMENU selection=%d" % selection)
                if selection == SYSMENU_BARRACK:
                    conn.sendall(build_move_barrack())
                    conn.sendall(build_logout_ok())
                    log("SENT ZC_MOVE_BARRACK + ZC_LOGOUT_OK - waiting for the "
                        "client to drop this session itself")
                    # IMPORTANT: do NOT close here. The client's
                    # CNormalMode::ProcessModeSwitch runs on its next update,
                    # does Net()->Disconnect() itself and only then reconnects to
                    # the LOGIN server and sends CB_LOGIN. Closing first makes the
                    # client process a disconnect (-> login mode) before the
                    # switch, which is exactly the "dropped at login" symptom.
                    # The loop ends when the client closes (recv returns 0).
                else:
                    log("  sysmenu %d not handled (barrack only for now)"
                        % selection)
            elif cmd == CMD_CZ_CLICK_TRIGGER and len(pt) >= TR_CLICK_HANDLE + 4:
                # PKS_CZ_CLICK_TRIGGER: 10B header + BYTE charIndex + BYTE
                # longdistance + DWORD trigger handle.
                handle = struct.unpack_from("<I", pt, TR_CLICK_HANDLE)[0]
                longdistance = bool(pt[11])
                log("  CZ_CLICK_TRIGGER char=%d long=%d handle=0x%08x"
                    % (pt[10], pt[11], handle))
                if npc_handle_index(handle) is None:
                    log("  !! click on unknown handle 0x%08x" % handle)
                else:
                    # Remember it and answer on arrival (see try_pending_click).
                    # An in-range click with the player standing still opens the
                    # dialog right here, exactly as before.
                    remember_click(handle, longdistance)
                    try_pending_click(conn)
            elif cmd == CMD_CZ_USEITEM_TARGET and len(pt) >= 20:
                # Item use. The only field we need is invIndex at +0xb; the
                # client numbers the bag 1..N exactly like the item list we
                # sent, so CUR.last_inv is the lookup table.
                inv_index = struct.unpack_from("<I", pt, 0x0B)[0]
                bag = CUR.last_inv or []
                row = bag[inv_index - 1] if 0 < inv_index <= len(bag) else None
                log("  CZ_USEITEM_TARGET invIndex=%d -> %s"
                    % (inv_index, row["type"] if row else "OUT OF RANGE"))
                if row is not None:
                    import ge_db
                    ok, how, item_class, remain = ge_db.consume_one(
                        CUR.commander_id, row["item_id"], 1)
                    log("    use class=%d -> %s"
                        % (item_class, how if ok else "FAILED (%s)" % how))
                    if ok:
                        effect = ITEM_EFFECTS.get(str(item_class))
                        if effect and effect.get("scp") == "SCP_USE_ITEM_HPUP":
                            heal_pc(conn, int(effect.get("arg1", 0)))
                        if remain > 0:
                            row["count"] = remain
                            conn.sendall(build_add_item(item_class, inv_index,
                                                        remain))
                            log("    SENT ZC_ADDITEM class=%d invIndex=%d n=%d"
                                % (item_class, inv_index, remain))
                        else:
                            del bag[inv_index - 1]
                            conn.sendall(build_remove_item(inv_index, 0))
                            log("    SENT ZC_REMOVEITEM invIndex=%d" % inv_index)
                        replied = True
            elif cmd == CMD_CZ_BUYITEM and len(pt) >= 20:
                # Records are 13 bytes: BYTE flag; DWORD type (client pointer -
                # IGNORE); DWORD count; DWORD index. The original prices from the
                # ITEM row and pays from the vis stack (class 30000), then tells
                # the client. We refresh the whole bag instead of sending
                # ZC_ADDITEM, so the client re-reads what the DB now holds.
                import ge_db
                import item_prices
                shop_type, n = struct.unpack_from("<II", pt, 12)
                log("  CZ_BUYITEM shopType=%d records=%d shop=%s"
                    % (shop_type, n,
                       CUR.last_shop["group"] if CUR.last_shop else "?"))
                if not CUR.last_shop:
                    log("    ignored: no shop window was sent this session")
                elif n <= 0 or n > 32 or BUY_HEADER + n * BUY_ITEM_SIZE > len(pt):
                    log("    ignored: bad record count")
                elif shop_type == SHOP_TYPE_CUSTOM:
                    # Custom shop: the SERVER sent the prices, so there is no vis
                    # involved. CTxBuyCustomItem pays the row's own
                    # (pricetype, count) pair(s); index maps to the list we sent.
                    entries = CUR.last_shop.get("entries") or []
                    if CUR.last_shop.get("kind") != "custom" or not entries:
                        log("    ignored: shopType=1 but the last window was not "
                            "a custom shop")
                        continue
                    buys, costs = [], {}
                    for i in range(n):
                        off = BUY_HEADER + i * BUY_ITEM_SIZE
                        junk, count, index = struct.unpack_from("<III", pt, off)
                        e = entries[index] if 0 <= index < len(entries) else None
                        log("    rec%d index=%d count=%d -> %s"
                            % (i, index, count, e))
                        if e is None or count <= 0 or count > 99:
                            log("    skipped: bad index/count")
                            continue
                        sell, p1, c1, p2, c2 = e
                        buys.append((sell, count, item_prices.max_stack(sell)))
                        if c1:
                            costs[p1] = costs.get(p1, 0) + c1 * count
                        if c2:
                            costs[p2] = costs.get(p2, 0) + c2 * count
                    log("    barter costs=%s" % sorted(costs.items()))
                    if not buys:
                        log("    nothing to buy")
                    else:
                        ok, how, added = ge_db.apply_custom_purchase(
                            CUR.commander_id, buys, list(costs.items()))
                        log("    custom buy -> %s"
                            % (how if ok else "FAILED (%s)" % how))
                        spans = plan_consumption(CUR.last_inv, costs) if ok else []
                        if ok:
                            for inv_idx, remain in spans:
                                conn.sendall(build_remove_item(
                                    inv_idx, remain, REMOVE_ITEM_CUSTOM_SHOP))
                            inv_remove(CUR.last_inv, spans)
                            for item_id, cls, cnt in added:
                                inv_idx = inv_add(CUR.last_inv, item_id, cls, cnt)
                                conn.sendall(build_add_item(
                                    cls, inv_idx, cnt,
                                    ADD_ITEM_CUSTOM_SHOP_BUY))
                                log("    SENT ZC_ADDITEM %s invIndex=%d n=%d"
                                    % (cls, inv_idx, cnt))
                            log("    SENT %d ZC_REMOVEITEM (custom-shop counters)"
                                % len(spans))
                else:
                    items = CUR.last_shop["items"]
                    buys = []
                    for i in range(n):
                        off = BUY_HEADER + i * BUY_ITEM_SIZE
                        junk, count, index = struct.unpack_from("<III", pt, off)
                        item = items[index] if 0 <= index < len(items) else None
                        log("    rec%d index=%d count=%d type-field=0x%08x "
                            "-> item=%s" % (i, index, count, junk, item))
                        if item is None or count <= 0 or count > 99:
                            log("    skipped: bad index/count")
                            continue
                        buys.append((item, count))
                    inv = ge_db.get_inventory_full(CUR.commander_id)
                    vis = next((r for r in inv if r["type"] == 30000), None)
                    total = sum(item_prices.price(it) * c for it, c in buys)
                    log("    price total=%d vis, balance=%d"
                        % (total, vis["count"] if vis else 0))
                    if not buys:
                        pass
                    elif not vis or vis["count"] < total:
                        log("    REJECTED: not enough vis")
                    else:
                        bought, paid = [], 0
                        for it, c in buys:
                            cost = item_prices.price(it) * c
                            ok, how, new_id, new_cnt = ge_db.apply_purchase(
                                CUR.commander_id, vis["item_id"], cost, it, c,
                                item_prices.max_stack(it))
                            log("    buy item=%s x%d cost=%d -> %s"
                                % (it, c, cost,
                                   how if ok else "FAILED (%s)" % how))
                            if not ok:
                                break
                            bought.append((it, new_id, new_cnt, cost, c))
                            paid += cost
                        if bought and paid:
                            for it, new_id, new_cnt, _cost, _qty in bought:
                                idx = inv_add(CUR.last_inv, new_id, it, new_cnt)
                                conn.sendall(build_add_item(
                                    it, idx, new_cnt, ADD_ITEM_BUY))
                                log("    SENT ZC_ADDITEM %s invIndex=%d n=%d"
                                    % (it, idx, new_cnt))
                            vis_idx = next(
                                (i for i, r in enumerate(CUR.last_inv or [], 1)
                                 if r["item_id"] == vis["item_id"]), None)
                            if vis_idx:
                                left = vis["count"] - paid
                                conn.sendall(build_remove_item(
                                    vis_idx, left, REMOVE_ITEM_BUY))
                                inv_remove(CUR.last_inv, [(vis_idx, left)])
                                log("    SENT ZC_REMOVEITEM vis -> %d" % left)
            elif cmd == CMD_CZ_SELLITEM and len(pt) >= SELL_HEADER:
                # Mirror of the buy path: records are {DWORD invIndex; DWORD
                # count} and invIndex numbers CUR.last_inv 1..N. The price is the
                # item table's OWN SellPrice (CInvItem::GetSellPrice), never
                # anything the client sends, and it is paid into the vis stack.
                import ge_db
                import item_prices
                n = struct.unpack_from("<I", pt, 12)[0]
                log("  CZ_SELLITEM records=%d shop=%s"
                    % (n, CUR.last_shop["group"] if CUR.last_shop else "?"))
                log("    hex: %s" % hexs(pt[:96]))
                if not CUR.last_inv:
                    log("    ignored: no inventory list was sent this session")
                elif n <= 0 or n > 32 or SELL_HEADER + n * SELL_ITEM_SIZE > len(pt):
                    log("    ignored: bad record count")
                else:
                    vis = next((r for r in CUR.last_inv if r["type"] == VIS_CLASS), None)
                    sales = []
                    for i in range(n):
                        off = SELL_HEADER + i * SELL_ITEM_SIZE
                        idx, cnt = struct.unpack_from("<II", pt, off)
                        row = (CUR.last_inv[idx - 1]
                               if 1 <= idx <= len(CUR.last_inv) else None)
                        log("    rec%d invIndex=%d count=%d -> item=%s"
                            % (i, idx, cnt,
                               "%s x%d" % (row["type"], row["count"])
                               if row else None))
                        if row is None or cnt <= 0 or cnt > row["count"]:
                            log("    skipped: bad index/count")
                            continue
                        if row["type"] == VIS_CLASS:
                            log("    skipped: that is the vis stack itself")
                            continue
                        price = item_prices.sell_price(row["type"])
                        if price <= 0 or not item_prices.venderable(row["type"]):
                            log("    skipped: item is not sellable (SellPrice=%d)"
                                % price)
                            continue
                        sales.append((idx, row, cnt, price))
                    if not vis:
                        log("    REJECTED: no vis stack in the bag")
                    elif not sales:
                        log("    nothing to sell")
                    else:
                        gained = 0
                        removes = []
                        for idx, row, cnt, price in sales:
                            ok, how = ge_db.apply_sale(
                                CUR.commander_id, vis["item_id"], row["item_id"], cnt,
                                price * cnt)
                            log("    sell item=%s x%d gain=%d -> %s"
                                % (row["type"], cnt, price * cnt,
                                   how if ok else "FAILED (%s)" % how))
                            if not ok:
                                break
                            left = row["count"] - cnt
                            removes.append((idx, left, row["type"], cnt,
                                            price * cnt))
                            inv_remove(CUR.last_inv, [(idx, left)])
                            gained += price * cnt
                        if gained:
                            vis_idx = next(
                                (i for i, r in enumerate(CUR.last_inv or [], 1)
                                 if r["item_id"] == vis["item_id"]), None)
                            if vis_idx:
                                new_total = vis["count"] + gained
                                conn.sendall(build_add_item(
                                    VIS_CLASS, vis_idx, new_total,
                                    ADD_ITEM_SELL))
                                inv_add(CUR.last_inv, vis["item_id"], VIS_CLASS,
                                        new_total)
                                log("    SENT ZC_ADDITEM vis invIndex=%d n=%d"
                                    % (vis_idx, new_total))
                        # Removes LAST: their handler ends with
                        # ui::UpdateData("shop"), so the shop panel rebuilds
                        # after the money has already changed.
                        for idx, left, cls, cnt, gain in removes:
                            conn.sendall(build_remove_item(
                                idx, left, REMOVE_ITEM_SELL))
                            log("    SENT ZC_REMOVEITEM invIndex=%d -> %d "
                                "(sold %s)" % (idx, left, cls))
                            msg = "Sold %s x%d for %s vis" % (
                                item_prices.name(cls) or str(cls), cnt,
                                "{:,}".format(int(gain)))
                            conn.sendall(build_system_message(msg))
                            log("    SENT ZC_MESSAGE %r" % msg)
            elif cmd == CMD_CZ_DIALOG_SELECT and len(pt) >= 12:
                log("  CZ_DIALOG_SELECT char=%d select=%d" % (pt[10], pt[11]))
                resume_script(conn, pt[11])
            elif cmd == CMD_CZ_ACK_DIALOG and len(pt) >= 12:
                # The player pressed OK (or closed the dialog). Only now can the
                # client leave the modal dialog camera; without this reply it
                # sits there looking at the NPC with no way back.
                log("  CZ_ACK_DIALOG char=%d next=%d" % (pt[10], pt[11]))
                if CUR.pending_shop is not None:
                    # The OK opens the shop: send the list but do NOT close
                    # the dialog. The client stays in its dialog mode, which is
                    # what keeps the character in place while shopping, and its
                    # shop CLOSE sends this same CZ_Ack_Dialog
                    # (CGENormalUIHandler::OnMsgCloseNpcTrade), so the next one
                    # falls through to the close below.
                    group = CUR.pending_shop["group"]
                    kind = CUR.pending_shop.get("kind", "normal")
                    entries = CUR.pending_shop.get("entries") or []
                    items = CUR.pending_shop.get("items") or []
                    CUR.pending_shop = None
                    if kind == "custom":
                        pk = build_custom_trade_dialog(entries)
                        conn.sendall(pk)
                        CUR.last_shop = {"group": group, "kind": "custom",
                                     "items": [e[0] for e in entries],
                                     "entries": list(entries)}
                        log("  SENT ZC_DIALOG_NPC_CUSTOM_TRADE group=%s rows=%d "
                            "bytes=%d (dialog stays open)"
                            % (group, len(entries), len(pk)))
                    else:
                        pk = build_trade_dialog(items, group)
                        conn.sendall(pk)
                        CUR.last_shop = {"group": group, "items": list(items)}
                        log("  SENT ZC_DIALOG_NPC_TRADE group=%s items=%d bytes=%d "
                            "layout=%s (dialog stays open)"
                            % (group, len(items), len(pk), _trade_layout()))
                    continue
                conn.sendall(build_dialog_close())
                log("  SENT ZC_DIALOG_CLOSE")
    except socket.timeout:
        log("no more data (timeout)")
    except Exception as exc:
        log("error: %s" % exc)
    finally:
        # The player is leaving: this is the moment the "Last Point" button
        # will read, so the position MUST be written now (the in-memory
        # throttle may not have written it yet).
        try:
            _idx = 0
            if CUR.team and 0 <= _idx < len(CUR.team):
                _cid = CUR.team[_idx].get("character_id")
                if _cid and CUR.pos:
                    save_position(_cid, CUR.pos[0], CUR.pos[1], CUR.pos[2],
                                  CUR.map_id, force=True)
        except Exception as exc:
            log("  !! could not save the position on leaving: %s" % exc)
        # The player is leaving: take them out of the zone, and tell the
        # others, so their client removes the character.
        rec = remove_member(conn)
        if rec is not None:
            try:
                broadcast(build_leave_pc(rec["handle"]), skip_conn=conn)
                log("  SENT ZC_LEAVE_PC handle=%d" % rec["handle"])
            except Exception as exc:
                log("  !! leave broadcast failed: %s" % exc)
        # The player is leaving: take them out of the zone's head count, so
        # the next one into an empty zone can set the map again.
        if CUR.entered:
            global ZONE_PLAYERS
            with ZONE_LOCK:
                ZONE_PLAYERS = max(0, ZONE_PLAYERS - 1)
            CUR.entered = False
            log("  player left - players in this zone: %d" % ZONE_PLAYERS)
        conn.close()
        log("CLOSED %s:%d (replied=%s)" % (addr[0], addr[1], replied))


FORCE_COUNT = int(sys.argv[1]) if len(sys.argv) > 1 else None
# The commander's zone (map) id - picks the map's NPC list. This is the ZONE's
# map, not the player's: one zone process serves one map, exactly like the
# original (one zone process per map, per channel).
ZONE_ID = 0
# How many players are in this zone right now. A zone serves ONE map, so the
# map is set by the first player to enter an empty zone and then kept.
ZONE_PLAYERS = 0
ZONE_LOCK = threading.Lock()
# The commander used when a player's account cannot be resolved. It is the
# commander the port has always used, so an unknown account still gets a
# playable character instead of nothing.
DEFAULT_COMMANDER = 24


# --------------------------------------------------------------------------
# per-player state
# --------------------------------------------------------------------------
# The zone used to keep ONE player's state in module-level globals, so it could
# only ever serve one player: the second connection waited for the first to
# disconnect. Each connection is now handled on its own thread, so the
# per-player state lives in a thread-local - the connection's thread IS the
# player. The zone-level state (the map, the NPC list, the caches) stays in
# module globals, because it really is shared.
class _PlayerState(threading.local):
    """The state of the player whose connection this thread is serving."""

    def __init__(self):
        self.handle = None
        self.pos = None
        self.move = None
        self.move_from_client = False
        # The last position written to the database, and when (see SAVE_INTERVAL).
        self.last_saved_pos = None
        self.last_saved_at = 0.0
        # When the client last reported a move of its own (its own pathing).
        self.last_client_move_at = 0
        # The last position the CLIENT itself said it was heading to. This is
        # the only position statement we trust: the interpolated one is a guess,
        # and it drifts badly while a server-invented walk is in flight.
        self.last_client_pos = None
        # When that report arrived, and when we last issued a walk of our own.
        # The later of the two is the better "where is the player now": while a
        # leg we sent is in flight the client is silent, so its report is older
        # and the interpolated position (the leg's own destination, once its ETA
        # has passed) is the right answer. Comparing the two timestamps is what
        # makes a leg CHAIN advance instead of re-sending the first leg forever.
        self.last_client_pos_at = 0
        self.chase_issued_at = 0
        self.pending_click = None
        self.click_lock = None
        self.pending_shop = None
        self.pending_script = None      # a script suspended on a question
        self.last_shop = None
        self.last_inv = None
        self.block = None
        self.blocks = {}
        self.team = []
        self.stance = None
        # The socket this player's thread owns, and whether the other players
        # have been told that this player's walk is over.
        self.conn = None
        self.stop_sent = True
        # Which account / commander this player is, and which map they are in.
        self.account_id = 0
        self.commander_id = 0
        # the lobby type a `ShowLobby` click asked for, so a later
        # CZ_LOBBY_ROOM_LIST can be answered with the same one
        self.lobby_type = ""
        # the name START_INFO gave the client (cm["name"]); the room leader
        # test compares against THIS, not the start-game packet id
        self.commander_name = ""
        self.map_id = 0
        self.entered = False


# CUR is the player this thread is serving. `CUR.pos` is where that player is;
# `CUR.team` is that player's team.
CUR = _PlayerState()


# --------------------------------------------------------------------------
# the world module: this zone announces which map it serves
# --------------------------------------------------------------------------
# The world module owns the zone registry (see world.py). This zone tells it
# "I serve map X at this address", and the barrack asks the same module where
# to send the client - so the address is not a constant any more. The original
# server did the same thing with a global server that pushed "Start Zone
# Server [id]" / "Stop Zone Server [id]" to the barrack.
ZONE_PRESENCE = None
ZONE_HEARTBEAT = None


def announce_zone(zone_id):
    """Tell the world module which map this zone is serving.

    The presence file is refreshed by a heartbeat while the zone runs, so the
    barrack can tell a live zone from a dead one.
    """
    global ZONE_PRESENCE, ZONE_HEARTBEAT
    name = world.map_name(zone_id)
    path = world.announce(zone_id, HOST, PORT, channel=0, map_name=name)
    if path is None:
        return
    if ZONE_HEARTBEAT is not None:
        ZONE_HEARTBEAT.stop()
    ZONE_PRESENCE = path
    ZONE_HEARTBEAT = world.Heartbeat(path, map_id=zone_id, map_name=name).start()
    log("  announced: zone %s (%s) at %s:%d" % (zone_id, name, HOST, PORT))


def withdraw_zone():
    """Remove our presence file on a clean shutdown."""
    global ZONE_PRESENCE, ZONE_HEARTBEAT
    if ZONE_HEARTBEAT is not None:
        ZONE_HEARTBEAT.stop()
        ZONE_HEARTBEAT = None
    ZONE_PRESENCE = None


def main():
    # The zone logs to STDOUT. When it is started detached (Start-Process, or
    # by a supervisor) that output goes NOWHERE, and the log file stops
    # updating - which then reads as "the zone never got the request" when in
    # fact it did. So tee every line into the log file as well.
    try:
        # `LOGS`, not `LOGS_DIR`: ge_paths exports LOGS. The wrong name made
        # the whole tee fail, so the log file stayed frozen and read as "the
        # zone never got the request" when it had.
        log_path = os.path.join(LOGS, "zone_server.log")
        os.makedirs(LOGS, exist_ok=True)
        if not os.path.exists(log_path):
            open(log_path, "w", encoding="utf-8").close()
        _fh = open(log_path, "a", encoding="utf-8")
        _orig_print = print

        def _tee(*args, **kw):
            _orig_print(*args, **kw)
            try:
                _fh.write(" ".join(str(a) for a in args) + "\n")
                _fh.flush()
            except Exception:
                pass

        globals()["print"] = _tee
    except Exception as exc:
        print("!! could not open the zone log file: %s" % exc)

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    # On Windows, SO_REUSEADDR lets a SECOND server bind the same port, and
    # then the two fight over the connections - which looks exactly like a
    # server that "sometimes" ignores a packet. SO_EXCLUSIVEADDRUSE makes the
    # second start fail loudly instead.
    if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    log("zone server UP on %s:%d (force_count=%s, team_file=%s)"
        % (HOST, PORT, FORCE_COUNT, os.path.abspath(TEAM_FILE)))
    # Warm the data cache BEFORE accepting a player. The tables are decoded on
    # first use, and doing that while the first player is already clicking
    # made the walk timing slip: the click test failed on a cold cache and
    # passed on a warm one (measured 2026-10-22). A real player's first
    # click-to-move had the same race. Decoding now moves the cost to startup,
    # where it is visible, instead of to the player's first move, where it is
    # not.
    #
    # Only the SHARED tables are warmed here: the map table (zone id ->
    # world name) and the NPC types (every map's npcgen rows point into it).
    # The per-map npcgen table is NOT: which map this zone serves is decided
    # by the first player to enter (one zone process serves one map, named by
    # the team the barrack saved), and warming all 376 worlds would take
    # minutes. That one map's table is decoded when its first player arrives.
    try:
        import npc_data
        _t0 = _ms()
        _maps = npc_data.load_map_table()
        _types = npc_data.load_npc_types()
        log("  data cache warm: %d maps, %d NPC types (%.0fs)"
            % (len(_maps), len(_types), (_ms() - _t0) / 1000.0))
    except Exception as exc:
        log("  !! could not warm the data cache: %s" % exc)
    # The SCRIPT index too. It is built by walking ~4,280 .scp files, and it
    # costs 6.2 s (measured). Without this, the FIRST click on an NPC built it
    # inside the update tick - and the tick is what answers a click on
    # arrival, so the dialog was 6 s late and the player had already walked
    # on. The symptom was "the dialog never opened" on a cold cache.
    try:
        import npc_script
        _t0 = _ms()
        _n = len(npc_script.index())
        log("  script index warm: %d scripts (%.0fs)"
            % (_n, (_ms() - _t0) / 1000.0))
    except Exception as exc:
        log("  !! could not warm the script index: %s" % exc)
    # The locale id table. It is PERMANENT and checked in - the server only
    # looks ids up in it, it never works them out. If it is missing, say so
    # loudly: a missing table means every dialog falls back to a bare
    # number, and a silent fallback is worse than the bug it hides.
    try:
        import ge_locale
        if os.path.exists(ge_locale.ID_MAP):
            log("  locale id table: %d translations (%s)"
                % (len(ge_locale.id_map()), ge_locale.ID_MAP))
        else:
            log("  !! the locale id table is MISSING (%s) - every dialog will"
                " show a bare number.  Run: python make_locale_map.py"
                % ge_locale.ID_MAP)
    except Exception as exc:
        log("  !! could not check the locale id table: %s" % exc)
    log("move speed follows the leader's stance once the team loads "
        "(stance id %d) - table: %d rows"
        % (START_STANCE, stance_data.stance_count()))
    # Announce the map we serve. The zone reads the same commander's map the
    # team loader does, so this is the map a player will land in.
    try:
        import ge_db
        cm = ge_db.get_commander(24)
        if cm:
            announce_zone(cm.get("zone_id", 0))
    except Exception as exc:
        log("  !! could not announce to the world module: %s" % exc)
    # Each connection gets its own thread, so several players can be in the
    # world at once. The per-player state lives in CUR, which is thread-local
    # (see _PlayerState), so the threads never share it.
    try:
        while True:
            conn, addr = srv.accept()
            t = threading.Thread(target=handle, args=(conn, addr), daemon=True)
            t.start()
    except KeyboardInterrupt:
        log("STOP by user")
    finally:
        withdraw_zone()
        srv.close()


if __name__ == "__main__":
    main()
