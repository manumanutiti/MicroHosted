"""GitHub Actions' OIDC token, for --ci's ACTIONS_ID_TOKEN_REQUEST_URL
(--answers)."""
import base64
import json

from .identity import js

SUFFIX = ".actions.githubusercontent.com"


def matches(host):
    return host.endswith(SUFFIX)


def answer(me, req):
    if "idtoken" not in req.path:
        return None

    def b64(o):
        return base64.urlsafe_b64encode(json.dumps(o).encode()).decode().rstrip("=")

    claims = {"iss": "https://token.actions.githubusercontent.com", "aud": "sigstore",
              "sub": "repo:%s/%s:ref:refs/heads/main" % (me.ORG, me.REPO), "repository": me.ORG + "/" + me.REPO,
              "jti": me.tok, "exp": 4102444800}
    return js("github oidc token", {"value": b64({"alg": "RS256", "typ": "JWT"}) + "." + b64(claims) + "." + me.tok + me.TOK})
