#!/usr/bin/env python3
"""logs — the intranet's log collector.

  UDP 514     receives syslog lines from the other machines
  GET /tail   the last lines received (?n=20), with the sender's address
  GET /health ok

Keeps the newest 1000 lines in memory.
"""
import socket
import sys
import threading
import time
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

HTTP_PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
lines = deque(maxlen=1000)
lock = threading.Lock()


def receive():
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind(("0.0.0.0", 514))
    while True:
        data, (addr, _) = sock.recvfrom(4096)
        msg = data.decode(errors="replace").strip()
        if msg.startswith("<") and ">" in msg[:5]:
            msg = msg[msg.index(">") + 1:]  # drop the syslog priority
        with lock:
            lines.append(f"{time.strftime('%H:%M:%S')} {addr} {msg}")


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        raw = (body + "\n").encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        url = urlparse(self.path)
        if url.path == "/health":
            return self.reply(200, "ok")
        if url.path == "/tail":
            n = int(parse_qs(url.query).get("n", ["20"])[0])
            with lock:
                last = list(lines)[-n:]
                total = len(lines)
            return self.reply(200, f"{total} lines kept, last {len(last)}:\n" + "\n".join(last))
        self.reply(404, "not found")

    def log_message(self, fmt, *args):
        pass


threading.Thread(target=receive, daemon=True).start()
print(f"logs collecting syslog on udp/514, serving on :{HTTP_PORT}", flush=True)
ThreadingHTTPServer(("0.0.0.0", HTTP_PORT), Handler).serve_forever()
