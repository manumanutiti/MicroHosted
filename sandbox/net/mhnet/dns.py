"""The resolver: every name looked up written down, and answered as the
internet's would: a name under a TLD that does not exist (IANA's list,
tlds.txt) is NXDOMAIN, with the root's SOA, as a resolver says it."""
import os
import re
import socket
import threading
import time

from .log import printable

TYPES = {1: "A", 2: "NS", 5: "CNAME", 6: "SOA", 12: "PTR", 15: "MX", 16: "TXT",
         28: "AAAA", 33: "SRV", 64: "SVCB", 65: "HTTPS", 255: "ANY"}
HOSTNAME = re.compile(r"^[a-z0-9_]([a-z0-9_-]{0,62}\.)*[a-z0-9_-]{1,63}\.?$")
NXDOMAIN, SERVFAIL = 3, 2
TCP_SLOTS = 16     # connections over TCP served at once; the rest are closed
TCP_DEADLINE = 20  # seconds one may last: a query dripped a byte at a time


def load_tlds():
    with open(os.path.join(os.path.dirname(__file__), "tlds.txt"), encoding="ascii") as f:
        return frozenset(l.strip() for l in f if l.strip() and not l.startswith("#"))


TLDS = load_tlds()


def exists(name):
    """name could be on the internet: a host name under a TLD there is (a
    name with no dot is one only when it is a TLD itself), or the root."""
    name = name.rstrip(".")
    return name == "" or bool(HOSTNAME.match(name)) and name.rsplit(".", 1)[-1] in TLDS


def root_soa():
    """The root zone's SOA, as an authority record: its serial is today's."""
    serial = int(time.strftime("%Y%m%d00", time.gmtime()))
    rdata = (b"\x01a\x0croot-servers\x03net\x00" + b"\x05nstld\x0cverisign-grs\x03com\x00" +
             b"".join(v.to_bytes(4, "big") for v in (serial, 1800, 900, 604800, 86400)))
    # the root, SOA, IN, a day
    return b"\x00\x00\x06\x00\x01\x00\x01\x51\x80" + len(rdata).to_bytes(2, "big") + rdata


def question(pkt):
    """The first question's name and type, and where the question ends."""
    if len(pkt) < 12 or int.from_bytes(pkt[4:6], "big") < 1:
        return None
    i, labels = 12, []
    while True:
        if i >= len(pkt):
            return None
        n = pkt[i]
        i += 1
        if n == 0:
            break
        if n & 0xC0 or i + n > len(pkt):  # no compression in a question
            return None
        labels.append(pkt[i:i + n])
        i += n
    if i + 4 > len(pkt):
        return None
    name = printable(b".".join(labels).decode("ascii", "replace").lower(), 253) or "."
    return name, int.from_bytes(pkt[i:i + 2], "big"), i + 4


def respond(pkt, log, names, sinkhole):
    """The answer to a query, or None for what is not one."""
    q = question(pkt)
    if q is None or pkt[2] & 0x80:  # not a query
        return None
    name, qtype, end = q
    log.write("dns", TYPES.get(qtype, str(qtype)), name)
    answer = authority = b""
    rcode = 0 if sinkhole else SERVFAIL  # servfail: as with no network
    if sinkhole and not exists(name):
        rcode, authority = NXDOMAIN, root_soa()
        log.write("nx", name)
    elif sinkhole and qtype == 1 and name != ".":
        a = names.addr(name)
        if a is not None:
            # the name by pointer to the question, A, IN, 60 s, the address
            answer = b"\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x3c\x00\x04" + a.packed
    # the id, the opcode and RD as asked; QR and RA set: a recursive
    # resolver's answer, not the zone's own (AA)
    flags = bytes([0x80 | (pkt[2] & 0x79), 0x80 | rcode])
    counts = (b"\x00\x01" + (b"\x00\x01" if answer else b"\x00\x00") +
              (b"\x00\x01" if authority else b"\x00\x00") + b"\x00\x00")
    return pkt[0:2] + flags + counts + pkt[12:end] + answer + authority


def serve_udp(sock, log, names, sinkhole):
    while True:
        try:
            pkt, peer = sock.recvfrom(4096)
        except InterruptedError:
            continue
        out = respond(pkt, log, names, sinkhole)
        if out is not None:
            try:
                sock.sendto(out, peer)
            except OSError:
                pass


def serve_tcp(sock, log, names, sinkhole):
    """DNS over TCP (a client's choice, or an answer too long for UDP's):
    each query after its length, a few to a connection."""
    slots = threading.BoundedSemaphore(TCP_SLOTS)
    while True:
        try:
            conn, _ = sock.accept()
        except InterruptedError:
            continue
        if not slots.acquire(blocking=False):
            conn.close()
            continue
        threading.Thread(target=tcp_conn, args=(conn, slots, log, names, sinkhole), daemon=True).start()


def tcp_conn(conn, slots, log, names, sinkhole):
    end = time.monotonic() + TCP_DEADLINE
    try:
        for _ in range(16):
            n = recv_exactly(conn, 2, end)
            pkt = n and recv_exactly(conn, int.from_bytes(n, "big"), end)
            if not pkt:
                return
            out = respond(pkt, log, names, sinkhole)
            if out is None:
                return
            conn.sendall(len(out).to_bytes(2, "big") + out)
    except OSError:
        pass
    finally:
        conn.close()
        slots.release()


def recv_exactly(conn, n, end):
    """n bytes, or None at the end of the connection, or of its time, before them."""
    buf = b""
    while len(buf) < n:
        left = end - time.monotonic()
        if left <= 0:
            return None
        conn.settimeout(min(left, 5))
        c = conn.recv(n - len(buf))
        if not c:
            return None
        buf += c
    return buf
