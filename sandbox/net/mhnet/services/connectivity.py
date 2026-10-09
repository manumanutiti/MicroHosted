"""The checks operating systems and browsers make to know they are online:
answered as the internet does, not as a captive portal would."""
from ..http import Answer

# host → path → (status, content type, body); "" for any path
CHECKS = {
    "connectivitycheck.gstatic.com": {"": ("204 No Content", "", b"")},
    "www.gstatic.com": {"/generate_204": ("204 No Content", "", b"")},
    "clients3.google.com": {"/generate_204": ("204 No Content", "", b"")},
    "connectivitycheck.android.com": {"": ("204 No Content", "", b"")},
    "captive.apple.com": {"": ("200 OK", "text/html",
                               b"<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>\n")},
    "www.msftconnecttest.com": {"/connecttest.txt": ("200 OK", "text/plain", b"Microsoft Connect Test")},
    "www.msftncsi.com": {"/ncsi.txt": ("200 OK", "text/plain", b"Microsoft NCSI")},
    "detectportal.firefox.com": {"": ("200 OK", "text/plain", b"success\n")},
    "nmcheck.gnome.org": {"": ("204 No Content", "", b"")},
    "connectivity-check.ubuntu.com": {"": ("204 No Content", "", b"")},
    "network-test.debian.org": {"": ("204 No Content", "", b"")},
}


def matches(host):
    return host in CHECKS


def answer(ctx, req):
    paths = CHECKS[req.host]
    hit = paths.get(req.path) or paths.get("")
    if hit is None:
        return None
    status, ctype, body = hit
    return Answer("connectivity check", status, ctype, body, [])
