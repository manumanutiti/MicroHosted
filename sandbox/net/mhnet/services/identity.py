"""--answers: an identity made up for this run — the VM's user, in an
organization with one private repository, owner of an npm package, with
keys of an AWS account that keeps one secret — as GitHub, npm, AWS and
Actions' OIDC would answer for it, instead of the generic answer, on which
worms stop: after whoami, after creating a repository. What they do next
shows. Every secret handed out (an OIDC token, a runner's registration
token, a secret's value) has this run's token in it, so sending it on is a
secret sent out, as a decoy's. Nothing in an answer is the code's choice
but the names it echoes, made printable and cut."""
import hashlib
import json

from ..http import Answer
from ..log import printable


class Identity:
    ORG, REPO, PKG = "acme", "billing-api", "acme-billing-utils"

    def __init__(self, user, token):
        self.user = user
        self.tok = token.decode()
        self.TOK = self.tok.upper()
        self.account = "%012d" % (int(self.tok[:10] or "0", 16) % 10 ** 12)

    def sha(self, *words):
        return hashlib.sha1(("\0".join((self.tok,) + words)).encode()).hexdigest()


def js(name, obj, status="200 OK", extra=()):
    return Answer(name, status, "application/json; charset=utf-8", json.dumps(obj).encode(), list(extra))


def field(body, key, default):
    """A string field of a JSON body, made printable and cut, or default."""
    try:
        v = json.loads(body.decode("utf-8")).get(key)
    except (ValueError, AttributeError, UnicodeError):
        return default
    return printable(v, 100) if isinstance(v, str) and v else default
