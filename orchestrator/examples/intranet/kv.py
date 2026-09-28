#!/usr/bin/env python3
"""kv — the intranet's database: an in-memory key-value store over HTTP.

  GET  /            every key, as JSON
  GET  /KEY         a value (404 if unset)
  POST /set/KEY     store the request body
  POST /incr/KEY    add 1 and return the new value
  GET  /health      ok

State lives in memory on purpose: when the orchestrator replaces this VM, the
data is gone, and the other services have to cope — that is part of the test.
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 6379
data = {}
lock = threading.Lock()


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
        with lock:
            if self.path == "/":
                return self.reply(200, json.dumps(data, sort_keys=True))
            value = data.get(self.path[1:])
        if value is None:
            return self.reply(404, "no such key")
        self.reply(200, value)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(min(length, 64 << 10)).decode(errors="replace")
        if self.path.startswith("/set/"):
            with lock:
                data[self.path[5:]] = body
            return self.reply(200, "stored")
        if self.path.startswith("/incr/"):
            key = self.path[6:]
            with lock:
                value = int(data.get(key, "0")) + 1
                data[key] = str(value)
            return self.reply(200, str(value))
        self.reply(404, "unknown operation")

    def log_message(self, fmt, *args):
        pass  # quiet: the console is kept for failures


print(f"kv listening on :{PORT}", flush=True)
ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
