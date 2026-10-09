"""The run's certificate authority, as a company's TLS-inspecting proxy
has one: a root made years ago (the one the VM trusts), an intermediate
that signs, and for each name a certificate issued weeks ago for 90 days,
as a public CA's would be. Nothing in them dated at the run: a validity
that starts the moment the code connects is a sandbox's tell.

  mh-sandbox-net DIR ca   (mh-sandbox-prepare, as root): ca.crt, the root,
                          for the system's list; inter.crt and inter.key
"""
import datetime
import ipaddress
import os
import secrets

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

ORG = "Acme Corp"
DAY = datetime.timedelta(days=1)


def now():
    return datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)


def ago(lo, hi):
    """A moment between lo and hi days ago, to the second."""
    return now() - datetime.timedelta(seconds=secrets.randbelow((hi - lo) * 86400) + lo * 86400)


def name(cn):
    return x509.Name([x509.NameAttribute(NameOID.COUNTRY_NAME, "US"),
                      x509.NameAttribute(NameOID.ORGANIZATION_NAME, ORG),
                      x509.NameAttribute(NameOID.COMMON_NAME, cn)])


def serial():
    return int.from_bytes(secrets.token_bytes(16), "big") >> 1  # positive, as RFC 5280 asks


def sign(subject, key, issuer, issuer_key, start, days, exts):
    b = (x509.CertificateBuilder().subject_name(subject).issuer_name(issuer).public_key(key.public_key())
         .serial_number(serial()).not_valid_before(start).not_valid_after(start + days * DAY))
    b = b.add_extension(x509.SubjectKeyIdentifier.from_public_key(key.public_key()), critical=False)
    if issuer_key is not key:
        b = b.add_extension(x509.AuthorityKeyIdentifier.from_issuer_public_key(issuer_key.public_key()), critical=False)
    for ext, critical in exts:
        b = b.add_extension(ext, critical=critical)
    return b.sign(issuer_key, hashes.SHA256())


def ca_usage():
    return x509.KeyUsage(digital_signature=True, content_commitment=False, key_encipherment=False,
                         data_encipherment=False, key_agreement=False, key_cert_sign=True, crl_sign=True,
                         encipher_only=False, decipher_only=False)


def pem_key(key):
    return key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())


def pem(cert):
    return cert.public_bytes(serialization.Encoding.PEM)


def write(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(data)


def make(d):
    """The root and the intermediate, in d. The root's key is not kept:
    nothing signs with it again."""
    root_key = ec.generate_private_key(ec.SECP384R1())
    root = sign(name(ORG + " Root CA"), root_key, name(ORG + " Root CA"), root_key, ago(1200, 1800), 7305,
                [(x509.BasicConstraints(ca=True, path_length=None), True), (ca_usage(), True)])
    key = ec.generate_private_key(ec.SECP256R1())
    inter = sign(name(ORG + " TLS Inspection CA"), key, root.subject, root_key, ago(400, 700), 1826,
                 [(x509.BasicConstraints(ca=True, path_length=0), True), (ca_usage(), True),
                  (x509.ExtendedKeyUsage([ExtendedKeyUsageOID.SERVER_AUTH, ExtendedKeyUsageOID.CLIENT_AUTH]), False)])
    write(os.path.join(d, "ca.crt"), pem(root))
    write(os.path.join(d, "inter.crt"), pem(inter))
    write(os.path.join(d, "inter.key"), pem_key(key))


class Issuer:
    def __init__(self, d):
        with open(os.path.join(d, "inter.crt"), "rb") as f:
            self.inter_pem = f.read()
        self.inter = x509.load_pem_x509_certificate(self.inter_pem)
        with open(os.path.join(d, "inter.key"), "rb") as f:
            self.key = serialization.load_pem_private_key(f.read(), None)

    def issue(self, host):
        """host's key, and its certificate followed by the intermediate's:
        the chain a server sends. Issued 1 to 60 days ago, for 90 days."""
        try:
            alt = x509.IPAddress(ipaddress.ip_address(host))
        except ValueError:
            alt = x509.DNSName(host)
        subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, host)])
        key = ec.generate_private_key(ec.SECP256R1())
        usage = x509.KeyUsage(digital_signature=True, content_commitment=False, key_encipherment=False,
                              data_encipherment=False, key_agreement=False, key_cert_sign=False, crl_sign=False,
                              encipher_only=False, decipher_only=False)
        cert = sign(subject, key, self.inter.subject, self.key, ago(1, 60), 90,
                    [(x509.BasicConstraints(ca=False, path_length=None), True), (usage, True),
                     (x509.ExtendedKeyUsage([ExtendedKeyUsageOID.SERVER_AUTH, ExtendedKeyUsageOID.CLIENT_AUTH]), False),
                     (x509.SubjectAlternativeName([alt]), False)])
        return pem_key(key), pem(cert) + self.inter_pem
