"""The protocols whose server speaks first, as far as a client needs them
to get to what it sends: SMTP to its message (STARTTLS and AUTH included),
FTP to its file (its data connection is one more connection to the
sinkhole, PASV's address its own), POP3 and IMAP to their login. SSH: its
greeting, then what the client sends, unanswered (stream.py).

Each reads a line at a time, within bounds: a line is cut at MAXLINE, the
whole conversation at its deadline, and what is kept of it at max."""
import random
import socket
import time

MAXLINE = 8192
IDLE = 15  # seconds a client may think between commands (resolving its own name…)

GREETING = {
    "smtp": b"220 %s ESMTP Postfix (Ubuntu)\r\n",
    "ftp": b"220 (vsFTPd 3.0.5)\r\n",
    "pop3": b"+OK Dovecot (Ubuntu) ready.\r\n",
    "imap": b"* OK [CAPABILITY IMAP4rev1 SASL-IR LOGIN-REFERRALS ID ENABLE IDLE LITERAL+ AUTH=PLAIN AUTH=LOGIN] Dovecot (Ubuntu) ready.\r\n",
    "ssh": b"SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13.5\r\n",
}


class Session:
    """A connection's client side, read: kept (up to max) and its size."""

    def __init__(self, conn, deadline, maximum, first=b"", idle=IDLE):
        self.conn, self.deadline, self.max, self.idle = conn, deadline, maximum, idle
        self.buf, self.kept, self.total = first, first[:maximum], len(first)

    def recv(self):
        left = self.deadline - time.monotonic()
        if left <= 0:
            return b""
        self.conn.settimeout(min(self.idle, left))
        try:
            c = self.conn.recv(65536)
        except (socket.timeout, OSError):  # ssl's errors are OSError's
            return b""
        self.total += len(c)
        self.kept += c[:max(0, self.max - len(self.kept))]
        return c

    def line(self):
        """The next line, without its end; None when the client is done."""
        while b"\n" not in self.buf and len(self.buf) < MAXLINE:
            c = self.recv()
            if not c:
                line, self.buf = self.buf, b""
                return line or None
            self.buf += c
        line, nl, rest = self.buf[:MAXLINE].partition(b"\n")
        self.buf = rest + self.buf[MAXLINE:] if nl else self.buf[MAXLINE:]
        return line.rstrip(b"\r")

    def skip(self, n):
        """n bytes read past (an IMAP literal), kept as any; never held."""
        while n > 0:
            if not self.buf:
                self.buf = self.recv()
                if not self.buf:
                    return
            take = min(n, len(self.buf))
            self.buf, n = self.buf[take:], n - take

    def drain(self):
        """The rest, until the client stops."""
        self.buf = b""
        while self.recv():
            pass

    def send(self, data):
        self.conn.sendall(data)

    def verb(self):
        """The next line's command, upper case, and its argument."""
        line = self.line()
        if line is None:
            return None, b""
        verb, _, arg = line.partition(b" ")
        return verb.upper(), arg


def ssh(s, host, addr, starttls):
    s.send(GREETING["ssh"])
    s.drain()


def raw(s, host, addr, starttls):
    s.idle = 5  # nothing asked of it: silence is the end
    s.drain()


EHLO = [b"PIPELINING", b"SIZE 10240000", b"ETRN", b"AUTH PLAIN LOGIN", b"ENHANCEDSTATUSCODES", b"8BITMIME", b"DSN", b"SMTPUTF8"]


def smtp(s, host, addr, starttls):
    name = host.encode() if not host[:1].isdigit() else b"mail"
    s.send(GREETING["smtp"] % name)
    tls = starttls is None  # SMTPS: TLS already
    while True:
        verb, arg = s.verb()
        if verb is None:
            return
        if verb == b"EHLO":
            caps = EHLO if tls else EHLO[:1] + [b"STARTTLS"] + EHLO[1:]
            s.send(b"".join(b"250-%s\r\n" % c for c in [name] + caps[:-1]) + b"250 " + caps[-1] + b"\r\n")
        elif verb == b"HELO":
            s.send(b"250 " + name + b"\r\n")
        elif verb == b"STARTTLS" and not tls:
            s.send(b"220 2.0.0 Ready to start TLS\r\n")
            w = starttls(s.conn)
            if w is None:
                return
            s.conn, s.buf, tls = w[0], b"", True
        elif verb == b"AUTH":
            mech, _, initial = arg.partition(b" ")
            if mech.upper() == b"LOGIN":
                s.send(b"334 VXNlcm5hbWU6\r\n")
                s.line()
                s.send(b"334 UGFzc3dvcmQ6\r\n")
                s.line()
            elif not initial:
                s.send(b"334 \r\n")
                s.line()
            s.send(b"235 2.7.0 Authentication successful\r\n")
        elif verb == b"MAIL":
            s.send(b"250 2.1.0 Ok\r\n")
        elif verb == b"RCPT":
            s.send(b"250 2.1.5 Ok\r\n")
        elif verb == b"DATA":
            s.send(b"354 End data with <CR><LF>.<CR><LF>\r\n")
            while True:
                line = s.line()
                if line is None:
                    return
                if line == b".":
                    break
            s.send(b"250 2.0.0 Ok: queued as %X\r\n" % random.getrandbits(40))
        elif verb in (b"RSET", b"NOOP"):
            s.send(b"250 2.0.0 Ok\r\n")
        elif verb == b"QUIT":
            s.send(b"221 2.0.0 Bye\r\n")
            return
        else:
            s.send(b"502 5.5.2 Error: command not recognized\r\n")


