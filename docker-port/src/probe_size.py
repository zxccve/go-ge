"""probe_size - find the shipped client's real sizeof(PKS_BC_COMMANDER_INFO).

The client has no length prefix on server->client fixed packets, so it waits
until it holds its own sizeof of bytes. We send our 601-byte version, then ONE
byte at a time, pausing for a reaction each time. The cumulative byte count at
which the client reacts IS sizeof. The session then continues normally.

Run:  python probe_size.py
Then click LOGIN in the client.
"""
import datetime
import socket
import os
import struct

import ge_crypt as gc
import ge_db
from ge_paths import OUT
from ge_barrack_server import (Session, parse_login, build_loginok,
                               build_commander_info, build_barrack_info,
                               build_equip_extra, build_channel_list,
                               CMD_CB_LOGIN, CMD_CB_STARTBARRACK)

HOST, PORT = "127.0.0.1", 7001
MAX_SIZE = 2600
STEP_WAIT = 0.12


def log(msg):
    print("[%s] %s" % (datetime.datetime.now().strftime("%H:%M:%S"), msg), flush=True)


def read_more(conn, buf, timeout):
    conn.settimeout(timeout)
    try:
        d = conn.recv(8192)
    except socket.timeout:
        return buf, b""
    return buf + d, d


def next_frame(conn, buf):
    """Blocking: return (plaintext, leftover) for the next whole wire frame."""
    while True:
        if len(buf) >= 2:
            block, = struct.unpack_from("<H", buf, 0)
            if 2 + block <= len(buf) and block > 0:
                pt = gc.decrypt_packet(buf[:2 + block])
                return pt, buf[2 + block:]
        conn.settimeout(600)
        d = conn.recv(8192)
        if not d:
            return None, buf
        buf += d


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    log("PROBE server UP on %s:%d" % (HOST, PORT))
    while True:
        conn, addr = srv.accept()
        log("CONNECT from %s:%d" % addr)
        sess = Session()
        buf = b""
        probed = False
        try:
            while True:
                pt, buf = next_frame(conn, buf)
                if pt is None:
                    log("CLOSE by client")
                    break
                cmd, = struct.unpack_from("<H", pt, 0)
                log("RECV command=%d len=%d" % (cmd, len(pt)))

                if cmd == CMD_CB_LOGIN:
                    info = parse_login(pt)
                    acc = ge_db.get_account(info["id"]) or {
                        "account_id": 35, "commander_id": 24, "member_id": info["id"]}
                    sess.account = acc
                    conn.sendall(build_loginok(acc["account_id"], 0x3333, info["id"]))
                    log("SENT LOGINOK")

                elif cmd == CMD_CB_STARTBARRACK and not probed:
                    probed = True
                    cm = ge_db.get_commander(sess.account["commander_id"])
                    base = build_commander_info(cm)
                    conn.sendall(base)
                    cum = len(base)
                    log("sent base COMMANDER_INFO %d bytes; probing 1 byte at a time..." % cum)
                    found = None
                    while cum < MAX_SIZE:
                        conn.sendall(b"\0")
                        cum += 1
                        buf, d = read_more(conn, buf, STEP_WAIT)
                        if d:
                            found = cum
                            break
                    if found is None:
                        log("no reaction up to %d bytes" % MAX_SIZE)
                        break
                    log("*** CLIENT REACTED at cumulative = %d ***" % found)
                    open(os.path.join(OUT, "probe_size.txt"), "w").write(str(found))
                    chars = ge_db.get_characters(cm["commander_id"])
                    conn.sendall(build_barrack_info(chars))
                    conn.sendall(build_equip_extra())
                    conn.sendall(build_channel_list())
                    log("SENT BARRACK_INFO chars=%d (source2 stride 157)" % len(chars))
                    log("==== watch the client screen now ====")
                else:
                    log("    (other command %d, %d bytes)" % (cmd, len(pt)))
        except Exception as e:
            log("ERROR %r" % e)
        finally:
            conn.close()


if __name__ == "__main__":
    main()
