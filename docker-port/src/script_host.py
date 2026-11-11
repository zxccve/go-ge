"""Run a `.scp` script for real, with stub builtins.

The server used to only *read* scripts (`npc_script`). This module actually
executes them: it translates the file with `scp_to_lua`, loads it into a real
Lua, and answers the server functions the script calls. Anything not answered
is recorded and returns 0, so a script runs to the end instead of dying on the
first unknown name.

Why stubs first: there are 2,447 distinct builtins and the top 11 cover half of
all calls (`data\\script_builtins.txt`). A stub that records the call is enough
to see what a script DOES - which is the debugging tool for the rest.

    python script_host.py --selftest           # run a few real click scripts
    python script_host.py --selftest --trace   # ... printing every call

Nothing here writes to the database. `Tx*` calls are recorded, not applied.
"""

import argparse
import json
import os
import threading
import re
import sys

from ge_paths import GE_SCRIPT, OUT
from scp_to_lua import translate_file

FUNC_RE = re.compile(r"\bfunc\s+([A-Za-z_][\w.]*)\s*\(")

# Lua 5.0 helpers the game's scripts still use, plus aliases removed later.
PRELUDE = """
table.getn = function(t) return #t end
if not table.foreach then table.foreach = function(t, f)
  for k, v in pairs(t) do local r = f(k, v) if r ~= nil then return r end end
end end
if not unpack then unpack = table.unpack end
if not string.gfind then string.gfind = string.gmatch end
if not math.mod then math.mod = math.fmod end
-- unknown globals become recording stubs instead of nil
setmetatable(_G, {__index = function(t, k)
  local f = function(...) return __on_missing(k, ...) end
  rawset(t, k, f)
  return f
end})
"""


# The nation this build serves. Scripts branch on it (`if (nationName == 'JPN')`)
# and the data here is the English/USA client, so 'USA' is the honest answer.
SERVICE_NATION = "USA"


class PropBag:
    """A game object seen from Lua: `commander.Quest_00001` reads and writes.

    Lupa indexes a Python object through `__getitem__`, so both protocols are
    here. A missing property reads as 0, which is what the IES layer does for an
    unset integer.
    """

    def __init__(self, kind="object", **props):
        object.__setattr__(self, "_d", dict(props))
        object.__setattr__(self, "_kind", kind)

    def __getitem__(self, key):
        return self._d.get(key, 0)

    def __setitem__(self, key, value):
        self._d[key] = value

    __getattr__ = __getitem__

    def __setattr__(self, key, value):
        self._d[key] = value

    def get(self, key, default=0):
        return self._d.get(key, default)

    def __repr__(self):
        return "<%s %r>" % (self._kind, self._d)


class CommanderBag(PropBag):
    """The PLAYER's own object: `pc.<Property>` is a real read, not always 0.

    A quest guards itself with the state it wrote last time
    (`if (pc.Quest_00001 == 0)`), so a bag that answers 0 for every name makes
    every guard pass and the quest pays out on every click. That is exactly what
    a plain `PropBag` did. The rows are loaded once per click from
    `COMMANDER_PROPERTY`, and a property the running transaction has already
    queued wins over the stored one, so a script that sets then reads agrees
    with itself.
    """

    def __init__(self, host, commander_id=None, **props):
        PropBag.__init__(self, "pc", **props)
        # object.__setattr__: PropBag.__setattr__ writes into the property dict
        object.__setattr__(self, "_host", host)
        object.__setattr__(self, "_commander",
                           int(commander_id) if commander_id else None)
        object.__setattr__(self, "_loaded", False)

    def _ensure_loaded(self):
        if self._loaded or not self._commander:
            return
        object.__setattr__(self, "_loaded", True)
        try:
            import ge_db
            for name, value in ge_db.get_commander_quest_states(
                    self._commander).items():
                self._d.setdefault(name, value)
        except Exception as exc:
            print("script_host: could not load properties of commander %s: %s"
                  % (self._commander, exc))

    def __getitem__(self, key):
        queued = self._host.pending_prop(self._commander, key)
        if queued is not None:
            return queued
        self._ensure_loaded()
        return self._d.get(key, 0)

    # PropBag binds __getattr__ to ITS __getitem__; a subclass must rebind it.
    __getattr__ = __getitem__


class Call:
    __slots__ = ("name", "args")

    def __init__(self, name, args):
        self.name = name
        self.args = args

    def __repr__(self):
        shown = ", ".join(repr(a) for a in self.args)
        return "%s(%s)" % (self.name, shown)


