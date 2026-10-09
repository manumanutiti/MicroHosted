"""Each name its own address, in the order asked, and back."""
import ipaddress
import threading

SINK = ipaddress.ip_network("198.18.0.0/15")
MAXNAMES = 20000  # a loop over made-up names: the rest get no address


class Names:
    def __init__(self, log):
        self.log = log
        self.by_name, self.by_addr = {}, {}
        self.lock = threading.Lock()

    def addr(self, name):
        with self.lock:
            a = self.by_name.get(name)
            if a is None:
                if len(self.by_name) >= MAXNAMES:
                    return None
                a = SINK.network_address + 1 + len(self.by_name)
                self.by_name[name], self.by_addr[a] = a, name
                self.log.write("addr", a, name)
            return a

    def name(self, addr):
        with self.lock:
            return self.by_addr.get(ipaddress.ip_address(addr))
