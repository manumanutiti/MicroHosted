"""Whether what the code sent carries this run's decoy token: a secret sent
out — or only as its own service's credential, a tool logged in."""
import base64
import binascii
import re
import urllib.parse
import zlib

# What one request may cost to look through: every layer decoded counts its
# bytes and itself. A body made to branch (runs of base64 in base64 in gzip)
# stops here instead of holding the sinkhole.
BUDGET_BYTES = 32 << 20
BUDGET_LAYERS = 400
MAXLAYER = 4 << 20  # a gzip layer decompressed at most to this

B64RUN = re.compile(rb"[A-Za-z0-9+/_-]{24,}={0,2}")


class _Budget:
    def __init__(self):
        self.bytes, self.layers = BUDGET_BYTES, BUDGET_LAYERS

    def spend(self, d):
        self.bytes -= len(d)
        self.layers -= 1
        return self.bytes >= 0 and self.layers >= 0


def carries(data, token, depth=0, budget=None):
    """Whether data has the token in it: as is, or under up to three layers
    of URL-encoding, base64 and gzip, in any order (gzip of base64 of a
    file is a stealer's usual)."""
    if not token:
        return False
    budget = budget or _Budget()
    if token in data.lower():
        return True
    if depth == 3:
        return False
    for d in _layers(data):
        if d == data or not budget.spend(d):
            continue
        if carries(d, token, depth + 1, budget):
            return True
        if budget.layers < 0 or budget.bytes < 0:
            return False
    return False


def _layers(data):
    """What data decodes to, one layer down, lazily: cheapest first."""
    if b"%" in data:
        try:
            yield urllib.parse.unquote_to_bytes(data)
        except Exception:
            pass
    for run in B64RUN.findall(data)[:50]:
        for dec in (base64.b64decode, base64.urlsafe_b64decode):
            try:
                yield dec(run + b"=" * (-len(run) % 4))
                break
            except (binascii.Error, ValueError):
                pass
    i = data.find(b"\x1f\x8b")
    if i >= 0:
        try:
            yield zlib.decompressobj(31).decompress(data[i:], MAXLAYER)
        except zlib.error:
            pass


# Where a decoy's credential goes as it should: to its own service, in the
# header that service reads (a tool logged in, npm whoami with NPM_TOKEN).
# By host: the headers dropped before looking for the token again, and
# whether the request line goes too (the OIDC token's URL is a secret).
# The first that matches.
OWN = [(re.compile(r"^pipelines\.actions\.githubusercontent\.com$"), ("authorization",), True),
       (re.compile(r"^(api\.github\.com|github\.com|uploads\.github\.com|ghcr\.io|[a-z0-9.-]+\.githubusercontent\.com)$"), ("authorization",), False),
       (re.compile(r"^registry\.(npmjs\.org|yarnpkg\.com)$"), ("authorization", "npm-auth-type"), False),
       (re.compile(r"^(upload\.pypi\.org|test\.pypi\.org)$"), ("authorization",), False),
       (re.compile(r"^[a-z0-9.-]+\.amazonaws\.com$"), ("authorization", "x-amz-security-token"), False)]


def token_field(host, head, body, token):
    """0: the token is not in the request; 1: it is — a secret sent out;
    2: only where its own service reads it (OWN): a credential used, not
    sent out. Anywhere else in the request — the body, the query, another
    header — is 1."""
    if not carries(head + b"\r\n\r\n" + body, token):
        return 0
    host = host.lower().split(":")[0]
    for pat, headers, line in OWN:
        if pat.match(host):
            lines = head.split(b"\r\n")
            rest = [l for i, l in enumerate(lines)
                    if not (i == 0 and line) and l.split(b":", 1)[0].strip().lower().decode("latin-1") not in headers]
            return 2 if not carries(b"\r\n".join(rest) + b"\r\n\r\n" + body, token) else 1
    return 1