FTP = {
    b"USER": b"331 Please specify the password.",
    b"PASS": b"230 Login successful.",
    b"SYST": b"215 UNIX Type: L8",
    b"PWD": b'257 "/" is the current directory',
    b"CWD": b"250 Directory successfully changed.",
    b"MKD": b'257 "/" created',
    b"TYPE": b"200 Switching to Binary mode.",
    b"MODE": b"200 Mode set to S.",
    b"FEAT": b"211-Features:\r\n EPSV\r\n PASV\r\n SIZE\r\n UTF8\r\n211 End",
    b"OPTS": b"200 Always in UTF8 mode.",
    b"NOOP": b"200 NOOP ok.",
    b"DELE": b"250 Delete operation successful.",
}


def ftp(s, host, addr, starttls):
    s.send(GREETING["ftp"])
    while True:
        verb, _ = s.verb()
        if verb is None:
            return
        # the data connection, wherever it is said to be, is the sinkhole's
        # too (its REDIRECT): a file stored is written down as any connection
        port = random.randrange(30000, 50000)
        if verb == b"PASV":
            s.send(b"227 Entering Passive Mode (%s,%d,%d).\r\n" % (str(addr).replace(".", ",").encode(), port >> 8, port & 255))
        elif verb == b"EPSV":
            s.send(b"229 Entering Extended Passive Mode (|||%d|)\r\n" % port)
        elif verb in (b"STOR", b"APPE", b"RETR", b"LIST", b"NLST", b"MLSD"):
            s.send(b"150 Ok to send data.\r\n226 Transfer complete.\r\n")
        elif verb == b"PORT" or verb == b"EPRT":
            s.send(b"200 PORT command successful. Consider using PASV.\r\n")
        elif verb == b"QUIT":
            s.send(b"221 Goodbye.\r\n")
            return
        else:
            s.send(FTP.get(verb, b"500 Unknown command.") + b"\r\n")


POP3 = {
    b"USER": b"+OK",
    b"PASS": b"+OK Logged in.",
    b"APOP": b"+OK Logged in.",
    b"STAT": b"+OK 0 0",
    b"LIST": b"+OK 0 messages:\r\n.",
    b"UIDL": b"+OK\r\n.",
    b"CAPA": b"+OK\r\nCAPA\r\nTOP\r\nUIDL\r\nRESP-CODES\r\nPIPELINING\r\nAUTH-RESP-CODE\r\nUSER\r\nSASL PLAIN LOGIN\r\n.",
    b"NOOP": b"+OK",
}


def pop3(s, host, addr, starttls):
    s.send(GREETING["pop3"])
    while True:
        verb, _ = s.verb()
        if verb is None:
            return
        if verb == b"QUIT":
            s.send(b"+OK Logging out.\r\n")
            return
        s.send(POP3.get(verb, b"-ERR Unknown command.") + b"\r\n")


IMAP = {
    b"CAPABILITY": b"* CAPABILITY IMAP4rev1 SASL-IR LOGIN-REFERRALS ID ENABLE IDLE LITERAL+ AUTH=PLAIN AUTH=LOGIN\r\n",
    b"SELECT": b"* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n* 0 EXISTS\r\n* 0 RECENT\r\n",
    b"EXAMINE": b"* 0 EXISTS\r\n* 0 RECENT\r\n",
    b"LIST": b'* LIST (\\HasNoChildren) "." INBOX\r\n',
}
IMAP_OK = (b"LOGIN", b"AUTHENTICATE", b"NOOP", b"ID", b"ENABLE", b"LSUB", b"STATUS", b"CREATE", b"APPEND", b"FETCH", b"SEARCH", b"CLOSE", b"EXPUNGE")


def imap(s, host, addr, starttls):
    s.send(GREETING["imap"])
    while True:
        line = s.line()
        if line is None:
            return
        parts = line.split(b" ", 2)
        tag, verb = parts[0], parts[1].upper() if len(parts) > 1 else b""
        if verb == b"UID" and len(parts) > 2:
            verb = parts[2].split(b" ")[0].upper()
        # a literal ({N}): the message APPEND sends, a password; read whole
        if line.endswith(b"}") and b"{" in line:
            n = line[line.rindex(b"{") + 1:-1].rstrip(b"+")
            if n.isdigit():
                if not line.endswith(b"+}"):
                    s.send(b"+ OK\r\n")
                s.skip(int(n))
                s.line()  # the rest of the command, after the literal
        if verb == b"AUTHENTICATE" and len(parts) > 2 and b" " not in parts[2]:
            s.send(b"+ \r\n")
            s.line()
        if verb == b"LOGOUT":
            s.send(b"* BYE Logging out\r\n" + tag + b" OK Logout completed.\r\n")
            return
        if verb in IMAP or verb in IMAP_OK:
            s.send(IMAP.get(verb, b"") + tag + b" OK " + verb + b" completed.\r\n")
        else:
            s.send(tag + b" BAD Error in IMAP command received by server.\r\n")


DIALOGUES = {"smtp": smtp, "ftp": ftp, "pop3": pop3, "imap": imap, "ssh": ssh}
