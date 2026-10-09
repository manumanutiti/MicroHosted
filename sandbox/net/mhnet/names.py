"""Each name its own address, in the order asked, and back.

The addresses look like the internet's: spread over BLOCKS /24s of public
space picked at random for the run (pick), each held by the VM itself (a
local route, main), so a connection to one never reaches the host. Not a
reserved range such as 198.18.0.0/15, which only a sandbox hands out."""
import hashlib
import ipaddress
import random
import threading

BLOCKS = 96       # /24s a run: 254 addresses each
MAXNAMES = 20000  # a loop over made-up names: the rest get no address
# not handed out: where a well-known address is (resolvers, the cloud's
# metadata service), and the made-up home connection (services/ipinfo.py)
AVOID = [ipaddress.ip_network(n) for n in ("1.0.0.0/8", "8.8.0.0/16", "9.9.9.0/24", "73.0.0.0/8",
                                             "208.67.0.0/16", "94.140.0.0/16")]


def pick(n=BLOCKS, rng=None):
    """n /24s of public space, none twice, none in AVOID."""
    rng = rng or random.SystemRandom()
    out = set()
    while len(out) < n:
        net = ipaddress.ip_network((rng.randrange(1 << 24) << 8, 24))
        if net.network_address.is_global and not net.is_multicast and not any(net.subnet_of(a) for a in AVOID):
            out.add(net)
    return sorted(out)


class Names:
    def __init__(self, log, blocks=None):
        self.log = log
        self.blocks = blocks if blocks is not None else pick()
        self.prefixes = {int(b.network_address) >> 8 for b in self.blocks}
        self.salt = random.SystemRandom().getrandbits(64).to_bytes(8, "big")
        self.free = {}  # block → its unused hosts, in a shuffled order
        self.by_name, self.by_addr = {}, {}
        self.lock = threading.Lock()

    def owns(self, addr):
        """addr is one of the run's: its connections are the sinkhole's."""
        a = ipaddress.ip_address(addr)
        return a.version == 4 and int(a) >> 8 in self.prefixes

    def addr(self, name):
        with self.lock:
            a = self.by_name.get(name)
            if a is None:
                if len(self.by_name) >= MAXNAMES:
                    return None
                a = self.allot(name)
                self.by_name[name], self.by_addr[a] = a, name
                self.log.write("addr", a, name)
            return a

    def allot(self, name):
        # the name's block, by a hash salted for the run; the next one when full
        i = int.from_bytes(hashlib.sha256(self.salt + name.encode()).digest()[:4], "big")
        for k in range(len(self.blocks)):
            b = self.blocks[(i + k) % len(self.blocks)]
            free = self.free.get(b)
            if free is None:
                free = self.free[b] = list(range(1, 255))
                random.Random(self.salt + b.network_address.packed).shuffle(free)
            if free:
                return b.network_address + free.pop()
        raise RuntimeError("names: every block full")  # MAXNAMES < BLOCKS * 254

    def name(self, addr):
        with self.lock:
            return self.by_addr.get(ipaddress.ip_address(addr))
