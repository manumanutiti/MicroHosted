"""mh-sandbox-net DIR sinkhole|answers USER|servfail < TOKEN

mh-sandbox-prepare starts it as root. It binds its ports, reads this run's
decoy token on stdin and DIR's CA, then becomes DIR's owner (mhsink): what
it parses is the code's to choose, and a mistake here must not hand the
code root.

The resolver, on 127.53.0.1:53, UDP and TCP (resolv.conf points at it;
mh-sandbox-prepare sends any other resolver's queries here too), writes
down every name looked up. sinkhole: an A query is answered with an
address of its own for each name, one that looks public (names.py), in
/24s the VM holds locally (a local route each, added here), so a
connection to it never reaches the host; any other type has no answer; a
name under a TLD that does not exist is NXDOMAIN. servfail: no name is
answered, as with no network at all.

The sinkhole, on ports 80 and 443 of those addresses, and of any bare
address the code connects to (mh-sandbox-prepare redirects them here; the
address it meant is read back, SO_ORIGINAL_DST): an HTTP request is
answered as the internet would (services/: connectivity checks, what is my
address, DNS over HTTPS; content.py: a small valid file of the type asked
for, with the headers a server sends) and written down (log.py). answers: the services worms
go for are answered as for USER, logged in (services/identity.py). HTTPS
is answered with a certificate for the name, signed by DIR's CA (tls.py).
"""
import ipaddress
import os
import socket
import subprocess
import sys
import threading

from . import dns, http, services
from .log import Log
from .names import Names, pick
from .tls import Certs

RESOLVER = "127.53.0.1"
SLOTS = 64            # connections served at once; the rest are closed
HARD_DEADLINE = 30    # seconds a connection may last, whatever it does
SO_ORIGINAL_DST = 80  # linux/netfilter_ipv4.h


def original_dst(conn, names):
    """Where a connection mh-sandbox-prepare's REDIRECT sent here was going:
    a bare address the code connected to, or None (not redirected)."""
    try:
        raw = conn.getsockopt(socket.SOL_IP, SO_ORIGINAL_DST, 16)
    except OSError:
        return None
    a = ipaddress.ip_address(raw[4:8])
    return None if a.is_loopback or names.owns(a) else a


def route(blocks):
    """Each block the VM's own, as root: a local route, in one ip."""
    ip = next((p for p in ("/usr/sbin/ip", "/sbin/ip", "/bin/ip") if os.path.exists(p)), None)
    batch = "".join("route add local %s dev lo\n" % b for b in blocks)
    if ip is None or subprocess.run([ip, "-batch", "-"], input=batch.encode(), stdout=subprocess.DEVNULL).returncode:
        # without them, a connection to a name's address would leave the VM
        sys.exit("mh-sandbox-net: the names' addresses could not be routed locally")


def cut(conn):
    try:
        conn.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass


class Sinkhole:
    def __init__(self, log, names, token, certs, answer):
        self.log, self.names, self.token, self.certs, self.answer = log, names, token, certs, answer
        self.slots = threading.BoundedSemaphore(SLOTS)

    def accept(self, sock, port):
        while True:
            try:
                conn, _ = sock.accept()
            except InterruptedError:
                continue
            local = ipaddress.ip_address(conn.getsockname()[0])
            # a name's address, by its name; a bare address, as itself
            host = (self.names.name(local) or local) if self.names.owns(local) else original_dst(conn, self.names)
            if host is None or not self.slots.acquire(blocking=False):
                conn.close()
                continue
            threading.Thread(target=self.handle, args=(conn, port, str(host)), daemon=True).start()

    def handle(self, conn, port, host):
        # a hard end to whatever the code makes of it: a handshake dripped a
        # byte at a time cannot hold a slot for the run
        timer = threading.Timer(HARD_DEADLINE, cut, (conn,))
        timer.daemon = True
        timer.start()
        try:
            conn.settimeout(5)
            if port != 443:
                http.serve(conn, "http", port, host, self.log, self.token, self.answer)
                return
            w = self.certs.wrap(conn, host, self.log)
            if w is not None:
                http.serve(w[0], "https", port, w[1], self.log, self.token, self.answer)
        finally:
            timer.cancel()
            conn.close()
            self.slots.release()


def main(argv):
    d, mode = argv[1], argv[2]
    token = sys.stdin.read().strip().lower().encode()
    user = argv[3] if mode == "answers" else None
    if user:
        mode = "sinkhole"
    blocks = pick()
    if mode == "sinkhole":
        route(blocks)
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp.bind((RESOLVER, 53))
    dns_tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    dns_tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    dns_tcp.bind((RESOLVER, 53))
    dns_tcp.listen(32)
    tcp = []
    if mode == "sinkhole":
        for port in (80, 443):
            t = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            t.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            t.bind(("0.0.0.0", port))
            t.listen(128)
            tcp.append((t, port))
    # root no longer: DIR's owner
    st = os.stat(d)
    if os.getuid() == 0:
        os.setgroups([])
        os.setgid(st.st_gid)
        os.setuid(st.st_uid)
    if os.getuid() == 0:
        sys.exit("mh-sandbox-net: DIR must not be root's")

    log = Log(os.path.join(d, "log"))
    names = Names(log, blocks)
    if mode == "sinkhole":
        sink = Sinkhole(log, names, token, Certs(d), services.Dispatcher(services.Context(token, names, log, user)))
        for t, port in tcp:
            threading.Thread(target=sink.accept, args=(t, port), daemon=True).start()
    threading.Thread(target=dns.serve_tcp, args=(dns_tcp, log, names, mode == "sinkhole"), daemon=True).start()
    dns.serve_udp(udp, log, names, mode == "sinkhole")
