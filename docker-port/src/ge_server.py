"""ge_server - first real stub: answer CB_LOGIN with BC_LOGINOK.

Replaces barrack_stub.py. Listens on 127.0.0.1:7001, decrypts what the client
sends, prints it, and replies to the login packet so the client can move on.

Field sizes are taken from the client source + the captures:
  ACC_NAME_LEN = 218  (empirical: id ends and PW lands at +230)
  PWD_LEN      = 17
  AID/SESSIONID = DWORD; USER_RESTRICTION_INFO = {DWORD age; double}

Run:  python ge_server.py
Then click LOGIN in the client. Ctrl+C to stop.
"""
import datetime
import socket
import struct

import ge_crypt as gc

HOST = "127.0.0.1"
PORT = 7001

CMD_CB_LOGIN = 4
CMD_BC_LOGINOK = 8

ACC_NAME_LEN = 218
PWD_LEN = 17


def log(msg):
    stamp = datetime.datetime.now().strftime("%H:%M:%S")
    print("[%s] %s" % (stamp, msg), flush=True)


def parse_login(pt):
    """Pull the interesting fields out of a CB_LOGIN packet."""
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
    payload = struct.pack("<H", CMD_BC_LOGINOK)
    payload += struct.pack("<I", account_id)
    payload += struct.pack("<I", session_id)
    payload += ident.encode("latin-1")[:ACC_NAME_LEN - 1].ljust(ACC_NAME_LEN, b"\0")
    payload += struct.pack("<Id", 0, 0.0)   # USER_RESTRICTION_INFO
    payload += b"\0" * 10                    # birthday
    return payload


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    log("UP on %s:%d - waiting for client (key=%s)" % (HOST, PORT, gc.KEY.decode()))
    try:
        while True:
            conn, addr = srv.accept()
            conn.settimeout(30)
            log("CONNECT from %s:%d" % addr)
            try:
                buf = b""
                while True:
                    data = conn.recv(4096)
                    if not data:
                        log("CLOSE by client")
                        break
                    buf += data
                    while len(buf) >= 2:
                        block, = struct.unpack_from("<H", buf, 0)
                        if len(buf) < 2 + block:
                            break
                        wire, buf = buf[:2 + block], buf[2 + block:]
                        pt = gc.decrypt_packet(wire)
                        cmd, = struct.unpack_from("<H", pt, 0)
                        log("RECV len=%d command=%d" % (block, cmd))
                        log("     text=%r" % pt[:40])
                        if cmd == CMD_CB_LOGIN:
                            info = parse_login(pt)
                            log("     LOGIN %s" % info)
                            reply = build_loginok(0x11112222, 0x00003333, info["id"])
                            # Server -> client is PLAINTEXT (CClient::Send does a
                            # raw m_netStream->Send). Only client -> server is
                            # encrypted. No length prefix either.
                            conn.sendall(reply)
                            log("SENT LOGINOK plaintext len=%d (payload %d)"
                                % (len(reply), len(reply)))
                            log("     reply text=%r" % reply[:40])
            except socket.timeout:
                log("TIMEOUT - closing")
            except Exception as e:
                log("ERROR %r" % e)
            finally:
                conn.close()
    except KeyboardInterrupt:
        log("STOP by user")
    finally:
        srv.close()


if __name__ == "__main__":
    main()