class ScriptHost:
    """One Lua runtime holding the game's scripts and a stub server API."""

    def __init__(self, root=None, zone_id=0):
        from lupa import LuaRuntime
        self.root = root or GE_SCRIPT
        self.zone_id = zone_id
        # where the player is, for `GetPos(pc)`. Set per click by the zone; the
        # offline sweep leaves it None and every object is then at 0,0,0.
        self.position = None
        self.lua = LuaRuntime(unpack_returned_tuples=True)
        self.calls = []
        self.ticks = 0
        self.commander_id = None       # fallback for offline runs
        self.commander_name = None     # the family name a script prints as CommName
        self.account_id = None         # for GetCommanderAccID
        self._inv_used = None          # used slots, cached per click
        self._local = threading.local()   # per-thread commander, one host, many players
        self._tx = {}
        self._tx_next = 0
        # item changes a COMMITTED transaction made, for the caller to send to the
        # client: (kind, class_id, count, item_id). Only filled after a commit - a
        # rolled-back transaction must leave the client's bag untouched.
        self.item_events = []
        # property names a COMMITTED transaction wrote, so the caller can refresh
        # the quest panel. Same rule as item_events: only after a commit.
        self.prop_events = []
        # windows a script asked for: (kind, group). Sent by the zone AFTER the
        # branch ran, because which window depends on the player's answer.
        self.trade_events = []
        # the mission LOBBY a script asked for: (type, layer, allowCreate).
        # Opened by the zone after the branch ran, same rule as the shops.
        self.lobby_events = []
        self._prop_map = None
        # Decoded IES tables, by idspace name. Tables never change at runtime, so
        # this cache belongs to the host, not to one click.
        self._dt_cache = {}
        # The team the barrack handed over, and the character rows behind it,
        # both keyed by commander id (one host serves every player).
        self._team_cache = {}
        self._char_cache = {}
        # positions a script asked for via `SetPos`, sent by the zone after the
        # branch has run: (is_player, x, y, z).
        self.setpos_events = []
        # lines an NPC spoke via `ChatNPC`, sent by the zone after the branch:
        # (handle, text). ZC_CHAT, NOT the balloon packet.
        self.chat_events = []
        # Loop-body budget: a runaway `while(1)` raises instead of hanging.
        self.tick_budget = 5_000_000
        self.loaded = set()
        self._index = None
        self.lua.globals()["__on_missing"] = self._missing
        # A dialog that YIELDS still has to be in the call log: the sweep counts
        # "opens a dialog" from there, and a yielded box makes no Python call.
        self.lua.globals()["__record_dialog"] = lambda name, text: (
            self.calls.append(Call(str(name), (0, text))))
        self.lua.globals()["_vtick"] = self._tick
        self.lua.execute(PRELUDE)
        # `_G.name` would hit the __index stub above and hand back a recorder,
        # so a script function that does not exist yet would look real. Read the
        # table itself.
        self._rawget = self.lua.eval("function(k) return rawget(_G, k) end")
        self._install_api()
        self._load_libraries()

    # -- server API ------------------------------------------------------
    def _install_api(self):
        g = self.lua.globals()

        def record(name):
            def fn(*args):
                self.calls.append(Call(name, args))
                return 0
            g[name] = fn
            return fn

        # Dialogs. `ShowSelDlg` answers 1 (the first option) so the script keeps
        # going; a real click would answer with the player's choice.
        for name in ("ShowOkDlg", "ShowMsgDlg", "ShowScnDlg", "ShowCommonDlg",
                     "BalloonChat", "Chat", "SysMsgToComm", "SendMsg"):
            record(name)
        # Windows. Recorded here and opened by the zone AFTER the script's branch
        # has run: a menu NPC's choice decides WHICH window, so sending it from
        # inside Lua would open the wrong one (or open it before the answer).
        g["ShowNPCTradeDlg"] = lambda *a: self.trade_request("normal", a[2] if len(a) > 2 else "")
        g["ShowLobby"] = self.show_lobby
        g["ShowCustomNPCTradeDlg"] = lambda *a: self.trade_request("custom", a[2] if len(a) > 2 else "")
        g["ShowWarehouseDlg"] = lambda *a: self.trade_request("warehouse", "")
        g["ShowSelDlg"] = record("ShowSelDlg")
        g["ShowScnSelDlg"] = record("ShowScnSelDlg")

        # Objects. A script asks for the commander and then reads properties off
        # it, so these must hand back a bag, not 0.
        g["GetCommander"] = lambda pc: pc
        g["GetPC"] = lambda obj: obj
        g["GetLeader"] = lambda obj: obj
        g["GetZoneObject"] = lambda *a: PropBag("zone", ID=self.zone_id)
        g["GetCurrentZoneInstID"] = lambda *a: self.zone_id
        g["GetIESProp"] = lambda obj, prop: obj[prop] if hasattr(obj, "__getitem__") else 0

        def set_ies(obj, prop, value):
            if hasattr(obj, "__setitem__"):
                obj[prop] = value
            return 0
        g["SetIESProp"] = set_ies
        g["ChangeNPCPropertyTx"] = lambda *a: 0

        # Transactions. A script changes the world through these, and all of it
        # must land or none of it, so TxEnd is ONE database transaction.
        g["TxBegin"] = lambda pc=None: self.tx_begin()
        g["TxEnd"] = lambda tx: self.tx_end(tx)
        g["TxGetLastResultCode"] = lambda tx: 0
        g["TxSetCommProp"] = self.tx_set_prop
        g["TxGiveItem"] = lambda tx, name, count=1: self.tx_item(tx, name, count, "give")
        g["TxTakeItem"] = lambda tx, name, count=1: self.tx_item(tx, name, count, "take")

        # Answers that scripts branch on.
        g["IsTestServer"] = lambda *a: "NO"
        g["IsInventoryItem"] = lambda *a: "NO"
        g["IsValidClassID"] = lambda *a: 1
        # Item counts are REAL reads. A stub answering 0 makes every
        # affordability check fail ("not enough money") whatever the player holds,
        # and every "do you have it" branch take the wrong side.
        g["GetItemCountInInventory"] = self.item_count
        g["GetInvItemCount"] = lambda pc, name: self.item_count(pc, name)
        g["GetInventoryItemCount"] = self.inventory_used
        g["GetCommanderAccID"] = lambda *a: getattr(self, "account_id", None) or 0
        g["GetServiceNation"] = lambda *a: SERVICE_NATION
        # Property reads. `GetKillCount` is the commander's IES property read
        # (CommanderFunc.cpp:339), not the quest-stage counter - 479 click
        # scripts branch on it, and a 0 stub made every "is this objective
        # done?" guard false.
        g["GetKillCount"] = self.ies_prop
        g["GetZoneMapClassID"] = self.zone_map_class_id
        g["GetZoneMapName"] = self.zone_map_name
        # Data tables: the client's own IES rows, read by class id or by name.
        # `GetDataTableStringValue('Item', id, 'ClassName')` alone is 452 call
        # sites, so a 0 stub decided a lot of "which item/zone/quest is this"
        # branches by accident.
        g["GetDataTableNumberValue"] = self.data_table_number
        g["GetDataTableStringValue"] = self.data_table_string
        g["GetDataTableNumberValueByClassName"] = self.data_table_number_by_name
        g["GetDataTableStringValueByClassName"] = self.data_table_string_by_name
        # Moving an object is a WORLD action, queued for the zone like a commit.
        g["SetPos"] = self.set_pos
        g["GetItemName"] = self.item_name
        # An NPC speaking is also a world action: ZC_CHAT with UseBalloon, NOT
        # the balloon packet (that one belongs to the `BalloonChat` builtin).
        g["ChatNPC"] = self.chat_npc
        g["GetBit"] = self.get_bit
        # The commander's TEAM, from the same rows the barrack/zone already use.
        # A solo player is count 1 and two "None" job slots, which is what the
        # original answers when only the leader is in the world.
        g["GetMccCount"] = self.mcc_count
        g["GetMccJob"] = self.mcc_job
        g["GetSquadId"] = self.squad_id
        g["GetAlliance"] = self.alliance
        g["GetServerGroupID"] = self.server_group_id
        # `GetPos` pushes THREE values: x, y, height (FieldCharFunc.cpp:972).
        g["GetPos"] = self._obj_xyz
        # `GetFatigue` is the play-time PENALTY level (CommanderFunc.cpp:1581 ->
        # CCheckFatigue::GetPenalty), not a number of hours. A fresh account is
        # NONE=0 (geCheckFatigue.h); we do not track play/rest time yet, so that
        # is also the honest answer for every account here.
        g["GetFatigue"] = lambda *a: 0
        g["sleep"] = lambda *a: 0
        g["IMCRandom"] = lambda a, b: a
        # `ShowSelDlg` must not answer itself: the script has to WAIT for the
        # player. It yields the question out of a coroutine, and the value the
        # coroutine is resumed with becomes the return of ShowSelDlg - so
        # `var pick = ShowSelDlg(...)` continues down the chosen branch.
        self.lua.execute("""
-- Suspended scripts live in Lua; Python only ever sees an integer id.
__cos, __co_next = {}, 0
function __co_new(fn)
  __co_next = __co_next + 1
  __cos[__co_next] = coroutine.create(fn)
  return __co_next
end
function __co_step(id, ...)
  local co = __cos[id]
  local ok, val = coroutine.resume(co, ...)
  local status = coroutine.status(co)
  if status == "dead" then __cos[id] = nil end
  return {ok = ok, val = val, status = status}
end

function ShowSelDlg(pc, prompt, ...)
  local opts = {...}
  return coroutine.yield({prompt = prompt, options = opts})
end

-- `MsgBox` is the OK box WITHOUT fade-in (PlayerCharFunc.cpp:1073) and
-- `MsgSelBox` the select box without fade-in (:1132). Both WAIT for the player
-- in the original (they end in `Suspend()`), so both yield here: a no-op would
-- let the script run down a branch nobody chose. MsgSelBox returns the pick.
function MsgBox(pc, prompt)
  __record_dialog('MsgBox', prompt)
  coroutine.yield({prompt = prompt, options = {}, fade = 0})
end

function MsgSelBox(pc, prompt, ...)
  __record_dialog('MsgSelBox', prompt)
  local opts = {...}
  return coroutine.yield({prompt = prompt, options = opts, fade = 0})
end
""")
        g["IMCFormat"] = self._imc_format
        g["GetCurDateNumber"] = lambda *a: 0

    @staticmethod
    def _imc_format(fmt, *args):
        """`IMCFormat("<$>10137067</>", 50000)` - resolve the token, then fill it.

        The token must become the client's TEXT before `%d` can be substituted;
        formatting the token itself leaves every argument unplaced and the player
        reads a literal `%d` (seen live 2026-10-23).
        """
        import re as _re
        try:
            import ge_locale
            fmt = ge_locale.resolve(str(fmt))
        except Exception:
            fmt = str(fmt)
        if not args:
            return fmt
        fmt = _re.sub(r"%(?:I64|I32|l|h)?([diouxXfeEgGsc])", r"%\1", fmt)
        values = [int(v) if isinstance(v, float) and v.is_integer() else v for v in args]
        try:
            return fmt % tuple(values)
        except (TypeError, ValueError):
            return fmt

    def item_count(self, pc=None, name=""):
        """How many of `name` this commander holds. A real read, never a stub."""
        commander = getattr(self._local, "commander", None) or self.commander_id
        if not commander:
            return 0
        try:
            import ge_db
            import ge_item_index
            cls = name if isinstance(name, int) else ge_item_index.class_id(str(name))
            if cls is None:
                return 0
            return sum(int(row.get("count") or 0)
                       for row in ge_db.get_inventory_full(commander)
                       if row.get("type") == cls)
        except Exception as exc:
            print("script_host: could not count %r: %s" % (name, exc))
            return 0

    def inventory_used(self, *a):
        """Slots in use - `inventory->GetCount()` in the original.

        Cached per click: it is one DB read, and a script may ask repeatedly.
        """
        commander = getattr(self._local, "commander", None) or self.commander_id
        if not commander:
            return 0
        if self._inv_used is None:
            try:
                import ge_db
                self._inv_used = len(ge_db.get_inventory_full(commander))
            except Exception as exc:
                print("script_host: could not count used slots: %s" % exc)
                return 0
        return self._inv_used

    def ies_prop(self, obj, name):
        """`GetKillCount(obj, name)` / `GetIESProp(obj, name)`: ONE property read.

        Source: `CommanderFunc.cpp:339` - `GetKillCount` is NOT the quest stage
        counter (`CQuestObject::GetKillCount`); it reads the commander's IES
        object and pushes the value, or 0 when the name is absent. So the
        answer comes from `COMMANDER_PROPERTY`, through the bag, which also
        sees the running transaction's queued writes.
        """
        if name is None:
            return 0
        try:
            value = obj[name]
        except Exception:
            return 0
        return value if value is not None else 0

    def _obj_xyz(self, obj):
        """`GetPos(obj)` -> x, y, height (`FieldCharFunc.cpp:972`, three values).

        Our objects are bags, not field objects with a world; the position is
        put on the bag when the click starts (the player's saved spot, or the
        NPC's spawn row). An object with no position is 0,0,0 - which is what
        the original pushes for a null object.
        """
        d = getattr(obj, "_d", None)
        if not isinstance(d, dict):
            return 0.0, 0.0, 0.0
        try:
            return (float(d.get("X") or 0), float(d.get("Y") or 0),
                    float(d.get("Z") or 0))
        except (TypeError, ValueError):
            return 0.0, 0.0, 0.0

    def zone_map_class_id(self, zone_inst=None):
        """`GetZoneMapClassID(zoneInstID)` -> the map's class id
        (`SystemFunc.cpp:3834`). In this port one zone id IS the map class id
        (`datatable_map` ClassID), so the only wrong input is an unknown zone.
        """
        try:
            zone = int(float(zone_inst)) if zone_inst not in (None, "") else self.zone_id
        except (TypeError, ValueError):
            return 0
        try:
            import npc_data
            return zone if zone in npc_data.load_map_table() else 0
        except Exception:
            return zone

    def zone_map_name(self, zone_inst=None):
        """`GetZoneMapName(zoneInstID)` -> the map's class NAME
        (`SystemFunc.cpp:3810` pushes `pMapProp->name`; the map table's `Name`
        column is a locale token, so scripts compare the stable class name).
        """
        try:
            zone = int(float(zone_inst)) if zone_inst not in (None, "") else self.zone_id
        except (TypeError, ValueError):
            return "None"
        try:
            import npc_data
            entry = npc_data.load_map_table().get(zone)
            return entry[0] if entry else "None"
        except Exception:
            return "None"

    def _data_table(self, idspace):
        """`{classID: {property: text}}` for one IES idspace, decoded on demand.

        `GetDataTableStringValue('Item', 7705, 'ClassName')` reads the client's
        own table. **The idspace is not the file name**: `datatable_missionenter`
        is `Mission_Rank`, and 140 of 316 tables differ, so the lookup goes
        through `npc_data.tables_for_idspace` (the .ies header's own name).
        A missing table is not an error: the original pushes Null for an
        unknown class, and so do we.
        """
        key = str(idspace or "")
        if key in self._dt_cache:
            return self._dt_cache[key]
        rows = None
        try:
            import npc_data
            rows = {}
            for name in npc_data.tables_for_idspace(key):
                try:
                    root = npc_data._decode(name)
                except Exception:
                    continue
                for cls in root.iter("Class"):
                    try:
                        row = rows.setdefault(int(cls.get("ClassID") or 0), {})
                    except (TypeError, ValueError):
                        continue
                    for prop, value in cls.attrib.items():
                        row.setdefault(prop, value)
        except Exception:
            rows = None
        self._dt_cache[key] = rows
        return rows

    def data_table_row(self, idspace, class_id, by_name=False):
        """The IES class row, or None. `by_name` picks ClassName instead of id."""
        rows = self._data_table(idspace)
        if not rows or class_id is None:
            return None
        if by_name:
            want = str(class_id)
            for row in rows.values():
                if row.get("ClassName") == want:
                    return row
            return None
        try:
            return rows.get(int(float(class_id)))
        except (TypeError, ValueError):
            return None

    @staticmethod
    def _as_number(text):
        """The IES numeric view of a table cell.

        The original asks the class for a FLOAT, which fails - leaving 0 - on a
        text cell, so a name never reads as a number. A bool cell does convert
        (true = 1); the XML spells it TRUE/FALSE.
        """
        if text is None:
            return 0
        t = str(text).strip()
        if t.upper() == "TRUE":
            return 1
        if t.upper() == "FALSE":
            return 0
        try:
            return int(t, 0)
        except (TypeError, ValueError):
            pass
        try:
            return float(t)
        except (TypeError, ValueError):
            return 0

    def data_table_number(self, idspace, class_id, prop):
        """`GetDataTableNumberValue` (SystemFunc.cpp:3860).

        The original pushes `Null` for an unknown class. In THIS dialect a null
        compares false instead of raising, but our host is Lua, where `nil > 0`
        is an ERROR - and 83 script runs died exactly there. Answer 0: it keeps
        every guard false, which is what the null meant, and it cannot kill the
        script. A text cell reads as 0 too (the original's float conversion
        fails and leaves 0).
        """
        row = self.data_table_row(idspace, class_id)
        if row is None:
            return 0
        return self._as_number(row.get(str(prop)))

    def data_table_string(self, idspace, class_id, prop):
        """`GetDataTableStringValue` (SystemFunc.cpp:3891). Missing class = nil.

        `Item` is special: one idspace is SPLIT across 8 files
        (`datatable_item_weapon`, `_quest`, ...), and `datatable_item` itself
        holds 2 rows, so a single-file read finds nothing. `ge_item_index`
        already merges them for the item layer (and caches the result), so an
        item ClassName comes from there.
        """
        if str(idspace).lower() == "item" and str(prop) == "ClassName":
            name = self._item_name(class_id)
            if name:
                return name
        row = self.data_table_row(idspace, class_id)
        if row is None:
            return "None"
        value = row.get(str(prop))
        return "None" if value is None else str(value)

    @staticmethod
    def _item_name(class_id):
        """Class id -> item ClassName, through the shared item index."""
        try:
            import ge_item_index
            return ge_item_index.name_of(int(float(class_id)))
        except Exception:
            return None

    def item_name(self, item):
        """`GetItemName(itemName)` -> the class name (SystemFunc.cpp:2311).

        The original looks the name up in `geItemTable` and answers StrID::None
        when it is not there, so an unknown name is "None", not nil.
        """
        if item is None:
            return "None"
        try:
            import ge_item_index
            if isinstance(item, (int, float)):
                return ge_item_index.name_of(int(item)) or "None"
            cls = ge_item_index.class_id(str(item))
            if cls is None:
                return "None"
            return ge_item_index.name_of(cls) or "None"
        except Exception:
            return "None"

    def data_table_number_by_name(self, idspace, class_name, prop):
        row = self.data_table_row(idspace, class_name, by_name=True)
        if row is None:
            return 0
        return self._as_number(row.get(str(prop)))

    def data_table_string_by_name(self, idspace, class_name, prop):
        row = self.data_table_row(idspace, class_name, by_name=True)
        if row is None:
            return "None"
        value = row.get(str(prop))
        return "None" if value is None else str(value)

    def set_pos(self, obj, x, y, z):
        """`SetPos(obj, x, y, z)` - put an object somewhere (PlayerCharFunc.cpp:977).

        The original sends `OM_SET_POS` for a player and `OM_WARP` for a monster;
        either way the move happens on the WORLD, not inside the script. So it is
        queued here and sent by the zone after the branch has run, exactly like
        item and property writes. The bag's own X/Y/Z move at once, so a script
        that Sets then Gets the position agrees with itself.
        """
        try:
            pos = (int(float(x)), int(float(y)), int(float(z)))
        except (TypeError, ValueError):
            return 0
        d = getattr(obj, "_d", None)
        if isinstance(d, dict):
            d["X"], d["Y"], d["Z"] = pos
        is_player = obj is getattr(self, "_pc", None) or getattr(
            obj, "_kind", "") == "pc"
        self.setpos_events.append((is_player, pos))
        self.calls.append(Call("SetPos", pos))
        return 0

    def chat_npc(self, zone_inst, obj, text):
        """`ChatNPC(zoneInstID, npc, text)` - an NPC says a line (World.cpp:2650).

        The original broadcasts `OM_CHAT`, which the field char turns into the
        ordinary `ZC_CHAT` with `UseBalloon = true` (`FieldChar.cpp:1010`). It is
        NOT `ZC_BALLOON_CHAT`: that packet belongs to the `BalloonChat` builtin.
        Queued like the other world actions, so the zone sends it once the
        branch has finished.
        """
        d = getattr(obj, "_d", None)
        handle = d.get("Handle") if isinstance(d, dict) else 0
        try:
            handle = int(handle or 0)
        except (TypeError, ValueError):
            handle = 0
        text = "" if text is None else str(text)
        self.chat_events.append((handle, text))
        self.calls.append(Call("ChatNPC", (handle, text)))
        return 0

    def get_bit(self, flag, bit):
        """`GetBit(flag, bit)` (`SystemFunc.cpp:2961` -> `imc::BitGet`)."""
        try:
            return (int(flag) >> int(bit)) & 1
        except (TypeError, ValueError):
            return 0

    def _team_slots(self):
        """The three team positions as CHARACTER SLOTS (255 = empty).

        The barrack writes `out\\zone_team_<commander>.json` on "Continue" and
        the zone reads it; `zone_team.json` is the shared fallback. The order is
        the original's `GetPCByIndex(0..2)`, which `GetMccJob` walks.
        """
        commander = self._commander()
        if commander in self._team_cache:
            return self._team_cache[commander]
        slots = [0, 255, 255]
        names = []
        if commander:
            names.append("zone_team_%s.json" % commander)
        names.append("zone_team.json")
        for name in names:
            try:
                with open(os.path.join(OUT, name), encoding="utf-8") as fh:
                    slots = json.load(fh).get("slots") or slots
                break
            except Exception:
                continue
        try:
            slots = [int(s) for s in slots][:3]
        except (TypeError, ValueError):
            slots = [0, 255, 255]
        self._team_cache[commander] = slots
        return slots

    def _commander(self):
        return getattr(self._local, "commander", None) or self.commander_id

    def _characters(self):
        """Every character of the commander, as stored rows (cached per host)."""
        commander = self._commander()
        if commander not in self._char_cache:
            try:
                import ge_db
                self._char_cache[commander] = ge_db.get_characters(commander) or []
            except Exception as exc:
                print("script_host: could not load characters: %s" % exc)
                self._char_cache[commander] = []
        return self._char_cache[commander]

    def _team_member(self, pos):
        """The character in team position `pos`, or None when it is empty."""
        slots = self._team_slots()
        if pos >= len(slots):
            return None
        slot = slots[pos]
        if slot is None or slot < 0 or slot == 255:
            return None
        for row in self._characters():
            try:
                if int(row.get("slot") or 0) == slot:
                    return row
            except (TypeError, ValueError):
                continue
        return None

    def mcc_count(self, *a):
        """`GetMccCount(pc)` -> `commander->GetPCCount()` (PlayerCharFunc.cpp:1757)."""
        return sum(1 for i in range(3) if self._team_member(i) is not None)

    def _job_name(self, job_id):
        """Job class id -> its name. This build's Job table has no `TypeName`
        column, so the CLASS NAME ('Fighter', 'Wizard') is the job's name - which
        is also what the client shows."""
        try:
            key = int(float(job_id))
        except (TypeError, ValueError):
            return None
        row = (self._data_table("Job") or {}).get(key) or {}
        return row.get("ClassName")

    def mcc_job(self, *a):
        """`GetMccJob(pc)` -> THREE job names, one per team position
        (PlayerCharFunc.cpp:1299). An empty position is "None"."""
        names = []
        for pos in range(3):
            member = self._team_member(pos)
            job_id = member.get("job") if member else None
            names.append(self._job_name(job_id) or "None")
        return tuple(names)

    def squad_id(self, *a):
        """`GetSquadId(commander)` -> the squad id (CommanderFunc.cpp:230).

        There is no squad system here, so a commander with no squad is 0, which
        is the member's own initial value; -1 means "no commander", exactly what
        the original answers in that case.
        """
        return 0 if self._commander() else -1

    @staticmethod
    def alliance(*a):
        """`GetAlliance(obj)` -> "None" without a mission (MissionFunc.cpp:1043).

        The original only resolves a name through `CMission::GetAllianceName`,
        and with no mission it falls through to StrID "None".
        """
        return "None"

    @staticmethod
    def server_group_id(*a):
        """`GetServerGroupID()` -> the server's group id (SystemFunc.cpp:3018).

        One zone process, one group: 0.
        """
        return 0

    def _tick(self):
        """Loop-body counter. A runaway `while(1)` raises instead of hanging.

        The real server has the same protection ("Too long script execution
        time"); without it one bad script stops the zone thread.
        """
        self.ticks += 1
        if self.ticks > self.tick_budget:
            raise RuntimeError("script ran too long (%d ticks)" % self.ticks)

    # -- transactions ---------------------------------------------------
    def set_commander(self, commander_id):
        """Whose properties a transaction writes. Set before running a click.

        Per THREAD, not per host: one Lua runtime serves every player, and two
        clicks can be in flight at once.
        """
        self._local.commander = int(commander_id) if commander_id else None

    def _properties(self):
        """name -> MASTER_PROPERTY.IDX. That table is the dictionary; the DB's
        `property` table is empty and the XML column order is NOT the id."""
        if self._prop_map is None:
            import ge_db
            self._prop_map = {name: int(idx) for idx, name in
                              ge_db._run("SELECT IDX, PROPERTY FROM MASTER_PROPERTY")}
        return self._prop_map

    @staticmethod
    def _sql_value(value):
        if isinstance(value, float) and value.is_integer():
            return str(int(value))
        return str(value)

    def _tx_op(self, tx):
        """The op bag for a transaction, created on first use with every key."""
        return self._tx.setdefault(tx, {"ops": [], "failed": False, "commander": None})

    def trade_request(self, kind, group):
        """A window the script asked for. Opened later, by the zone.

        Not sent from inside Lua: the branch decides WHICH window, and the branch
        is only known once the player has answered. Same shape as `item_events`.
        """
        self.trade_events.append((kind, str(group or "")))
        self.calls.append(Call("ShowNPCTradeDlg" if kind == "normal"
                               else "ShowCustomNPCTradeDlg" if kind == "custom"
                               else "ShowWarehouseDlg", (kind, group)))
        return 0

    def show_lobby(self, pc, lobby_type, layer=None, create_ok=None):
        """`ShowLobby(pc, type [, layer [, createOk]])` - the mission lobby window.

        `MissionFunc.cpp:1396` sends no packet itself: it posts `OM_SHOW_LOBBY`
        to the commander, whose handler stores the type/layer/create flag and
        asks the world for the room list (`CCommander::ShowLobby`,
        `Commander.cpp:661`). The CLICK decides the type, so the reply is sent by
        the zone after the branch has run - the same rule as the shop windows.
        """
        kind = str(lobby_type or "")
        try:
            lyr = int(float(layer)) if layer is not None else 0
        except (TypeError, ValueError):
            lyr = 0
        create = 1
        if create_ok is not None:
            try:
                create = 1 if int(float(create_ok)) > 0 else 0
            except (TypeError, ValueError):
                create = 1
        self.lobby_events.append((kind, lyr, create))
        self.calls.append(Call("ShowLobby", (kind, lyr, create)))
        return 0

    def pending_prop(self, commander_id, name):
        """A property this commander's OPEN transaction queued but not committed.

        A script that sets a property and then reads it back must see its own
        pending write, exactly like the shipped IES object does. `None` means
        "nothing queued" - 0 is a real value a guard can compare.
        """
        want = int(commander_id) if commander_id else None
        for bag in self._tx.values():
            if bag.get("commander") != want:
                continue
            for op in bag.get("ops", ()):
                if op[0] == "prop" and str(op[1]) == str(name):
                    return op[2]
        return None

    def tx_begin(self, pc=None):
        self._tx_next += 1
        # remember whose transaction this is AT BEGIN, so a later resume (from a
        # different moment, maybe a different thread) cannot write to the wrong
        # character
        self._tx[self._tx_next] = {"ops": [], "failed": False,
                                   "commander": getattr(self._local, "commander", None)
                                   or self.commander_id}
        self.calls.append(Call("TxBegin", (pc,)))
        return self._tx_next

    def tx_set_prop(self, tx, name, value):
        self._tx_op(tx)["ops"].append(("prop", str(name), value))
        self.calls.append(Call("TxSetCommProp", (tx, name, value)))
        return 0

    def tx_item(self, tx, name, count, kind):
        """Queue a give/take, and answer 1 or 0 exactly as the shipped builtin does.

        The original decides AT CALL TIME, not at commit: an item name that the
        item table does not know, or a stack the commander does not have, makes
        the builtin answer 0 and marks the whole transaction failed
        (`TransactionFunc.cpp:294` and `TxScript.cpp:1170`). Getting this wrong
        is what makes a script take the wrong branch.
        """
        bag = self._tx_op(tx)
        self.calls.append(Call("TxGiveItem" if kind == "give" else "TxTakeItem",
                               (tx, name, count)))
        count = int(count or 0)
        cls = self._class_id(name)
        if cls is None:
            print("script_host: %s item %r is not in the item table - tx fails"
                  % (kind, name))
            bag["failed"] = True
            return 0
        if count < 0 or (kind == "give" and count == 0):
            print("script_host: %s of %r with count %d - tx fails" % (kind, name, count))
            bag["failed"] = True
            return 0
        if kind == "take":
            have = self._stack_for(bag["commander"], cls)
            if have is None or have[1] < count:
                print("script_host: take %d of %r but %s - tx fails"
                      % (count, name, "none held" if have is None
                         else "only %d held" % have[1]))
                bag["failed"] = True
                return 0
        else:
            full = self._give_blocked(bag["commander"], cls, count)
            if full:
                print("script_host: give %r - %s - tx fails" % (name, full))
                bag["failed"] = True
                return 0
        bag["ops"].append((kind, cls, count, str(name)))
        return 1

    @staticmethod
    def _give_blocked(commander, class_id, count):
        """Why this give is a FULL INVENTORY, or None - mirrors the shipped `Add`.

        A stack that would pass `MaxStack` makes the original answer 0 and mark
        the transaction failed; it never opens a second stack.
        """
        import ge_item_index
        mergeable, cap = ge_item_index.stack_rule(class_id)
        if not mergeable:
            return None
        have = ScriptHost._stack_for(commander, class_id)
        if have is not None and have[1] + count > cap:
            return ("stack %d holds %d, adding %d exceeds the client limit %d"
                    % (have[0], have[1], count, cap))
        return None

    def tx_end(self, tx):
        """Apply everything the script queued, in one transaction.

        Returns 1 on success and 0 on failure, which is what the scripts branch
        on (`if (ret == 0) { return; }`).

        Writes through the ORIGINAL server's own procs, never hand-written SQL:
        `A_SET_ITEMCREATE_GMCMD_1` for a give (it inserts ITEM and links STORAGE)
        and `A_SET_ITEMCOUNT_2` for a take (it decrements, and deletes the row at
        zero; STORAGE goes with it through FK_STORAGE_ITEM's cascade).
        """
        pending = self._tx.pop(tx, None)
        if pending is None:
            print("script_host: TxEnd on unknown transaction %r" % (tx,))
            return 0
        ops, commander = pending["ops"], pending["commander"]
        self.calls.append(Call("TxEnd", (tx,)))
        if pending["failed"]:
            print("script_host: TxEnd refused - an item op failed earlier in this "
                  "transaction, so nothing is written")
            return 0
        if not ops:
            return 1
        if not commander:
            print("script_host: TxEnd refused - no commander id set for this session")
            return 0
        try:
            import ge_db
            index = self._properties()
            events = []
            props = []
            with ge_db.transaction() as cur:
                # in script order: a take after a give must see the given row
                for op in ops:
                    kind = op[0]
                    if kind == "prop":
                        self._apply_prop(cur, commander, index, op[1], op[2])
                        props.append((op[1], op[2]))
                    elif kind == "give":
                        item_id, total = self._apply_give(cur, commander, op[1], op[2])
                        events.append(("add", op[1], total, item_id))
                    else:
                        item_id, total = self._apply_take(cur, commander, op[1], op[2])
                        events.append(("remove", op[1], total, item_id))
            # NOW, not inside the block: a rollback means nothing was written, and
            # telling the client about it would leave the bag showing a ghost.
            self.item_events.extend(events)
            self.prop_events.extend(name for name, _value in props)
            # A script may read a property back AFTER the commit - Leonardo's
            # "increased up to %d" line does. The original updates its IES object
            # in memory, so mirror that on our stand-in bag or the player is told
            # the old number.
            bag = getattr(self, "_pc", None)
            if bag is not None:
                for name, value in props:
                    try:
                        bag[name] = value
                    except Exception:
                        pass
            return 1
        except Exception as exc:
            print("script_host: TxEnd failed: %s" % exc)
            return 0

    @staticmethod
    def _apply_prop(cur, commander, index, name, value):
        idx = index.get(name)
        if idx is None:
            print("script_host: unknown property %r - TxEnd failed" % name)
            raise ValueError(name)
        cur.execute(
            "UPDATE COMMANDER_PROPERTY SET VALUE=?, UPDATE_DATE=GETDATE() "
            "WHERE COMMANDER_ID=? AND SECTION=1 AND PROPERTY_IDX=?",
            ScriptHost._sql_value(value), commander, idx)
        if cur.rowcount == 0:
            cur.execute(
                "INSERT INTO COMMANDER_PROPERTY "
                "(COMMANDER_ID, SECTION, PROPERTY_IDX, VALUE, UPDATE_DATE) "
                "VALUES (?, 1, ?, ?, GETDATE())",
                commander, idx, ScriptHost._sql_value(value))

    @staticmethod
    def _apply_give(cur, commander, class_id, count):
        """Give an item the way the shipped inventory does: MERGE when it can.

        The real `CInventoryControl::Add` joins the item of that class the player
        already holds and returns the EXISTING guid with the new total, so the
        proc's `COUNT = COUNT + n` branch fires. Minting a fresh id every time
        (what this did at first) yields one stack per reward - something the game
        never does for a stackable item.
        """
        item_id = ScriptHost._merge_into_id(cur, commander, class_id, count)
        if item_id is None:
            import ge_guid
            item_id = ge_guid.next_item_id()
        cur.execute("EXEC A_SET_ITEMCREATE_GMCMD_1 ?, ?, ?, ?, NULL",
                    commander, item_id, class_id, count)
        # the packet wants the NEW TOTAL, not the delta: ZC_ADDITEM carries the
        # count the client should now show
        cur.execute("SELECT [COUNT] FROM ITEM WHERE ITEM_ID = ?", item_id)
        row = cur.fetchone()
        return item_id, (int(row[0]) if row else int(count))

    @staticmethod
    def _merge_into_id(cur, commander, class_id, count):
        """The ITEM_ID this give should join, or None to make a new stack.

        A merge that would pass `MaxStack` is a FULL INVENTORY in the shipped
        server (`CInventoryControl::Add` returns ADD_FULL_CHARGE, `CreateItem`
        sets m_doFail), so it fails the whole transaction. It must NOT fall back
        to a second stack: two stacks of one item is a state the client cannot
        hold sensibly, and `MaxStack` is the client's own per-slot limit.
        """
        import ge_item_index
        mergeable, cap = ge_item_index.stack_rule(class_id)
        if not mergeable:
            return None
        cur.execute(
            "SELECT s.ITEM_ID, i.COUNT FROM STORAGE s "
            "JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
            "WHERE s.COMMANDER_ID = ? AND i.CLASS_ID = ? AND s.TRASH = 0 "
            "ORDER BY s.INSERT_DATE", commander, class_id)
        for item_id, have in cur.fetchall():
            total = int(have) + int(count)
            if total <= cap:
                return int(item_id)
            raise ValueError(
                "full inventory: class %d stack %s holds %d, adding %d exceeds "
                "the client limit %d" % (class_id, item_id, int(have), int(count), cap))
        return None
        return item_id

    @staticmethod
    def _apply_take(cur, commander, class_id, count):
        cur.execute(
            "SELECT s.ITEM_ID, i.COUNT FROM STORAGE s "
            "JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
            "WHERE s.COMMANDER_ID = ? AND i.CLASS_ID = ? AND s.TRASH = 0 "
            "ORDER BY s.INSERT_DATE", commander, class_id)
        row = cur.fetchone()
        if row is None:
            raise ValueError("no stack of class %d" % class_id)
        item_id, have = int(row[0]), int(row[1])
        if have < count:
            raise ValueError("stack %d holds %d, needed %d" % (item_id, have, count))
        cur.execute("SET NOCOUNT ON; DECLARE @err int; "
                    "EXEC A_SET_ITEMCOUNT_2 ?, ?, ?, @err OUTPUT; SELECT @err",
                    commander, item_id, count)
        result = cur.fetchone()
        if result is None or int(result[0]) != 0:
            raise ValueError("A_SET_ITEMCOUNT_2 returned %r" % (result,))
        # what the client should now show; no row at all means the stack is gone
        cur.execute("SELECT [COUNT] FROM ITEM WHERE ITEM_ID = ?", item_id)
        row = cur.fetchone()
        return item_id, (int(row[0]) if row else 0)

    @staticmethod
    def _class_id(name):
        import ge_item_index
        return ge_item_index.class_id(name)

    @staticmethod
    def _stack_for(commander, class_id):
        """(ITEM_ID, count) of the commander's first stack of that class, or None."""
        if not commander:
            return None
        import ge_db
        rows = ge_db._run(
            "SELECT s.ITEM_ID, i.COUNT FROM STORAGE s "
            "JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
            "WHERE s.COMMANDER_ID = %d AND i.CLASS_ID = %d AND s.TRASH = 0 "
            "ORDER BY s.INSERT_DATE" % (int(commander), int(class_id)))
        return (int(rows[0][0]), int(rows[0][1])) if rows else None


    def _missing(self, name, *args):
        """Any builtin we have not implemented: record it, answer 0."""
        self.calls.append(Call(name, args))
        return 0

    # -- loading ---------------------------------------------------------
    def index(self):
        """`func name` -> file, built once. The server reads it at startup too."""
        if self._index is None:
            self._index = {}
            for dirpath, _dirs, names in os.walk(self.root):
                for name in names:
                    if not name.endswith(".scp"):
                        continue
                    path = os.path.join(dirpath, name)
                    try:
                        with open(path, encoding="latin-1") as fh:
                            text = fh.read()
                    except OSError:
                        continue
                    for fn in FUNC_RE.findall(text):
                        self._index.setdefault(fn, path)
        return self._index

    def file_for(self, func_name):
        return self.index().get(func_name)

    # Libraries: plain `func` collections that the shipped scripts call as if
    # they were engine builtins. Without them every call to a helper fell into
    # the missing-stub and answered 0 - `EXP_QUEST_ADMIN` (372 click scripts) and
    # `ShowNormalQuest` (79) are the two that mattered most.
    LIBRARIES = ("npc/utility/func_define.scp", "npc/utility/util_define.scp",
                 "npc/utility/conv_define.scp")

    def _load_libraries(self):
        for rel in self.LIBRARIES:
            path = os.path.join(self.root, *rel.split("/"))
            if not os.path.exists(path):
                continue
            try:
                self.load_file(path)
            except Exception as exc:
                print("script_host: could not load %s: %s" % (rel, exc))

    def _emulate_varargs(self, code):
        """Lua 5.1 gave a vararg function a local `arg` TABLE; Lua 5.4 does not.

        The shipped scripts use it (`unpack(arg)` in func_define's ShowScnSelDlg),
        and without it `arg` fell through to the missing-stub, which returns a
        FUNCTION - so `unpack(arg)` raised "table expected, got function".
        Declaring `local arg = {...}` at the top of every vararg function is exact.
        """
        return re.sub(r"(function\s+[\w.:]*\s*\([^)]*\.\.\.[^)]*\))",
                      r"\1 local arg = {...};", code)


    def load_file(self, path):
        if path in self.loaded:
            return
        code = translate_file(path, guard=True)
        code = self._emulate_varargs(code)
        try:
            self.lua.execute(code, name=os.path.basename(path))
        except Exception as exc:
            # Modern Lua makes the `for` control variable read-only. The shipped
            # scripts assign to it, so a few files refuse to compile. Shadow it
            # with a local copy - same semantics (Lua 5.1 also only changed the
            # copy) - but ONLY for the files that fail, so the other 4,231 keep
            # the translation they were measured with.
            if "const variable" not in str(exc):
                raise
            fixed = re.sub(r"\bfor\s+([A-Za-z_]\w*)\s*=\s*(.+?)\s+do\b",
                           r"for \1 = \2 do local \1 = \1 ", code)
            self.lua.execute(fixed, name=os.path.basename(path))
        self.loaded.add(path)

    def function(self, func_name):
        """Load the file that defines it, then return the Lua function."""
        fn = self._rawget(func_name)
        if fn is not None:
            return fn
        path = self.file_for(func_name)
        if not path:
            raise KeyError("no script defines %r" % func_name)
        self.load_file(path)
        fn = self._rawget(func_name)
        if fn is None:
            raise KeyError("%r not defined after loading %s" % (func_name, path))
        return fn

    # -- running ---------------------------------------------------------
    def run(self, func_name, *args):
        """Call a script function. Returns its Lua return values (a tuple)."""
        self.ticks = 0
        self._tx = {}
        self._tx_next = 0
        # item changes a COMMITTED transaction made, for the caller to send to the
        # client: (kind, class_id, count, item_id). Only filled after a commit - a
        # rolled-back transaction must leave the client's bag untouched.
        self.item_events = []
        self._prop_map = None
        return self.function(func_name)(*args)

    def _lua_table_list(self, tbl):
        return [tbl[i] for i in range(1, len(tbl) + 1)]

    def _step(self, co, args, before):
        """Advance the coroutine once: done, or holding a question."""
        step = self._rawget("__co_step")(co, *args)
        ok, val, status = step["ok"], step["val"], step["status"]
        if not ok:
            return {"ok": False, "done": True, "error": str(val),
                    "question": None, "dialog": None,
                    "calls": self.calls[before:]}
        if status == "dead":
            res = {"ok": True, "done": True, "error": None, "question": None,
                   "calls": self.calls[before:]}
            res["dialog"] = self._first_dialog(res["calls"])
            return res
        question = {"prompt": val["prompt"],
                    "options": self._lua_table_list(val["options"])}
        fade = val["fade"]
        question["fade"] = 1 if fade is None else int(fade)
        return {"ok": True, "done": False, "error": None, "question": question,
                "dialog": None, "co": co, "calls": self.calls[before:]}

    @staticmethod
    def _first_dialog(calls):
        for call in calls:
            if call.name in ("ShowOkDlg", "ShowSelDlg", "ShowMsgDlg",
                             "MsgBox", "MsgSelBox") and len(call.args) > 1:
                return call.args[1]
        return None

    def start_click(self, script, pc=None, self_obj=None, self_pos=None,
                    self_handle=None):
        """Run `SCR_CLICK_<script>` until it finishes or asks the player something."""
        self.ticks = 0
        self._tx = {}
        self._tx_next = 0
        # item changes a COMMITTED transaction made, for the caller to send to the
        # client: (kind, class_id, count, item_id). Only filled after a commit - a
        # rolled-back transaction must leave the client's bag untouched.
        self.item_events = []
        self.prop_events = []
        self.trade_events = []
        self.lobby_events = []
        self.setpos_events = []
        self.chat_events = []
        self._prop_map = None
        commander = getattr(self._local, "commander", None) or self.commander_id
        name = self.commander_name or "Tester"
        props = {"Name": name, "CommName": name, "Level": 1}
        if self.position:
            # `GetPos(pc)` must answer where the player really stands.
            props["X"], props["Y"], props["Z"] = self.position[:3]
        pc = pc if pc is not None else CommanderBag(self, commander, **props)
        # kept so a commit can write its values back into the bag the script sees
        self._pc = pc
        if self_obj is None:
            npc_props = {"Script": script}
            if self_pos:
                npc_props.update(X=self_pos[0], Y=self_pos[1], Z=self_pos[2])
            if self_handle is not None:
                # `ChatNPC` speaks AS the NPC, so the bag must know its handle.
                npc_props["Handle"] = int(self_handle)
            self_obj = PropBag("npc", **npc_props)
        before = len(self.calls)
        func = self.function("SCR_CLICK_" + script)
        co = self._rawget("__co_new")(func)
        return self._step(co, (self_obj, pc), before)

    def resume_click(self, co, choice, before=0):
        """Hand the player's pick back to the suspended script."""
        self.ticks = 0
        self._tx = {}
        self._tx_next = 0
        # item changes a COMMITTED transaction made, for the caller to send to the
        # client: (kind, class_id, count, item_id). Only filled after a commit - a
        # rolled-back transaction must leave the client's bag untouched.
        self.item_events = []
        self.prop_events = []
        self.trade_events = []
        self.lobby_events = []
        self.setpos_events = []
        self.chat_events = []
        self._prop_map = None
        return self._step(co, (choice,), before)

    def run_click(self, script, pc=None, self_obj=None):
        """Run to the end, answering every question with option 1.

        Used by the offline measurements; the server uses `start_click` and
        `resume_click` so the PLAYER answers.
        """
        before = len(self.calls)
        res = self.start_click(script, pc, self_obj)
        guard = 0
        while res["ok"] and not res["done"]:
            guard += 1
            if guard > 20:
                res = {"ok": False, "done": True, "dialog": None,
                       "error": "more than 20 questions", "question": None,
                       "calls": self.calls[before:]}
                break
            res = self.resume_click(res["co"], 1, before)
        res["calls"] = self.calls[before:]
        if res.get("dialog") is None and res["calls"]:
            res["dialog"] = self._first_dialog(res["calls"])
        return res


