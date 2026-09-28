#!/usr/bin/env python3
"""A minimal HTTP service: / says where it runs, /health answers ok.

Usage: server.py PORT
"""
import json
import platform
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = b"ok\n"
        elif self.path == "/":
            body = (json.dumps({"host": platform.node(), "python": platform.python_version(),
                                "os": platform.freedesktop_os_release().get("PRETTY_NAME")}) + "\n").encode()
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass  # no access log on the VM's disk


ThreadingHTTPServer(("", int(sys.argv[1])), Handler).serve_forever()
