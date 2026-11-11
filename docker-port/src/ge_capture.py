"""ge_capture.py - a logging TCP relay for the live Granado Espada server.

WHY: to answer "what does a WORKING server actually send?", capture a real
one. Server->client is PLAINTEXT in this engine, so relaying it gives the
exact bytes of every packet the live server sends - including the
ZC_SPAWN_STANDINGPC (cmd 3520) that tells a client about another player,
which is the packet we have been reconstructing by hand.

RUN IT ON THE MACHINE THAT CAN REACH THE LIVE SERVER:

    python ge_capture.py

It does three things:
  1. asks the patch server's server list WHICH server to dial - the live game
     moved region (Singapore -> Netherlands) and the address can change under
     us, so it is never hardcoded here;
  2. listens on 127.0.0.1:7001 and relays to the live server. The live server
     tells the client the zone's address, so the login leg rewrites that
     address to 127.0.0.1 - and the zone port is read out of the same packet,
     so the script opens a second listener on exactly that port and relays the
     zone leg too;
  3. logs everything the live server sends.

To point the client at this relay, either use a client that is already
configured for 127.0.0.1 (ours is), or run with --point-client, which edits
the client's server list and blanks its ServerListURL (with a backup). Use
--restore to put the client back.

The client->server direction is Blowfish-encrypted and carries the login
hash, so it is NEVER stored: only its byte count is logged.

Everything is written in logs\\ge_capture\\:
    stream_<leg>.bin   the raw server->client bytes
    stream_<leg>.txt   the same, as a hexdump with timestamps
    capture.log        what the relay did
"""

import datetime
import os
import re
import socket
import struct
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(os.path.dirname(HERE), "logs", "ge_capture")

# Where the live server is. NOT hardcoded: the client gets it from the patch
# server's server list, and the live game moved region, so the address can
# change. This script asks the same list, and asks again if a connection
# fails.
SERVERLIST_URL = os.environ.get(
    "GE_SERVERLIST_URL",
    "https://gepatcheu.fra1.digitaloceanspaces.com/serverlist.xml")
LIVE_IP = os.environ.get("GE_LIVE_IP")      # set this to override
LIVE_PORT = int(os.environ.get("GE_LIVE_PORT", "7001"))
LOGIN_LISTEN = int(os.environ.get("GE_LOGIN_LISTEN", "7001"))

# Where the client's release folder is, for --point-client.
CLIENT_DIRS = [d for d in (
    os.environ.get("GE_CLIENT_DIR"),
    os.path.join(HERE, "GEAndromida", "AndroReborn", "release"),
    os.path.join(HERE, "release"),
) if d]
CLIENT_XML = "client.xml"
SERVERLIST = "serverlist_recent.xml"
BACKUP_DIR = "refcap_backup"

_lock = threading.Lock()
_zone_port = [None]
_streams = {}
_opened = set()
_live = [None]          # the (ip, port) we are relaying to
_rewrites = {}          # ip -> (old bytes, new bytes)


def now():
    return datetime.datetime.now().strftime("%H:%M:%S.%f")[:-3]


