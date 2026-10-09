"""DNS over HTTPS: a name looked up this way is answered by the same
resolver, and written down as a name (dns), not only as a request."""
import base64
import binascii
import json
import urllib.parse

from .. import dns
from ..http import Answer, header
from ..log import printable

HOSTS = {"dns.google", "dns.google.com", "8.8.8.8", "8.8.4.4", "cloudflare-dns.com", "mozilla.cloudflare-dns.com",
         "1.1.1.1", "1.0.0.1", "one.one.one.one", "dns.quad9.net", "9.9.9.9", "doh.opendns.com", "dns.adguard-dns.com"}
CODES = {v: k for k, v in dns.TYPES.items()}


def matches(host):
    return host in HOSTS


def answer(ctx, req):
    q = urllib.parse.parse_qs(req.query)
    if req.path in ("/dns-query", "/resolve") and "dns" not in q and "name" in q:
        return as_json(ctx, q)
    if req.path != "/dns-query":
        return None
    if req.method == "POST" and "dns-message" in header(req.head, "content-type"):
        pkt = req.body[:4096]
    elif "dns" in q:
        try:
            v = q["dns"][0][:6000]
            pkt = base64.urlsafe_b64decode(v + "=" * (-len(v) % 4))
        except (binascii.Error, ValueError):
            return Answer("dns over https", "400 Bad Request", "text/plain", b"Bad Request\n", [])
    else:
        return None
    out = dns.respond(pkt, ctx.log, ctx.names, True)
    if out is None:
        return Answer("dns over https", "400 Bad Request", "text/plain", b"Bad Request\n", [])
    return Answer("dns over https", "200 OK", "application/dns-message", out, [])


def as_json(ctx, q):
    name = printable(q["name"][0].lower().rstrip("."), 253)
    t = q.get("type", ["A"])[0].upper()
    qtype = int(t) if t.isdigit() and int(t) < 65536 else CODES.get(t, 1)
    ctx.log.write("dns", dns.TYPES.get(qtype, str(qtype)), name or ".")
    out = {"Status": 0, "TC": False, "RD": True, "RA": True, "AD": False, "CD": False,
           "Question": [{"name": name + ".", "type": qtype}]}
    if qtype == 1 and dns.HOSTNAME.match(name):
        a = ctx.names.addr(name)
        if a is not None:
            out["Answer"] = [{"name": name + ".", "type": 1, "TTL": 60, "data": str(a)}]
    return Answer("dns over https", "200 OK", "application/dns-json", json.dumps(out).encode(), [])
