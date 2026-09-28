#!/usr/bin/env python3
"""Serve index.html with a random word in place of {{WORD}}, a new one on
every request. Nothing is cached, so every refresh shows another word.

Usage: serve.py PORT TEMPLATE
"""
import html
import random
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
with open(sys.argv[2], encoding="utf-8") as f:
    TEMPLATE = f.read()

WORDS = [
    "aurora", "brújula", "cometa", "delta", "eclipse", "faro", "galaxia",
    "horizonte", "iceberg", "jazmín", "koala", "laberinto", "marea", "nebulosa",
    "órbita", "pingüino", "quásar", "relámpago", "satélite", "tornado",
    "universo", "volcán", "wolframio", "xilófono", "yunque", "zafiro",
]


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path not in ("/", "/index.html"):
            self.send_error(404)
            return
        body = TEMPLATE.replace("{{WORD}}", html.escape(random.choice(WORDS))).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)


print(f"serving on :{PORT}", flush=True)
ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
