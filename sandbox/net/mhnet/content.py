"""The generic answer: what a web server would send for a path nothing in
services/ knows — a small, valid file of the type the path names, so that
code that parses what it fetched (JSON, an image, an archive) goes on as
it would. Fixed content, ours: nothing of the request is echoed."""
import gzip
import io
import posixpath
import struct
import zipfile
import zlib

from .http import Answer


def _png():
    """A 1×1 transparent PNG."""
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(b"\x00\x00\x00\x00\x00")) + chunk(b"IEND", b""))


def _zip():
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w"):
        pass
    return buf.getvalue()


HTML = (b"<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>Welcome</title>\n</head>\n"
        b"<body>\n<h1>Welcome</h1>\n<p>It works.</p>\n</body>\n</html>\n")
GIF = b"GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"

# extension → (content type, body)
TYPES = {
    ".html": ("text/html; charset=utf-8", HTML), ".htm": ("text/html; charset=utf-8", HTML),
    ".php": ("text/html; charset=UTF-8", HTML), ".asp": ("text/html; charset=utf-8", HTML),
    ".json": ("application/json", b"{}"),
    ".js": ("application/javascript", b"\n"), ".mjs": ("application/javascript", b"\n"),
    ".css": ("text/css", b"\n"),
    ".txt": ("text/plain; charset=utf-8", b"\n"), ".md": ("text/markdown; charset=utf-8", b"\n"),
    ".xml": ("application/xml", b"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<root/>\n"),
    ".svg": ("image/svg+xml", b"<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\"/>\n"),
    ".png": ("image/png", _png()), ".gif": ("image/gif", GIF), ".ico": ("image/x-icon", _png()),
    ".zip": ("application/zip", _zip()),
    ".gz": ("application/gzip", gzip.compress(b"\0" * 1024, mtime=0)),
    ".tgz": ("application/gzip", gzip.compress(b"\0" * 1024, mtime=0)),
    ".sh": ("text/x-sh", b"#!/bin/sh\nexit 0\n"),
    ".py": ("text/x-python", b"\n"),
    ".ps1": ("text/plain", b"\n"),
}
PAGE = ("text/html; charset=utf-8", HTML)            # / and paths with no extension
OTHER = ("application/octet-stream", b"")            # a file of a type not above


def generic(req):
    if req.method in ("POST", "PUT", "PATCH", "DELETE"):
        # an API took it: what most answer with a body of JSON
        return Answer("", "200 OK", "application/json", b'{"status":"ok"}', [])
    ext = posixpath.splitext(req.path.lower())[1]
    if req.path.lower().endswith(".tar.gz"):
        ext = ".tgz"
    ctype, body = TYPES.get(ext) or (PAGE if not ext else OTHER)
    return Answer("", "200 OK", ctype, body, [])
