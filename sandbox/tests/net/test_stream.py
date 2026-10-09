"""Every port: what a connection speaks, told from its first bytes, answered
as its server would, and written down — with the clients that speak them."""
import ftplib
import imaplib
import ipaddress
import os
import poplib
import smtplib
import socket
import ssl
import tempfile
import threading
import unittest

from test_core import TOK, Records, dispatcher

from mhnet import ca, lines, stream  # noqa: E402
from mhnet.tls import Certs  # noqa: E402

LOCAL = ipaddress.ip_address("127.0.0.1")


class StreamTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        ca.make(cls.tmp.name)
        cls.certs = Certs(cls.tmp.name)
        stream.WAIT = 0.3

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        self.log = Records()
        self.stream = stream.Stream(self.log, TOK, self.certs, dispatcher(self.log))

    def listen(self, port, host="evil.example"):
        """A listener whose one connection is served as port of host's; its
        real port. join() waits for the record."""
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        sock.listen(1)

        def one():
            conn, _ = sock.accept()
            try:
                self.stream.serve(conn, port, host, LOCAL)
            finally:
                conn.close()
                sock.close()
        self.t = threading.Thread(target=one)
        self.t.start()
        return sock.getsockname()[1]

    def tcp(self):
        self.t.join(10)
        return [r for r in self.log.rows if r[0] == "tcp"]

    def client_ctx(self):
        ctx = ssl.create_default_context(cafile=os.path.join(self.tmp.name, "ca.crt"))
        return ctx

    def test_http_on_any_port(self):
        p = self.listen(8080)
        with socket.create_connection(("127.0.0.1", p)) as c:
            c.sendall(b"GET /x.json HTTP/1.1\r\nHost: evil.example:8080\r\n\r\n")
            self.assertIn(b"200", c.recv(100))
        self.t.join(10)
        http = [r for r in self.log.rows if r[0] == "http"]
        self.assertEqual(http[0][1:4], ("http", "GET", "http://evil.example:8080/x.json"))

    def test_https_on_any_port(self):
        p = self.listen(8443)
        with socket.create_connection(("127.0.0.1", p)) as raw:
            with self.client_ctx().wrap_socket(raw, server_hostname="c2.evil.example") as c:
                c.sendall(b"POST /k HTTP/1.1\r\nHost: c2.evil.example:8443\r\nContent-Length: 20\r\n\r\n" + TOK)
                self.assertIn(b"HTTP/1.1", c.recv(100))
        self.t.join(10)
        http = [r for r in self.log.rows if r[0] == "http"]
        self.assertEqual(http[0][1:3], ("https", "POST"))
        self.assertEqual(http[0][5], "1")

    def test_raw_written_down(self):
        p = self.listen(4444, "45.9.148.3")
        with socket.create_connection(("127.0.0.1", p)) as c:
            c.sendall(b"\x01hello " + TOK)
            c.shutdown(socket.SHUT_WR)
            c.recv(1)
        r = self.tcp()[0]
        self.assertEqual(r[1:6], ("4444", "tcp", "45.9.148.3", "27", "1"))
        self.assertTrue(r[6].startswith("\\x01hello 0123"))

    def test_silent_client(self):
        p = self.listen(6379)
        with socket.create_connection(("127.0.0.1", p)):
            pass
        self.assertEqual(self.tcp()[0][1:6], ("6379", "tcp", "evil.example", "0", "0"))

    def test_tls_not_http(self):
        p = self.listen(9999)
        with socket.create_connection(("127.0.0.1", p)) as raw:
            with self.client_ctx().wrap_socket(raw, server_hostname="evil.example") as c:
                c.sendall(b"beacon " + TOK)
        self.assertEqual(self.tcp()[0][1:3], ("9999", "tls"))
        self.assertEqual(self.tcp()[0][5], "1")

    def test_smtp_starttls_and_a_message(self):
        p = self.listen(587, "smtp.gmail.com")
        with smtplib.SMTP("127.0.0.1", p, "client.local", timeout=5) as s:
            s._host = "smtp.gmail.com"  # the name it connected to, for the certificate
            s.starttls(context=self.client_ctx())
            s.login("x@gmail.com", "app-password")
            s.sendmail("x@gmail.com", ["drop@evil.example"], b"Subject: loot\r\n\r\naws_secret=" + TOK)
        r = self.tcp()[0]
        self.assertEqual(r[1:3], ("587", "smtp"))
        self.assertEqual(r[5], "1")

    def test_smtps(self):
        p = self.listen(465, "smtp.evil.example")
        ctx = self.client_ctx()
        ctx.check_hostname = False  # smtplib asks for 127.0.0.1's; the chain is still checked
        with smtplib.SMTP_SSL("127.0.0.1", p, "client.local", timeout=5, context=ctx) as s:
            s.ehlo()
            s.sendmail("a@b.c", ["d@e.f"], b"\r\n" + TOK)
        r = self.tcp()[0]
        self.assertEqual(r[1:3], ("465", "smtps"))
        self.assertEqual(r[5], "1")

    def test_ftp_login_and_pasv(self):
        p = self.listen(21)
        f = ftplib.FTP()
        f.connect("127.0.0.1", p, timeout=5)
        f.login("anonymous", TOK.decode())
        self.assertTrue(f.sendcmd("PASV").startswith("227 Entering Passive Mode (127,0,0,1,"))
        f.quit()
        r = self.tcp()[0]
        self.assertEqual(r[1:3], ("21", "ftp"))
        self.assertEqual(r[5], "1")

    def test_imap_append(self):
        p = self.listen(143)
        m = imaplib.IMAP4("127.0.0.1", p, timeout=5)
        m.login("x", "y")
        m.append("INBOX", None, None, b"Subject: k\r\n\r\n" + TOK)
        m.logout()
        r = self.tcp()[0]
        self.assertEqual(r[1:3], ("143", "imap"))
        self.assertEqual(r[5], "1")

    def test_pop3_login(self):
        p = self.listen(110)
        m = poplib.POP3("127.0.0.1", p, timeout=5)
        m.user("x")
        m.pass_(TOK.decode())
        self.assertEqual(m.stat(), (0, 0))
        m.quit()
        self.assertEqual(self.tcp()[0][5], "1")

    def test_ssh_greeted(self):
        p = self.listen(22)
        with socket.create_connection(("127.0.0.1", p)) as c:
            self.assertTrue(c.recv(100).startswith(b"SSH-2.0-OpenSSH"))
            c.sendall(b"SSH-2.0-paramiko_3.4.0\r\n")
        self.assertTrue(self.tcp()[0][6].startswith("SSH-2.0-paramiko"))


class SessionTest(unittest.TestCase):
    def test_long_line_cut(self):
        a, b = socket.socketpair()
        a.sendall(b"x" * (lines.MAXLINE * 3) + b"\nend\n")
        a.close()
        s = lines.Session(b, float("inf"), 1 << 20)
        self.assertEqual(len(s.line()), lines.MAXLINE)
        n = 1
        while s.line() != b"end":
            n += 1
        self.assertEqual(n, 4)
        b.close()

    def test_kept_bounded(self):
        a, b = socket.socketpair()
        threading.Thread(target=lambda: (a.sendall(b"y" * 300000), a.close())).start()
        s = lines.Session(b, float("inf"), 1000)
        s.drain()
        self.assertEqual((len(s.kept), s.total), (1000, 300000))
        b.close()
