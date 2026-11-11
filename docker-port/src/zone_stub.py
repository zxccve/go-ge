"""zone_stub - placeholder listener for the zone hop (RECON BUILD).

BC_STARTGAMEOK points the client at a zone server IP/port. We do not have a
zone server yet (plan step 4). This stub accepts the connection, stays open,
and logs everything the client sends so we can see what it expects.

It also tries to DECRYPT each chunk with the known packet cipher (ge_crypt).
The client -> server direction is framed `[WORD blockByte][ciphertext]` and used
the same Blowfish key on the barrack port; if the zone port is the same, the
decrypt will show readable command ids. If not, the raw hex still tells us a lot.

Run:  python zone_stub.py     (listens on 127.0.0.1:7002)
"""
import datetime
import socket

try:
    import ge_crypt
except Exception as exc:            # cipher optional - raw capture still works
    ge_crypt = None
    print("(ge_crypt not loaded: %s)" % exc)

HOST = "127.0.0.1"
PORT = 7002
HOLD_SECONDS = 120                  # stay open so the client is not reset early


def log(msg):
    print("[%s] %s" % (datetime.datetime.now().strftime("%H:%M:%S"), msg), flush=True)


def hexs(b):
    return " ".join("%02X" % x for x in b)


def try_decrypt(buf):
    if ge_crypt is None or len(buf) < 2:
        return None
    try:
        return ge_crypt.decrypt_packet(buf)
    except Exception as exc:
        return "decrypt failed: %s" % exc


def handle(conn, addr):
    conn.settimeout(HOLD_SECONDS)
    log("CONNECT from %s:%d" % addr)
    buf = bytearray()
    try:
        while True:
            data = conn.recv(65536)
            if not data:
                log("client closed after %d bytes" % len(buf))
                break
            buf += data
            log("RECV %d bytes (total %d): %s" % (len(data), len(buf), hexs(data[:96])))
            dec = try_decrypt(bytes(buf))
            if isinstance(dec, bytes):
                log("  DECRYPTED (whole buffer): %s" % hexs(dec[:96]))
            elif dec is not None:
                log("  %s" % dec)
    except socket.timeout:
        log("no more data (timeout) - holding done, %d bytes total" % len(buf))
    except Exception as exc:
        log("error: %s" % exc)
    finally:
        log("RAW TOTAL %d bytes: %s" % (len(buf), hexs(bytes(buf[:128]))))
        dec = try_decrypt(bytes(buf))
        if isinstance(dec, bytes):
            log("RAW TOTAL DECRYPTED: %s" % hexs(dec[:128]))
        conn.close()
        log("CLOSED %s:%d" % addr)


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    log("zone stub UP on %s:%d (hold %ds)" % (HOST, PORT, HOLD_SECONDS))
    try:
        while True:
            conn, addr = srv.accept()
            handle(conn, addr)
    except KeyboardInterrupt:
        log("STOP by user")
    finally:
        srv.close()


if __name__ == "__main__":
    main()
