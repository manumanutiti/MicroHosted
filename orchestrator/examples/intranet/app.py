#!/usr/bin/env python3
"""app — the intranet's web application.

  GET /        count the visit in kv, log it to the collector, greet
  GET /health  ok while the process serves (kv being down is not app's fault)

Usage: app.py PORT KV_HOST:PORT LOGS_HOST:PORT
"""
import socket
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
KV = sys.argv[2]
LOGS_HOST, LOGS_PORT = sys.argv[3].split(":")
LOGS = (LOGS_HOST, int(LOGS_PORT))
log_sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)


def syslog(msg):
    # RFC 3164-style, facility user, severity info: <14>
    try:
        log_sock.sendto(f"<14>app: {msg}".encode(), LOGS)
    except OSError:
        pass  # a missing collector never breaks a request


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        raw = (body + "\n").encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, "ok")
        if self.path != "/":
            return self.reply(404, "not found")
        client = self.client_address[0]
        try:
            req = urllib.request.Request(f"http://{KV}/incr/visits", data=b"", method="POST")
            with urllib.request.urlopen(req, timeout=2) as r:
                n = r.read().decode().strip()
        except OSError as e:
            syslog(f"visit from {client}: kv unreachable ({e})")
            return self.reply(503, f"app is up but kv ({KV}) is not: {e}")
        syslog(f"visit #{n} from {client}")
        self.reply(200, f"Hello from app: visit #{n} (counted in kv at {KV}, logged to {LOGS[0]})")

    def log_message(self, fmt, *args):
        pass


print(f"app listening on :{PORT}, kv {KV}, logs {LOGS}", flush=True)
ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