def _norm(text):
    """Normalise a dialog line for text matching.

    Markup (`{#F8D752}`, `{/}`, `{br}`), format placeholders (`%s`) and
    punctuation differ between the pack's copy and the client's copy of the SAME
    line, which is why exact equality missed id 7453 ("Mr. Panfilo ..." is the
    client's 10135170).
    """
    s = re.sub(r"\{[^}]*\}", " ", str(text))
    s = re.sub(r"%[-0-9.]*[a-zA-Z]", " ", s)
    s = s.lower()
    s = re.sub(r"[^a-z0-9 ]+", " ", s)
    return re.sub(r"\s+", " ", s).strip()


def _index_client(client):
    """normalised text -> ids, plus a short-prefix bucket for fuzzy lookups."""
    exact, buckets = {}, {}
    for cid, text in client.items():
        n = _norm(text)
        if not n:
            continue
        exact.setdefault(n, []).append(cid)
        buckets.setdefault(n[:24], []).append(cid)
    return exact, buckets


def _candidates(pack_text, exact, buckets, client, limit=3):
    """Client ids whose wording matches this pack line, best first."""
    from difflib import SequenceMatcher
    n = _norm(pack_text)
    if not n:
        return []
    if n in exact:
        return [(cid, 1.0) for cid in exact[n][:limit]]
    for width in (24, 16, 12):
        shortlist = buckets.get(n[:width]) or []
        if not shortlist:
            continue
        scored = [(SequenceMatcher(None, n, _norm(client.get(cid, ""))).ratio(), cid)
                  for cid in shortlist[:2000]]
        scored.sort(reverse=True)
        best = [(cid, round(r, 3)) for r, cid in scored[:limit] if r >= 0.80]
        if best:
            return best
    return []



