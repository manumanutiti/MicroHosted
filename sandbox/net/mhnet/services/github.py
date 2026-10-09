"""GitHub's API, as a token with repo and workflow scopes sees it (--answers)."""
import re

from .identity import field, js

HOSTS = ("api.github.com",)


def matches(host):
    return host in HOSTS


def answer(me, req):
    scopes = [("X-OAuth-Scopes", "repo, workflow, read:org, write:packages"),
              ("X-GitHub-Media-Type", "github.v3; format=json")]
    method, path, body = req.method, req.path, req.body

    def repo(owner, name, private=True):
        return {"id": int(me.sha(owner, name)[:7], 16), "name": name, "full_name": owner + "/" + name,
                "owner": {"login": owner, "type": "Organization" if owner == me.ORG else "User"},
                "private": private, "default_branch": "main",
                "html_url": "https://github.com/%s/%s" % (owner, name),
                "clone_url": "https://github.com/%s/%s.git" % (owner, name),
                "permissions": {"admin": True, "maintain": True, "push": True, "triage": True, "pull": True}}

    if method == "GET" and path == "/user":
        return js("github user", {"login": me.user, "id": int(me.sha("user")[:7], 16), "type": "User",
                                  "name": me.user, "company": "@" + me.ORG, "public_repos": 3,
                                  "total_private_repos": 1, "two_factor_authentication": False}, extra=scopes)
    if method == "GET" and path in ("/user/repos", "/orgs/%s/repos" % me.ORG):
        return js("github repositories", [repo(me.ORG, me.REPO)], extra=scopes)
    if method == "GET" and path == "/user/orgs":
        return js("github orgs", [{"login": me.ORG, "id": int(me.sha("org")[:7], 16)}], extra=scopes)
    if method == "POST" and path in ("/user/repos", "/orgs/%s/repos" % me.ORG):
        owner = me.user if path == "/user/repos" else me.ORG
        name = field(body, "name", "repository")
        return js("github repository created", repo(owner, name, private=False), "201 Created", scopes)
    m = re.match(r"^/repos/([^/]+)/([^/]+)(/.*)?$", path)
    if not m:
        return None
    owner, name, rest = m.group(1), m.group(2), m.group(3) or ""
    if owner not in (me.ORG, me.user):  # another's: the generic answer
        return None
    if method == "GET" and rest == "":
        return js("github repository", repo(owner, name), extra=scopes)
    if method == "GET" and re.match(r"^/git/refs?/heads/", rest):
        ref = "refs/heads/" + rest.split("/heads/", 1)[1]
        return js("github ref", {"ref": ref, "object": {"sha": me.sha(owner, name, ref), "type": "commit"}}, extra=scopes)
    if method == "POST" and rest == "/git/refs":
        ref = field(body, "ref", "refs/heads/main")
        return js("github ref created", {"ref": ref, "object": {"sha": me.sha(owner, name, ref), "type": "commit"}},
                  "201 Created", scopes)
    if method == "PUT" and rest.startswith("/contents/"):
        p = rest[len("/contents/"):]
        return js("github file written", {"content": {"name": p.rsplit("/", 1)[-1], "path": p, "sha": me.sha(p)},
                                          "commit": {"sha": me.sha(owner, name, p, "commit")}}, "201 Created", scopes)
    if method == "POST" and rest == "/actions/runners/registration-token":
        return js("github runner token", {"token": "A" + me.TOK + me.TOK, "expires_at": "2099-01-01T00:00:00Z"},
                  "201 Created", scopes)
    if method == "GET" and rest in ("/actions/secrets", "/actions/variables"):
        return js("github secrets", {"total_count": 0, rest.rsplit("/", 1)[1]: []}, extra=scopes)
    return None
