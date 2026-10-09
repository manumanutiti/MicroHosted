"""A certificate for each name the code asks for, signed by DIR's
intermediate, under the root the VM trusts (ca.py, mh-sandbox-prepare)."""
import os
import re
import ssl
import threading

from .ca import Issuer, write
from .dns import HOSTNAME
from .log import printable

MAXCERTS = 1000  # names past it get the default certificate: a loop of made-up names costs no key each
DEFAULT = "sandbox.invalid"


class Certs:
    def __init__(self, d):
        self.issuer = Issuer(d)
        self.dir = os.path.join(d, "certs")
        os.makedirs(self.dir, exist_ok=True)
        self.ctxs = {}
        self.lock = threading.Lock()
        self.default = self.ctx(DEFAULT)

    def ctx(self, name):
        with self.lock:
            c = self.ctxs.get(name)
            if c is not None:
                return c
            if len(self.ctxs) >= MAXCERTS:
                return self.default
            base = os.path.join(self.dir, re.sub(r"[^a-z0-9.-]", "_", name))
            self.issue(name, base)
            c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            c.load_cert_chain(base + ".crt", base + ".key")
            self.ctxs[name] = c
            return c

    def issue(self, name, base):
        key, chain = self.issuer.issue(name)
        write(base + ".key", key)
        write(base + ".crt", chain)

    def wrap(self, conn, host, log):
        """conn as TLS's server, with the certificate for the name it asks
        for (SNI), or host's: (the socket, the name), or None when the
        client refused it — written down, the name only."""
        asked = {"name": host}

        def sni(sslobj, server_name, _ctx):
            if server_name and HOSTNAME.match(server_name.lower()):
                asked["name"] = server_name.lower()
            try:
                sslobj.context = self.ctx(asked["name"])
            except Exception:
                return ssl.ALERT_DESCRIPTION_INTERNAL_ERROR
            return None

        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.sni_callback = sni
        ctx.load_cert_chain(os.path.join(self.dir, DEFAULT + ".crt"), os.path.join(self.dir, DEFAULT + ".key"))
        try:
            return ctx.wrap_socket(conn, server_side=True), asked["name"]
        except (ssl.SSLError, OSError):
            log.write("tls", printable(asked["name"], 253), "refused")
            return None