def log(msg):
    line = "[%s] %s" % (now(), msg)
    print(line, flush=True)
    try:
        os.makedirs(OUT, exist_ok=True)
        with open(os.path.join(OUT, "capture.log"), "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass


def fetch_serverlist():
    """Ask the patch server which server to dial. -> (ip, port) or None."""
    try:
        import urllib.request
        with urllib.request.urlopen(SERVERLIST_URL, timeout=20) as r:
            data = r.read().decode("latin-1", "replace")
    except Exception as exc:
        log("  could not fetch the server list: %r" % exc)
        return None
    m = re.search(r'Server0_IP="([^"]+)"[^>]*?Server0_Port="(\d+)"', data)
    if not m:
        log("  the server list did not name a server")
        return None
    return m.group(1), int(m.group(2))


def live_target(port):
    """The (ip, port) to relay to, re-resolved if the address changed."""
    with _lock:
        if _live[0] is None:
            if LIVE_IP:
                _live[0] = (LIVE_IP, port)
            else:
                got = fetch_serverlist()
                _live[0] = (got[0], port) if got else None
        ip, p = _live[0]
        _live[0] = (ip, p if p else port)
        return _live[0]


def re_resolve():
    """Forget the address, so the next connection asks the server list again."""
    with _lock:
        if LIVE_IP:
            return
        _live[0] = None
        _rewrites.clear()
    log("  the address will be looked up again on the next connection")


def rewrite_pair(ip):
    """The address bytes to replace, and the same length with 127.0.0.1."""
    with _lock:
        pair = _rewrites.get(ip)
        if pair is None:
            old = ip.encode("ascii")
            new = b"127.0.0.1" + b"\0" * (len(old) - len(b"127.0.0.1"))
            pair = (old, new)
            _rewrites[ip] = pair
        return pair


def hexdump(data):
    out = []
    for off in range(0, len(data), 16):
        chunk = data[off:off + 16]
        hexpart = " ".join("%02X" % b for b in chunk)
        asciipart = "".join(chr(b) if 32 <= b < 127 else "." for b in chunk)
        out.append("   %06X  %-47s  %s" % (off, hexpart, asciipart))
    return "\n".join(out) + "\n"


def stream_for(leg):
    with _lock:
        s = _streams.get(leg)
        if s is None:
            os.makedirs(OUT, exist_ok=True)
            s = [open(os.path.join(OUT, "stream_%s.bin" % leg), "ab"),
                 open(os.path.join(OUT, "stream_%s.txt" % leg), "a", encoding="utf-8")]
            _streams[leg] = s
        return s


def note_zone_port(data, ip):
    """The login packet that names the zone also carries the zone's PORT.

    The address is a 16-byte C string; the port is the DWORD right after it.
    When we see it, open a listener on that port so the client's zone
    connection comes to us too.
    """
    old, _new = rewrite_pair(ip)
    i = data.find(old)
    if i < 0:
        return
    port = struct.unpack_from("<I", data, i + len(old))[0]
    if not (1 <= port <= 65535) or port == _zone_port[0]:
        return
    _zone_port[0] = port
    log("  the live server's zone is %s:%d - opening a listener on 127.0.0.1:%d"
        % (ip, port, port))
    threading.Thread(target=serve, args=("zone", port, port), daemon=True).start()


def find_client_dir():
    for d in CLIENT_DIRS:
        if os.path.isfile(os.path.join(d, CLIENT_XML)):
            return d
    return None


def client_config(restore=False):
    """Point the client at this relay (or put it back)."""
    d = find_client_dir()
    if d is None:
        log("!! the client's release folder was not found - tried: %s"
            % ", ".join(CLIENT_DIRS))
        return
    bak = os.path.join(d, BACKUP_DIR)
    os.makedirs(bak, exist_ok=True)
    for name in (CLIENT_XML, SERVERLIST):
        p = os.path.join(d, name)
        b = os.path.join(bak, name)
        if restore:
            if os.path.isfile(b):
                with open(b, "rb") as f:
                    data = f.read()
                with open(p, "wb") as f:
                    f.write(data)
                log("restored %s from the backup" % name)
            else:
                log("no backup of %s to restore" % name)
            continue
        if not os.path.isfile(p):
            continue
        with open(p, "rb") as f:
            raw = f.read()
        if not os.path.isfile(b):
            with open(b, "wb") as f:
                f.write(raw)
            log("backed up %s -> %s" % (name, BACKUP_DIR))
        s = raw.decode("latin-1")
        if name == CLIENT_XML:
            new = re.sub(r'ServerListURL="[^"]*"', 'ServerListURL=""', s)
        else:
            new = s
            for ip in ("15.235.233.176", "167.86.115.184"):
                new = new.replace('Server0_IP="%s"' % ip, 'Server0_IP="127.0.0.1"')
                new = new.replace('Server1_IP="%s"' % ip, 'Server1_IP="127.0.0.1"')
        if new != s:
            with open(p, "wb") as f:
                f.write(new.encode("latin-1"))
            log("%s: updated for the capture" % name)
        else:
            log("%s: already set for the capture" % name)


def serve(leg, listen_port, target_port):
    key = (leg, listen_port)
    with _lock:
        if key in _opened:
            return
        _opened.add(key)
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        srv.bind(("127.0.0.1", listen_port))
    except OSError as exc:
        log("cannot listen on %d: %r" % (listen_port, exc))
        return
    srv.listen(5)
    log("listening on 127.0.0.1:%d  (%s leg)" % (listen_port, leg))
    while True:
        try:
            client, addr = srv.accept()
        except OSError:
            return
        threading.Thread(target=handle, args=(leg, client, addr, target_port),
                         daemon=True).start()


def handle(leg, client, addr, target_port):
    log("CONNECT from %s:%d (%s leg)" % (addr[0], addr[1], leg))
    ip, port = live_target(target_port)
    server = None
    for attempt in (1, 2):
        try:
            server = socket.create_connection((ip, port), timeout=15)
            break
        except Exception as exc:
            log("  cannot reach %s:%d: %r" % (ip, port, exc))
            if attempt == 1:
                re_resolve()
                ip, port = live_target(target_port)
    if server is None:
        client.close()
        return
    server.settimeout(None)
    client.settimeout(None)
    c2s = [0]

    def pump_c2s():
        try:
            while True:
                d = client.recv(65536)
                if not d:
                    break
                c2s[0] += len(d)
                server.sendall(d)
        except Exception:
            pass
        finally:
            try:
                server.shutdown(socket.SHUT_WR)
            except Exception:
                pass

    def pump_s2c():
        old, new = rewrite_pair(ip)
        try:
            while True:
                d = server.recv(65536)
                if not d:
                    break
                s = stream_for(leg)
                with _lock:
                    s[0].write(d)
                    s[0].flush()
                    s[1].write("--- server->client %d bytes @ %s ---\n" % (len(d), now()))
                    s[1].write(hexdump(d))
                    s[1].flush()
                out = d
                if old in out:
                    out = out.replace(old, new)
                    log("  rewrote the server address -> 127.0.0.1 in a %d-byte chunk" % len(d))
                    note_zone_port(d, ip)
                client.sendall(out)
        except Exception:
            pass
        finally:
            try:
                client.shutdown(socket.SHUT_WR)
            except Exception:
                pass

    t1 = threading.Thread(target=pump_c2s, daemon=True)
    t2 = threading.Thread(target=pump_s2c, daemon=True)
    t1.start()
    t2.start()
    t1.join()
    t2.join()
    log("  closed (%s leg); client->server %d bytes were NOT stored (encrypted)"
        % (leg, c2s[0]))
    try:
        client.close()
    except OSError:
        pass
    try:
        server.close()
    except OSError:
        pass


def main():
    os.makedirs(OUT, exist_ok=True)
    restore = "--restore" in sys.argv
    log("capture relay starting")
    if restore:
        client_config(restore=True)
        log("the client is back to normal")
        return
    if LIVE_IP:
        log("  live server %s:%d (fixed by GE_LIVE_IP)" % (LIVE_IP, LIVE_PORT))
    else:
        got = fetch_serverlist()
        if got:
            log("  live server %s:%d (from the server list; it is re-read if a "
                "connection fails)" % got)
        else:
            log("  !! the server list could not be read - set GE_LIVE_IP")
    log("  output in %s" % OUT)
    if "--point-client" in sys.argv:
        client_config(restore=False)
    else:
        log("not touching any client config (--point-client if you want the "
            "client pointed at 127.0.0.1)")
    serve("login", LOGIN_LISTEN, LIVE_PORT)
    log("ready - start the client and log in")
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        log("STOP by user")


if __name__ == "__main__":
    main()
