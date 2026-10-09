"""npm's registry: the user owns a package, which it can fetch and publish
again (--answers)."""
import base64
import functools
import hashlib
import io
import json
import tarfile
import urllib.parse

from ..http import Answer
from ..log import printable
from .identity import js

HOSTS = ("registry.npmjs.org", "registry.yarnpkg.com")
REG = "https://registry.npmjs.org/"


def matches(host):
    return host in HOSTS


def answer(me, req):
    method, path, pkg, user = req.method, req.path, me.PKG, me.user
    if method == "GET" and path == "/-/whoami":
        return js("npm whoami", {"username": user})
    if method == "GET" and path == "/-/v1/search":
        return js("npm search", {"total": 1, "objects": [{"package": {
            "name": pkg, "version": "1.0.0", "publisher": {"username": user},
            "maintainers": [{"username": user}], "links": {"npm": "https://www.npmjs.com/package/" + pkg}}}]})
    if method == "GET" and path in ("/-/user/org.couchdb.user:" + user, "/-/npm/v1/user"):
        return js("npm user", {"name": user, "email": user + "@" + me.ORG + ".dev", "tfa": False})
    if method == "GET" and path == "/" + pkg:
        tgz = tarball(pkg)
        meta = {"name": pkg, "version": "1.0.0", "main": "index.js", "license": "MIT",
                "maintainers": [{"name": user}],
                "dist": {"tarball": REG + pkg + "/-/" + pkg + "-1.0.0.tgz",
                         "shasum": hashlib.sha1(tgz).hexdigest(),
                         "integrity": "sha512-" + base64.b64encode(hashlib.sha512(tgz).digest()).decode()}}
        return js("npm package", {"name": pkg, "dist-tags": {"latest": "1.0.0"},
                                  "versions": {"1.0.0": meta}, "maintainers": [{"name": user}]})
    if method == "GET" and path == "/%s/-/%s-1.0.0.tgz" % (pkg, pkg):
        return Answer("npm tarball", "200 OK", "application/octet-stream", tarball(pkg), [])
    if method == "PUT" and not path.startswith("/-/"):
        return js("npm published", {"ok": True, "success": True, "id": printable(urllib.parse.unquote(path[1:]), 100)})
    return None


@functools.lru_cache(maxsize=1)
def tarball(pkg):
    """The package's tarball: a package.json and an index.js, made here."""
    files = {"package/package.json": json.dumps({"name": pkg, "version": "1.0.0", "main": "index.js",
                                                "license": "MIT"}, indent=2).encode(),
             "package/index.js": b"module.exports = (n) => Math.round(n * 100) / 100;\n"}
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as t:
        for name, data in files.items():
            ti = tarfile.TarInfo(name)
            ti.size, ti.mode, ti.mtime = len(data), 0o644, 1704067200
            t.addfile(ti, io.BytesIO(data))
    return buf.getvalue()
