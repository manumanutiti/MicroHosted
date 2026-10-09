"""A connection on a port of any number (main.py: every TCP connection the
code makes comes here), told by its first bytes and answered as that:

  HTTP, on any port: http.py.
  TLS, on any port: a certificate for the name (tls.py), then what is
    inside, told the same way: HTTP is HTTPS; SMTPS, IMAPS, POP3S by port.
  A protocol whose server speaks first, by its port (SMTP, FTP, POP3,
    IMAP, SSH): its greeting, and what a client needs to get to what it
    sends (lines.py).
  Anything else: read until the client stops, unanswered.

Written down, past HTTP's: tcp PORT PROTO HOST BYTES TOKEN FIRST (log.py)."""
import re
import socket
import time

from . import http, lines
from .log import escaped, printable
from .token import carries

WAIT = 2              # seconds a client is given to speak first
DEADLINE = 20         # seconds a conversation may last
MAXKEPT = 1 << 20     # what is kept of what it sent, to look for the token in
FIRST = 400           # what the log keeps of it

METHOD = re.compile(rb"(GET|POST|PUT|HEAD|DELETE|OPTIONS|PATCH|CONNECT|TRACE|PROPFIND) ")
TLS = re.compile(rb"\x16\x03[\x00-\x04]")
# the server speaks first: in the clear on these, after TLS's handshake on IMPLICIT
SERVER_FIRST = {21: "ftp", 22: "ssh", 25: "smtp", 110: "pop3", 143: "imap", 587: "smtp", 2525: "smtp"}
IMPLICIT = {465: "smtp", 993: "imap", 995: "pop3"}


def peek(conn, wait):
    """The client's first bytes, left to be read: 8 of them, or fewer when
    it sent no more within wait seconds."""
    end = time.monotonic() + wait
    data = b""
    while len(data) < 8:
        left = end - time.monotonic()
        if left <= 0:
            break
        conn.settimeout(left)
        try:
            data = conn.recv(8, socket.MSG_PEEK)
        except (socket.timeout, OSError):
            break
        if not data:
            break
        if len(data) < 8:
            time.sleep(0.05)
    return data


class Stream:
    def __init__(self, log, token, certs, answer):
        self.log, self.token, self.certs, self.answer = log, token, certs, answer

    def serve(self, conn, port, host, addr):
        """conn, to port of host (a name, or addr as it was)."""
        proto = SERVER_FIRST.get(port)
        if proto:
            return self.converse(conn, port, proto, host, addr, self.starttls(host))
        first = peek(conn, WAIT)
        if TLS.match(first):
            w = self.certs.wrap(conn, host, self.log)
            if w is not None:
                self.inside(w[0], port, w[1], addr)
            return
        if METHOD.match(first):
            return http.serve(conn, "http", port, host, self.log, self.token, self.answer)
        # a client of SSH's on a port of its own: it says so first
        self.converse(conn, port, "ssh" if first.startswith(b"SSH-") else "tcp", host, addr, None)

    def inside(self, conn, port, name, addr):
        proto = IMPLICIT.get(port)
        if proto:
            return self.converse(conn, port, proto, name, addr, None, tls=True)
        conn.settimeout(WAIT)
        try:
            first = conn.recv(65536)
        except (socket.timeout, OSError):
            first = b""
        if METHOD.match(first):
            return http.serve(conn, "https", port, name, self.log, self.token, self.answer, first)
        self.converse(conn, port, "tls", name, addr, None, first=first)

    def starttls(self, host):
        return lambda conn: self.certs.wrap(conn, host, self.log)

    def converse(self, conn, port, proto, host, addr, starttls, tls=False, first=b""):
        s = lines.Session(conn, time.monotonic() + DEADLINE, MAXKEPT, first)
        try:
            lines.DIALOGUES.get(proto, lines.raw)(s, host, addr, starttls)
        except OSError:
            pass
        name = proto + "s" if tls and proto != "tls" else proto
        self.log.write("tcp", port, name, printable(host, 253), s.total,
                       1 if carries(s.kept, self.token) else 0, escaped(s.kept, FIRST))
