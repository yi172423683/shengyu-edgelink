"""冒烟测试用的源站：TCP 回显，回显时加前缀以便确认数据真的绕了一圈。

用法：  python smoke_origin.py <listen-port> [prefix]
"""

import socket
import sys
import threading

port = int(sys.argv[1]) if len(sys.argv) > 1 else 19090
prefix = sys.argv[2] if len(sys.argv) > 2 else "ECHO:"

srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(32)


def handle(conn):
    try:
        while True:
            data = conn.recv(4096)
            if not data:
                break
            conn.sendall(prefix.encode() + data)
    except OSError:
        pass
    finally:
        try:
            conn.close()
        except OSError:
            pass


while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
