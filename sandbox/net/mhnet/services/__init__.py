"""What answers a request: a module per service, each with matches(host)
and answer(me, req) → an Answer, or None for what it does not know (the
next one, then the generic answer, takes it). Adding a service is adding a
module here and to its list below.

IDENTITY answer as for a user logged in (--answers): they make code go
further with credentials, so only where asked."""
from ..http import Answer
from . import aws, github, npm, oidc
from .identity import Identity

IDENTITY = (github, npm, aws, oidc)

EMPTY = Answer("", "200 OK", "", b"", [])


class Dispatcher:
    def __init__(self, user=None, token=b""):
        self.me = Identity(user, token) if user else None

    def __call__(self, req):
        if self.me is not None:
            for svc in IDENTITY:
                if svc.matches(req.host):
                    try:
                        a = svc.answer(self.me, req)
                    except Exception:  # what the code sent is the code's: the generic answer then
                        a = None
                    if a is not None:
                        return a
                    break
        return EMPTY
