"""Controlled capture set (task 09).

Listens like barrack_stub.py, but saves each connection's packet to its
own numbered file under .\captures\ so runs never get mixed up.

Run:   python capture_set.py
Stop:  Ctrl+C

Each saved file holds one line:  <timestamp> len=<n> hex=[.. .. ..]

Run sheet (do them in this order, one client login each):
  1. account A, password P1
  2. account A, password P1      (same again -> tests if the body is random)
  3. account A, password P2      (password changed)
  4. account B, password P1      (account changed)
"""
import os
import re
import socket
import datetime

from ge_paths import CAPTURES

HOST = "127.0.0.1"
PORT = 7001
HERE = os.path.dirname(os.path.abspath(__file__))
OUT = CAPTURES
os.makedirs(OUT, exist_ok=True)


def next_index():
    n = 1
    for name in os.listdir(OUT):
        m = re.match(r"cap_(\d+)\.log$", name)
        if m:
            n = max(n, int(m.group(1)) + 1)
    return n


def save(idx, data):
    stamp = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    hexpart = " ".join("%02X" % b for b in data)
    path = os.path.join(OUT, "cap_%02d.log" % idx)
    with open(path, "w", encoding="utf-8") as f:
        f.write("%s len=%d hex=[%s]\n" % (stamp, len(data), hexpart))
    print("[%s] saved %s  len=%d" % (stamp, os.path.basename(path), len(data)),
          flush=True)


def main():
    idx = next_index()
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((HOST, PORT))
    srv.listen(5)
    print("UP on %s:%d - run %d will save next" % (HOST, PORT, idx), flush=True)
    try:
        while True:
            conn, addr = srv.accept()
            buf = b""
            conn.settimeout(10)
            try:
                while True:
                    data = conn.recv(4096)
                    if not data:
                        break
                    buf += data
            except socket.timeout:
                pass
            finally:
                conn.close()
            if buf:
                save(idx, buf)
                idx += 1
    except KeyboardInterrupt:
        print("STOP by user", flush=True)
    finally:
        srv.close()


if __name__ == "__main__":
    main()