def ids_report(limit=None):
    """Which click scripts send a dialog id the CLIENT cannot draw?

    The client is the authority: an id it has no text for is drawn as a bare
    number. For each such id this prints the wording the SERVER PACK has for it
    and the client ids whose text matches, so the right id can be chosen by
    text, not by guessing.
    """
    import ge_locale
    import make_locale_map
    client = ge_locale.client_table()
    pack = make_locale_map.pack_table()
    exact, buckets = _index_client(client)

    host = ScriptHost()
    names = sorted(n for n in host.index() if n.startswith("SCR_CLICK_"))
    rows, missing = [], {}
    for name in names:
        res = host.run_click(name[len("SCR_CLICK_"):])
        text = res["dialog"]
        if not text:
            continue
        ids = re.findall(r"<\$>(\d+)</>", text)
        for cid in ids:
            if cid in client:
                continue
            wording = pack.get(cid, "")
            cands = _candidates(wording, exact, buckets, client)
            missing.setdefault(cid, {"wording": wording, "candidates": cands,
                                     "scripts": []})["scripts"].append(
                name[len("SCR_CLICK_"):])
            rows.append((name, cid))
        if limit and len(rows) >= limit:
            break

    lines = ["# dialog ids the CLIENT cannot draw (from running the scripts)",
             "# id | pack wording | client ids with the same text | scripts"]
    for cid, info in sorted(missing.items(), key=lambda kv: -len(kv[1]["scripts"])):
        shown = ", ".join("%s(%.2f)" % (c, s) for c, s in info["candidates"]) \
            or "no candidate"
        lines.append("%s | %s | %s | %s" % (
            cid, (info["wording"] or "(no pack text)")[:60], shown,
            ", ".join(sorted(set(info["scripts"]))[:6])))
    import os as _os
    out = _os.path.join(os.path.dirname(os.path.dirname(_os.path.abspath(__file__))),
                        "data", "script_dialog_ids.txt")
    with open(out, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")

    print("click scripts run : %d" % len(names))
    print("undrawable ids    : %d" % len(missing))
    print("scripts affected  : %d" % len({r[0] for r in rows}))

    def _safe(s):
        return str(s).encode("ascii", "replace").decode()

    for cid, info in sorted(missing.items(), key=lambda kv: -len(kv[1]["scripts"]))[:12]:
        print("  %-8s %-42s -> %s" % (
            cid, _safe((info["wording"] or "(no pack text)")[:42]),
            ", ".join("%s(%.2f)" % (c, s) for c, s in info["candidates"]) or "-"))
    print("written:", out)
    return 0


def coverage(limit=None, skip_loops=False):
    """Run EVERY `SCR_CLICK_` script and report how many get a dialog.

    Loops are guarded by `_vtick()` (see `Translator.guard`), so a `while(1)`
    script raises "ran too long" instead of hanging the run. `skip_loops` is
    kept for the old crude filter and is off by default.
    """
    import collections
    host = ScriptHost()
    names = sorted(n for n in host.index() if n.startswith("SCR_CLICK_"))
    ok = nodialog = 0
    skipped = 0
    errors = collections.Counter()
    for name in names:
        path = host.file_for(name)
        if skip_loops:
            try:
                with open(path, encoding="latin-1") as fh:
                    if "while(1)" in fh.read().replace(" ", "").replace("\t", ""):
                        skipped += 1
                        continue
            except OSError:
                pass
        res = host.run_click(name[len("SCR_CLICK_"):])
        if not res["ok"]:
            key = res["error"][:70]
            errors["timeout" if "ran too long" in key else key] += 1
        elif res["dialog"]:
            ok += 1
        else:
            nodialog += 1
        if limit and ok + nodialog + len(errors) >= limit:
            break
    print("click scripts      : %d" % len(names))
    print("ran                : %d" % (ok + nodialog + sum(errors.values())))
    print("  dialog produced  : %d" % ok)
    print("  ran, no dialog   : %d" % nodialog)
    print("  error            : %d" % sum(errors.values()))
    print("skipped while(1)   : %d" % skipped)
    for msg, n in errors.most_common(8):
        print("   %4d  %s" % (n, msg))
    return 0


def selftest(trace=False, limit=6):
    """Run the first few real click scripts found on disk."""
    host = ScriptHost()
    names = sorted(n for n in host.index() if n.startswith("SCR_CLICK_"))
    if not names:
        print("no SCR_CLICK_ functions found under", host.root)
        return 1
    shown = 0
    for name in names:
        script = name[len("SCR_CLICK_"):]
        res = host.run_click(script)
        if not res["ok"]:
            continue
        print("=" * 60)
        print("%-40s calls=%d" % (script, len(res["calls"])))
        print("  dialog: %r" % (res["dialog"],))
        if trace:
            for call in res["calls"][:12]:
                print("   ", call)
        shown += 1
        if shown >= limit:
            break
    print("=" * 60)
    print("ran %d scripts, %d calls recorded" % (shown, len(host.calls)))
    return 0


def main():
    ap = argparse.ArgumentParser(description="Run .scp scripts against stub builtins.")
    ap.add_argument("--selftest", action="store_true", help="run real click scripts")
    ap.add_argument("--trace", action="store_true", help="print every recorded call")
    ap.add_argument("--limit", type=int, default=6)
    ap.add_argument("--coverage", action="store_true",
                    help="run every click script and count dialogs")
    ap.add_argument("--ids", action="store_true",
                    help="list dialog ids the client cannot draw, with candidates")
    args = ap.parse_args()
    if args.ids:
        return ids_report()
    if args.coverage:
        return coverage()
    if args.selftest:
        return selftest(trace=args.trace, limit=args.limit)
    ap.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
