"""world.py - the ONE owner of world state.

Why this file exists
--------------------
The shipped v28 server was a mesh. The zone and the barrack each dialled a
"global server", and that global server owned everything that crosses a map
boundary: who is online and where, parties, chat routing. Its address book
lived in the RESOURCE database (tables MACHINE, PVP_MACHINE, SERVER_INFO,
GROUP) - the original got a machine's IP and a zone's port out of the
database, not out of a config file. See `history/shipped-binaries.md` -> "4d".

We are rebuilding that IDEA without the mesh. This module owns the same
state, in one process, behind a small set of functions. Every other module
(the barrack, the zone) asks THIS module and never keeps its own copy.

The seam: every call goes through the functions below. The same calls can
later be served by a separate process (`SocketWorld`) - a TCP client instead
of a direct call - without touching a single caller. That is the whole point
of building it this way round, and it is why the zone registry below is
written as "a place to look up an address" rather than "a constant".

What lives here today
---------------------
1. THE ZONE REGISTRY - which zone instance serves which map, at what
   address, and whether it is still alive. This is the piece the original
   kept in RESOURCE.MACHINE + SERVER_INFO.PORT, and the piece our barrack
   used to hardcode (127.0.0.1:7002).
   A running zone announces itself with a small presence file; the barrack
   reads the presence files. A zone that dies leaves a stale file, which is
   ignored after ALIVE_TIMEOUT - the same job the original's global server
   did by pushing "Start Zone Server [id]" / "Stop Zone Server [id]".

2. THE MAP TABLE LOOKUP - which map class name belongs to which map id
   (10 -> "reb_basic"). The barrack used to send the literal "reb_basic" for
   every player, which is why a character saved in another map could not be
   entered. The zone already looks the map up properly; now both sides ask
   the same place.

3. THE SESSION REGISTRY - "who is online and in which zone". The original
   kept this in the GE_DATA table SESSION_INFO (which already exists in our
   database and has always been empty) with the global server holding a
   cached copy in memory. We write the table and read the table; there is no
   second copy to fall out of step.

Nothing here is allowed to raise into a caller. A lookup that fails returns
None and the caller falls back to what it did before, so a broken registry
degrades to "one zone on one port" instead of "no game".
"""

import json
import os
import threading
import time

from ge_paths import OUT, ensure

# One presence file per running zone instance.
ZONE_DIR = os.path.join(OUT, "zones")

# A zone that has not refreshed its presence file for this long is treated as
# dead. The original knew instantly because the global server saw the socket
# drop; we approximate it with a heartbeat.
ALIVE_TIMEOUT = 30.0

# How often a running zone refreshes its presence file.
REFRESH_SECONDS = 5.0

# The map table is decoded lazily: it is a big table and most callers only
# need the map name of one id.
_map_names = None
_map_names_lock = threading.Lock()


# --------------------------------------------------------------------------
# the map table
# --------------------------------------------------------------------------
def _load_map_names():
    """map id -> map class name, from the client's own datatable_map.ies.

    The client feeds this name straight into SwitchMode, so it must be the
    CLASS name ("reb_basic" for id 10), not a display name.
    """
    global _map_names
    with _map_names_lock:
        if _map_names is None:
            rows = {}
            try:
                import npc_data
                for zid, entry in npc_data.load_map_table().items():
                    rows[zid] = entry[0]
            except Exception as exc:  # data missing -> fall back, do not crash
                print("world: map table unavailable (%s)" % exc)
            _map_names = rows
        return _map_names


def map_name(map_id):
    """The map class name for a map id ("reb_basic" for 10), or None.

    A live zone's own announcement wins over the table: the zone knows which
    world it is actually serving.
    """
    for rec in zone_records():
        if rec.get("map_id") == map_id and rec.get("map_name"):
            return rec["map_name"]
    return _load_map_names().get(map_id)


# --------------------------------------------------------------------------
# the zone registry
# --------------------------------------------------------------------------
def _zone_path(port):
    """The presence file of the zone listening on `port`.

    Keyed by the port, not by the map: a zone process serves one map at a
    time, and it rewrites the same file when it moves to another map.
    """
    return os.path.join(ZONE_DIR, "zone_%s.json" % port)


def _write_atomic(path, rec):
    """Write JSON without leaving a half-written file behind."""
    ensure(ZONE_DIR)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(rec, f)
    os.replace(tmp, path)


def zone_records():
    """Every zone instance that announced itself and is still alive.

    A zone that was killed leaves its file behind; the file goes stale and
    is skipped, so the registry never points at a dead port for long.
    """
    out = []
    try:
        names = os.listdir(ZONE_DIR)
    except OSError:
        return out
    now = time.time()
    for fn in names:
        if not fn.endswith(".json"):
            continue
        path = os.path.join(ZONE_DIR, fn)
        try:
            with open(path, "r", encoding="utf-8") as f:
                rec = json.load(f)
        except (OSError, ValueError):
            continue
        seen = rec.get("last_seen") or 0
        if now - seen > ALIVE_TIMEOUT:
            continue
        out.append(rec)
    out.sort(key=lambda r: (r.get("map_id", 0), r.get("port", 0)))
    return out


def zone_address(map_id, channel=None):
    """(ip, port) of a live zone serving map_id, or None.

    With several zones for one map, the lowest port wins so the answer is
    stable between two calls.
    """
    cands = [r for r in zone_records() if r.get("map_id") == map_id]
    if channel is not None:
        same = [r for r in cands if r.get("channel") == channel]
        if same:
            cands = same
    if not cands:
        return None
    rec = cands[0]
    return (rec.get("ip") or "127.0.0.1", int(rec.get("port") or 0))


def announce(map_id, ip, port, channel=0, map_name=None):
    """A zone server says "I serve map_id at ip:port". Returns its file path.

    The presence file is refreshed by `Heartbeat` while the zone runs, and
    removed by `withdraw` on a clean shutdown. A zone that switches to
    another map rewrites the same file, so there is one file per process.
    """
    rec = {
        "map_id": map_id,
        "channel": channel,
        "ip": ip,
        "port": int(port),
        "map_name": map_name,
        "last_seen": time.time(),
    }
    path = _zone_path(port)
    try:
        _write_atomic(path, rec)
    except OSError as exc:
        print("world: could not announce zone %s: %s" % (map_id, exc))
        return None
    return path


def refresh(path, **changes):
    """Update a zone's presence file (the heartbeat)."""
    if not path:
        return
    try:
        with open(path, "r", encoding="utf-8") as f:
            rec = json.load(f)
    except (OSError, ValueError):
        return
    rec.update(changes)
    rec["last_seen"] = time.time()
    try:
        _write_atomic(path, rec)
    except OSError:
        pass


def withdraw(path):
    """Remove a zone's presence file (clean shutdown)."""
    if not path:
        return
    try:
        os.remove(path)
    except OSError:
        pass


class Heartbeat:
    """Keeps a zone's presence file fresh while the zone runs.

    The zone's main loop blocks in accept(), so the refresh has to run on
    its own thread. A daemon thread is enough: it dies with the process.
    """

    def __init__(self, path, **fields):
        self.path = path
        self.fields = fields
        self._stop = threading.Event()
        self._thread = None

    def start(self):
        if not self.path:
            return self
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()
        return self

    def _run(self):
        while not self._stop.wait(REFRESH_SECONDS):
            refresh(self.path, **self.fields)

    def stop(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2.0)
        withdraw(self.path)
