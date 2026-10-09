"""What answers a request: a module per service, each with matches(host)
and answer(ctx, req) → an Answer, or None for what it does not know (the
next one, then the generic answer, content.py, takes it). Adding a service
is adding a module here and to its list below.

IDENTITY answer as for a user logged in (--answers), called with the
Identity: they make code go further with credentials, so only where asked.
INTERNET are what any machine online gets, always."""
from ..content import generic
from . import aws, connectivity, doh, github, ipinfo, npm, oidc
from .identity import Identity

IDENTITY = (github, npm, aws, oidc)
INTERNET = (connectivity, doh, ipinfo)


class Context:
    def __init__(self, token, names, log, user=None):
        self.token, self.names, self.log = token, names, log
        self.me = Identity(user, token) if user else None


def _try(svc, arg, req):
    try:
        return svc.answer(arg, req)
    except Exception:  # what the code sent is the code's: the next answer then
        return None


class Dispatcher:
    def __init__(self, ctx):
        self.ctx = ctx

    def __call__(self, req):
        if self.ctx.me is not None:
            for svc in IDENTITY:
                if svc.matches(req.host):
                    a = _try(svc, self.ctx.me, req)
                    if a is not None:
                        return a
        for svc in INTERNET:
            if svc.matches(req.host):
                a = _try(svc, self.ctx, req)
                if a is not None:
                    return a
        return generic(req)
