"""What any machine online gets: the generic answer and services/INTERNET."""
import base64
import ipaddress
import json
import unittest

from test_core import Records, exchange, query


def body(out):
    return out.partition(b"\r\n\r\n")[2]


def get(url_path, host, extra=b""):
    return exchange(b"GET " + url_path + b" HTTP/1.1\r\nHost: " + host + b"\r\n" + extra + b"\r\n")


class GenericTest(unittest.TestCase):
    def test_typed_bodies(self):
        out, _ = get(b"/data.json", b"cdn.example")
        self.assertIn(b"Content-Type: application/json", out)
        self.assertEqual(json.loads(body(out)), {})
        out, _ = get(b"/logo.png", b"cdn.example")
        self.assertTrue(body(out).startswith(b"\x89PNG\r\n\x1a\n"))
        out, _ = get(b"/", b"evil.example")
        self.assertIn(b"<html>", body(out))
        out, _ = get(b"/a.tar.gz", b"evil.example")
        self.assertTrue(body(out).startswith(b"\x1f\x8b"))

    def test_head_has_no_body(self):
        out, _ = exchange(b"HEAD /x.json HTTP/1.1\r\nHost: a.example\r\n\r\n")
        self.assertEqual(body(out), b"")
        self.assertIn(b"Content-Length: 2", out)


class ConnectivityTest(unittest.TestCase):
    def test_generate_204(self):
        out, rows = get(b"/generate_204", b"connectivitycheck.gstatic.com")
        self.assertTrue(out.startswith(b"HTTP/1.1 204 No Content\r\n"))
        self.assertNotIn(b"Content-Length", out)
        self.assertEqual(rows[0][7], "connectivity check")

    def test_apple(self):
        out, _ = get(b"/hotspot-detect.html", b"captive.apple.com")
        self.assertIn(b"Success", body(out))


class IPTest(unittest.TestCase):
    def test_text_and_json(self):
        out, rows = get(b"/", b"api.ipify.org")
        ip = body(out).strip().decode()
        self.assertTrue(ip.startswith("73."))
        self.assertEqual(rows[0][7], "ip lookup")
        out, _ = get(b"/?format=json", b"api.ipify.org")
        self.assertEqual(json.loads(body(out))["ip"], ip)
        out, _ = get(b"/json", b"ipinfo.io")
        self.assertEqual(json.loads(body(out))["country"], "US")
        out, _ = get(b"/json/", b"ip-api.com")
        self.assertEqual(json.loads(body(out))["query"], ip)

    def test_trace(self):
        out, _ = get(b"/cdn-cgi/trace", b"www.cloudflare.com")
        self.assertIn(b"\nloc=US\n", body(out))
        out, rows = get(b"/other", b"www.cloudflare.com")
        self.assertEqual(rows[0][7], "-")


class DoHTest(unittest.TestCase):
    def test_json(self):
        out, rows = get(b"/resolve?name=evil.com&type=A", b"dns.google")
        a = json.loads(body(out))
        self.assertTrue(ipaddress.ip_address(a["Answer"][0]["data"]).is_global)
        self.assertIn(("dns", "A", "evil.com"), [r[:3] for r in rows])
        self.assertIn(("addr", a["Answer"][0]["data"], "evil.com"), [r[:3] for r in rows])

    def test_json_nxdomain(self):
        out, _ = get(b"/resolve?name=evil.example", b"dns.google")
        a = json.loads(body(out))
        self.assertEqual(a["Status"], 3)
        self.assertNotIn("Answer", a)
        self.assertEqual(a["Authority"][0]["type"], 6)

    def test_wire(self):
        q = base64.urlsafe_b64encode(query("evil.com")).rstrip(b"=")
        out, rows = get(b"/dns-query?dns=" + q, b"cloudflare-dns.com")
        self.assertIn(b"application/dns-message", out)
        addr = str(ipaddress.ip_address(body(out)[-4:]))
        self.assertIn(("addr", addr, "evil.com"), [r[:3] for r in rows])
        self.assertIn(("dns", "A", "evil.com"), [r[:3] for r in rows])

    def test_garbage(self):
        out, _ = get(b"/dns-query?dns=%%%", b"dns.google")
        self.assertTrue(out.startswith(b"HTTP/1.1 400"))


if __name__ == "__main__":
    unittest.main()
