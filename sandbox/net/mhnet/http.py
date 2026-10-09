"""HTTP as the sinkhole speaks it: a request read within bounds, written
down, and answered."""
import email.utils
import re
import ssl
import time
import urllib.parse
from collections import namedtuple

from .log import printable
from .token import token_field

MAXHEAD = 65536
MAXBODY = 1 << 20  # what is kept of a request, to look for the token in
DEADLINE = 20      # seconds a connection may take to send its request, whole

# What a request is answered with. name: what the report says it was
# answered as ("" for the generic answer, which the report shows as -).
Answer = namedtuple("Answer", "name status ctype body headers")
Request = namedtuple("Request", "method url host path query head body")


class Slow(Exception):
    """The connection took longer than DEADLINE to send its request."""


def recv(conn, deadline):
    left = deadline - time.monotonic()
    if left <= 0:
        raise Slow()
    conn.settimeout(min(5, left))
    return conn.recv(65536)


def read_request(conn, deadline, data=b""):
    """The request's head and as much of its body as is kept, and its size
    (data: what was read of it already). A connection that drips its bytes
    is cut at the deadline, whatever it sent: it cannot hold one of the
    sinkhole's slots for the whole run."""
    while b"\r\n\r\n" not in data and len(data) < MAXHEAD:
        chunk = recv(conn, deadline)
        if not chunk:
            break
        data += chunk
    head, _, body = data.partition(b"\r\n\r\n")
    want = 0
    m = re.search(rb"(?im)^content-length:\s*(\d{1,12})", head)
    if m:
        want = int(m.group(1))
    elif re.search(rb"(?im)^transfer-encoding:\s*chunked", head):
        want = MAXBODY  # until it stops, or the deadline
    total = len(body)
    try:
        while total < want:
            chunk = recv(conn, deadline)
            if not chunk:
                break
            total += len(chunk)
            if len(body) < MAXBODY:
                body += chunk[:MAXBODY - len(body)]
    except (Slow, TimeoutError, ssl.SSLError, OSError):
        pass  # what came is written down
    return head, body[:MAXBODY], total


def parse(head, body, scheme, host):
    line = head.split(b"\r\n", 1)[0].decode("latin-1")
    parts = line.split(" ")
    method, target = (parts[0], parts[1]) if len(parts) >= 2 else ("?", "")
    m = re.search(rb"(?im)^host:[ \t]*([^\r\n]+)", head)
    if m:
        host = m.group(1).decode("latin-1").strip()
    url = target if "://" in target else scheme + "://" + host + target
    u = urllib.parse.urlsplit(url)
    return Request(method.upper(), url, (u.hostname or "").lower(), u.path, u.query, head, body)


def header(head, name):
    """A header's value as text, or ""."""
    m = re.search(rb"(?im)^" + re.escape(name.encode()) + rb":[ \t]*([^\r\n]*)", head)
    return m.group(1).decode("latin-1").strip() if m else ""


SERVER = "nginx"  # what a server says it is, unless its answer says otherwise


def respond(conn, req, a):
    hdr = "HTTP/1.1 %s\r\n" % a.status
    names = {k.lower() for k, _ in a.headers}
    if "date" not in names:
        hdr += "Date: %s\r\n" % email.utils.formatdate(usegmt=True)
    if "server" not in names:
        hdr += "Server: %s\r\n" % SERVER
    hdr += "".join("%s: %s\r\n" % kv for kv in a.headers)
    if a.ctype:
        hdr += "Content-Type: %s\r\n" % a.ctype
    if not a.status.startswith(("204", "304")):  # no body, and none said
        hdr += "Content-Length: %d\r\n" % len(a.body)
    hdr += "Connection: close\r\n\r\n"
    conn.sendall(hdr.encode("latin-1") + (b"" if req.method == "HEAD" else a.body))


def serve(conn, scheme, port, host, log, token, answer, first=b""):
    """One request on conn: read, written down, answered by answer(req).
    first: its first bytes, read already (stream.py)."""
    try:
        head, body, size = read_request(conn, time.monotonic() + DEADLINE, first)
        req = parse(head, body, scheme, host)
        a = answer(req)
        log.write("http", scheme, printable(req.method, 16), printable(req.url, 300), size,
                  token_field(req.host, head, body, token), port, a.name or "-")
        respond(conn, req, a)
    except (Slow, OSError, ssl.SSLError, UnicodeError):
        pass
