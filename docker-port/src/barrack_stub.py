"""Milestone 1: fake Barrack listener on 127.0.0.1:7001.

Listens like the real Barrack server's door. Takes client bytes,
saves them as hex + text, then closes. This teaches us the login
words (packet shapes) the client speaks. Run: python barrack_stub.py
Stop: Ctrl+C. Logs go to barrack_stub.log in this folder.
"""
import socket
import datetime
import os

from ge_paths import CAPTURES

HOST = "127.0.0.1"
PORT = 7001
HERE = os.path.dirname(os.path.abspath(__file__))
LOG = os.path.join(CAPTURES, "barrack_stub.log")


def line(msg):
    stamp = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    row = "[%s] %s" % (stamp, msg)
    print(row, flush=True)
    with open(LOG, "a", encoding="utf-8") as f:
        f.write(row + "\n")


def dump(data):
    hexpart = " ".join("%02X" % b for b in data)
    textpart = "".join(chr(b) if 32 <= b < 127 else "." for b in data)
    return "len=%d hex=[%s] txt=[%s]" % (len(data), hexpart, textpart)


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    line("UP on %s:%d - waiting for client..." % (HOST, PORT))
    try:
        while True:
            conn, addr = srv.accept()
            line("CONNECT from %s:%d" % (addr[0], addr[1]))
            conn.settimeout(10)
            try:
                while True:
                    data = conn.recv(4096)
                    if not data:
                        line("CLOSE by client %s:%d" % (addr[0], addr[1]))
                        break
                    line("GOT %s" % dump(data))
            except socket.timeout:
                line("TIMEOUT %s:%d - closing" % (addr[0], addr[1]))
            except ConnectionResetError:
                line("RESET by client %s:%d" % (addr[0], addr[1]))
            finally:
                conn.close()
    except KeyboardInterrupt:
        line("STOP by user")
    finally:
        srv.close()


if __name__ == "__main__":
    main()
