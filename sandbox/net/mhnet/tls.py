"""A certificate for each name the code asks for, signed by DIR's CA, which
the VM trusts (mh-sandbox-prepare)."""
import ipaddress
import os
import re
import ssl
import subprocess
import threading

from .dns import HOSTNAME
from .log import printable

MAXCERTS = 1000  # names past it get the default certificate: a loop of made-up names costs no openssl run each
DEFAULT = "sandbox.invalid"


def is_ip(s):
    try:
        ipaddress.ip_address(s)
        return True
    except ValueError:
        return False


class Certs:
    def __init__(self, d):
        self.d = d
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
        ext = base + ".ext"
        with open(ext, "w") as f:
            f.write("subjectAltName=%s:%s\nextendedKeyUsage=serverAuth\n" % ("IP" if is_ip(name) else "DNS", name))
        subprocess.run(["openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1",
                        "-nodes", "-keyout", base + ".key", "-subj", "/CN=" + name, "-out", base + ".csr"],
                       check=True, capture_output=True, timeout=20)
        subprocess.run(["openssl", "x509", "-req", "-in", base + ".csr", "-CA", os.path.join(self.d, "ca.crt"),
                        "-CAkey", os.path.join(self.d, "ca.key"), "-set_serial", str(int.from_bytes(os.urandom(8), "big")),
                        "-days", "30", "-extfile", ext, "-out", base + ".crt"],
                       check=True, capture_output=True, timeout=20)

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
