"""The log mh-sandbox-report reads: one record per line, tab-separated,
every field the code's choice made printable and cut, and last on each the
time it was written (seconds since the epoch).

  dns    TYPE NAME
  addr   ADDRESS NAME            the address given to NAME
  http   SCHEME METHOD URL BYTES TOKEN(0|1|2) PORT ANSWER
                                 TOKEN 2: only in its own service's header
                                 ANSWER: what it was answered as, - generic
  tls    NAME refused            the client did not accept the certificate
  tcp    PORT PROTO HOST BYTES TOKEN FIRST
                                 a connection on a port that is not HTTP's:
                                 the protocol spoken, what it sent (FIRST: its
                                 first bytes, escaped)

At most MAXRECORDS: a loop says nothing new after that.
"""
import threading
import time

MAXRECORDS = 50000


def printable(s, n):
    return "".join(c if c.isprintable() and c != "\t" else "?" for c in s)[:n]


def escaped(data, n):
    """Bytes as text a person can read: printable ASCII as is, the rest \\xNN."""
    out = []
    for b in data:
        out.append(chr(b) if 0x20 <= b < 0x7f and b != 0x5c else "\\x%02x" % b)
        if sum(map(len, out)) >= n:
            break
    return "".join(out)[:n]


class Log:
    def __init__(self, path):
        self.f = open(path, "a", buffering=1)
        self.n = 0
        self.lock = threading.Lock()

    def write(self, *fields):
        with self.lock:
            if self.n < MAXRECORDS:
                self.f.write("\t".join(str(f) for f in fields) + "\t%.3f\n" % time.time())
                self.n += 1
