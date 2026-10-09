"""The resolver: every name looked up written down, and answered."""
import re

from .log import printable

TYPES = {1: "A", 2: "NS", 5: "CNAME", 6: "SOA", 12: "PTR", 15: "MX", 16: "TXT",
         28: "AAAA", 33: "SRV", 64: "SVCB", 65: "HTTPS", 255: "ANY"}
HOSTNAME = re.compile(r"^[a-z0-9_]([a-z0-9_-]{0,62}\.)*[a-z0-9_-]{1,63}\.?$")


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
    answer = b""
    rcode = 0 if sinkhole else 2  # servfail: as with no network
    if sinkhole and qtype == 1 and HOSTNAME.match(name):
        a = names.addr(name)
        if a is not None:
            # the name by pointer to the question, A, IN, 60 s, the address
            answer = b"\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x3c\x00\x04" + a.packed
    # the id, the opcode and RD as asked; QR, AA, RA set
    flags = bytes([0x84 | (pkt[2] & 0x79), 0x80 | rcode])
    counts = b"\x00\x01" + (b"\x00\x01" if answer else b"\x00\x00") + b"\x00\x00\x00\x00"
    return pkt[0:2] + flags + counts + pkt[12:end] + answer


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
