"""The run's certificates: a client that trusts the root accepts them, and
none of them is dated at the run."""
import datetime
import os
import socket
import ssl
import tempfile
import threading
import unittest

from cryptography import x509

from test_core import Records

from mhnet import ca  # noqa: E402
from mhnet.tls import Certs  # noqa: E402


def days_ago(t):
    return (datetime.datetime.now(datetime.timezone.utc) - t).total_seconds() / 86400


class TLSTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.d = self.tmp.name
        ca.make(self.d)
        self.certs = Certs(self.d)

    def tearDown(self):
        self.tmp.cleanup()

    def handshake(self, sni):
        """A client trusting only the run's root, checking the name, as a
        library does: the chain it was sent."""
        a, b = socket.socketpair()
        got = {}
        t = threading.Thread(target=lambda: got.setdefault("w", self.certs.wrap(b, "1.2.3.4", Records())))
        t.start()
        ctx = ssl.create_default_context(cafile=os.path.join(self.d, "ca.crt"))
        with ctx.wrap_socket(a, server_hostname=sni) as c:
            chain = c.get_verified_chain() if hasattr(c, "get_verified_chain") else None
            leaf = x509.load_der_x509_certificate(c.getpeercert(binary_form=True))
        t.join()
        self.assertEqual(got["w"][1], sni)
        got["w"][0].close()
        b.close()
        return leaf, chain

    def test_trusted_and_dated_as_a_real_one(self):
        leaf, chain = self.handshake("evil.com")
        self.assertEqual(leaf.subject.rfc4514_string(), "CN=evil.com")
        self.assertIn("Acme Corp TLS Inspection CA", leaf.issuer.rfc4514_string())
        self.assertTrue(1 <= days_ago(leaf.not_valid_before_utc) <= 60)
        self.assertEqual(leaf.not_valid_after_utc - leaf.not_valid_before_utc, datetime.timedelta(days=90))
        self.assertGreater(leaf.serial_number.bit_length(), 64)
        if chain is not None:  # Python 3.13: root, intermediate and leaf
            self.assertEqual(len(chain), 3)

    def test_ca_made_years_ago(self):
        with open(os.path.join(self.d, "ca.crt"), "rb") as f:
            root = x509.load_pem_x509_certificate(f.read())
        with open(os.path.join(self.d, "inter.crt"), "rb") as f:
            inter = x509.load_pem_x509_certificate(f.read())
        self.assertGreater(days_ago(root.not_valid_before_utc), 3 * 365)
        self.assertGreater(days_ago(inter.not_valid_before_utc), 365)
        self.assertGreater(root.not_valid_after_utc, inter.not_valid_after_utc)
        self.assertFalse(os.path.exists(os.path.join(self.d, "ca.key")))
        self.assertEqual(os.stat(os.path.join(self.d, "inter.key")).st_mode & 0o777, 0o600)

    def test_the_name_asked(self):
        # SNI picks the name: a client asking for one gets that one
        leaf, _ = self.handshake("api.github.com")
        san = leaf.extensions.get_extension_for_class(x509.SubjectAlternativeName).value
        self.assertEqual(san.get_values_for_type(x509.DNSName), ["api.github.com"])


if __name__ == "__main__":
    unittest.main()
