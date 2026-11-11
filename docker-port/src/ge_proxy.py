"""ge_proxy - a logging TCP relay in front of a reference Granado Espada server.

Why: to settle "is our reconstructed server's barrack packet wrong / missing?",
read the WORKING server's own packets. Server->client in this engine is
PLAINTEXT, so relaying it gives byte-exact BC_COMMANDER_INFO / BC_BARRACK_INFO
and the real packet ORDER to diff against ours.

Privacy: the client->server direction is Blowfish-encrypted and carries the
login hash, so it is NEVER stored or decrypted here - only its byte count is
logged. Only the plaintext server->client stream is dumped.

Usage:  python ge_proxy.py [target_ip] [target_port]
"""
import datetime
import os
import tempfile
import socket
import socketserver
import sys
import threading

LISTEN = ("127.0.0.1", 7001)
# The reference server MOVED. The old address (15.235.233.176) is dead - both
# of its ports refuse connections as of 2026-10-21. The live server list
# (https://gepatcheu.fra1.digitaloceanspaces.com/serverlist.xml) now names:
#   85.11.190.15    :7001 (barrack) and :7054 (zone)   <- this one answers
#   167.86.115.184  :7001                                <- does not answer
# Always pass the target explicitly: `python ge_proxy.py <ip> <port> [listen_port]`
TARGET = ("85.11.190.15", 7001)
OUT = os.path.join(tempfile.gettempdir(), "opencode")

if len(sys.argv) > 1:
    TARGET = (sys.argv[1], int(sys.argv[2]) if len(sys.argv) > 2 else 7001)
# Optional 3rd arg: the local port to listen on, so one relay per port can run
# side by side (barrack and zone) and each writes its own capture files.
if len(sys.argv) > 3:
    LISTEN = ("127.0.0.1", int(sys.argv[3]))

_lock = threading.Lock()
_bin = None
_txt = None

# Server address to rewrite while relaying (see pump_s2c). SAME byte length, so
# every packet keeps its size: the reference hands the client its own zone
# address, and we swap it for 127.0.0.1 (padded with NULs to the target's
# length) so the zone leg comes back through OUR second relay.
_REWRITE = (TARGET[0].encode("ascii"), b"127.0.0.1".ljust(len(TARGET[0]), b"\0"))


def now():
    return datetime.datetime.now().strftime("%H:%M:%S.%f")[:-3]


def log(msg):
    line = "[%s] %s" % (now(), msg)
    print(line, flush=True)
    with _lock:
        with open(os.path.join(OUT, "proxy_run.log"), "a", encoding="utf-8") as f:
            f.write(line + "\n")


def hexdump(data):
    out = []
    for off in range(0, len(data), 16):
        chunk = data[off:off + 16]
        hexpart = " ".join("%02X" % b for b in chunk)
        asciipart = "".join(chr(b) if 32 <= b < 127 else "." for b in chunk)
        out.append("   %06X  %-47s  %s" % (off, hexpart, asciipart))
    return "\n".join(out) + "\n"


class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        global _bin, _txt
        client = self.request
        try:
            server = socket.create_connection(TARGET, timeout=15)
        except Exception as exc:
            log("cannot reach %s: %r" % (TARGET, exc))
            return
        server.settimeout(None)
        log("CONNECT from %s:%d -> forwarding to %s:%d"
            % (self.client_address[0], self.client_address[1], TARGET[0], TARGET[1]))

        with _lock:
            _txt.write("\n===== connection %s -> %s  %s =====\n"
                       % (self.client_address, TARGET, now()))
            _txt.flush()

        c2s_bytes = [0]
        done = threading.Event()

        def pump_c2s():
            try:
                while True:
                    d = client.recv(65536)
                    if not d:
                        break
                    c2s_bytes[0] += len(d)
                    server.sendall(d)
            except Exception:
                pass
            finally:
                try:
                    server.shutdown(socket.SHUT_WR)
                except Exception:
                    pass

        def pump_s2c():
            try:
                while True:
                    d = server.recv(65536)
                    if not d:
                        break
                    with _lock:
                        _bin.write(d)
                        _bin.flush()
                        _txt.write("--- server->client %d bytes @ %s ---\n" % (len(d), now()))
                        _txt.write(hexdump(d))
                        _txt.flush()
                    # The reference hands the client its OWN address for the zone
                    # leg, so the client would connect straight to the internet and
                    # bypass this relay. Rewrite that IP to localhost, padded to the
                    # SAME length so every packet keeps its size.
                    out = d
                    if _REWRITE[0] in out:
                        out = out.replace(_REWRITE[0], _REWRITE[1])
                        log("  rewrote server address -> 127.0.0.1 in %d-byte chunk" % len(d))
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
        done.set()
        try:
            client.close()
        except Exception:
            pass
        try:
            server.close()
        except Exception:
            pass
        log("CLOSE  client->server bytes=%d (not stored)" % c2s_bytes[0])


def main():
    global _bin, _txt
    # One file per RUN as well as per port. A fixed filename means the next
    # run silently OVERWRITES the previous capture - and a capture you
    # compared against is exactly the one you cannot afford to lose.
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    tag = "%s_%s" % (stamp, LISTEN[1])
    _bin = open(os.path.join(OUT, "proxy_s2c_%s.bin" % tag), "wb")
    _txt = open(os.path.join(OUT, "proxy_s2c_%s.txt" % tag), "w", encoding="utf-8")
    log("relay listening on %s:%d -> %s:%d" % (LISTEN[0], LISTEN[1], TARGET[0], TARGET[1]))
    log("capture file: proxy_s2c_%s.bin  (this run only - it is not overwritten)" % tag)
    log("point the client at 127.0.0.1:%d and log in now" % LISTEN[1])
    socketserver.ThreadingTCPServer.allow_reuse_address = True
    with socketserver.ThreadingTCPServer(LISTEN, Handler) as srv:
        srv.serve_forever()


if __name__ == "__main__":
    main()
