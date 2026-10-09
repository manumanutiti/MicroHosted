"""What is my address, and where: the services code asks before it acts
(stealers report it, some spare some countries). One answer for the run:
a home connection's address, made up from the run's token, and its place."""
import json
import time

from ..http import Answer

PLACE = {"city": "Denver", "region": "Colorado", "region_code": "CO", "country": "United States",
         "cc": "US", "postal": "80202", "lat": 39.7392, "lon": -104.9903, "tz": "America/Denver",
         "isp": "Comcast Cable Communications, LLC", "asn": "AS7922", "colo": "DEN"}

TEXT = {"api.ipify.org", "api4.ipify.org", "api64.ipify.org", "icanhazip.com", "ipv4.icanhazip.com",
        "checkip.amazonaws.com", "ipecho.net", "ident.me", "v4.ident.me", "ifconfig.me", "ifconfig.co",
        "myexternalip.com", "ipinfo.io", "wtfismyip.com"}
JSON = {"ipinfo.io", "ip-api.com", "ipapi.co", "ifconfig.co", "ifconfig.me", "api.myip.com", "httpbin.org",
        "wtfismyip.com", "ipwho.is", "freegeoip.app", "api.ipify.org", "api64.ipify.org"}
TRACE = {"www.cloudflare.com", "cloudflare.com", "1.1.1.1", "1.0.0.1", "one.one.one.one"}


def matches(host):
    return host in TEXT or host in JSON or host in TRACE


def address(ctx):
    """73.x.y.z, from the run's token: a home connection's."""
    t = ctx.token or b"0" * 8
    b = [int(t[i:i + 2], 16) if len(t) >= i + 2 else 1 for i in (0, 2, 4)]
    return "73.%d.%d.%d" % (b[0], b[1], 1 + b[2] % 254)


def hostname(ip):
    return "c-%s.hsd1.co.comcast.net" % ip.replace(".", "-")


def as_json(host, ip):
    p = PLACE
    if host == "ipinfo.io":
        return {"ip": ip, "hostname": hostname(ip), "city": p["city"], "region": p["region"], "country": p["cc"],
                "loc": "%.4f,%.4f" % (p["lat"], p["lon"]), "org": p["asn"] + " " + p["isp"], "postal": p["postal"],
                "timezone": p["tz"], "readme": "https://ipinfo.io/missingauth"}
    if host == "ip-api.com":
        return {"status": "success", "country": p["country"], "countryCode": p["cc"], "region": p["region_code"],
                "regionName": p["region"], "city": p["city"], "zip": p["postal"], "lat": p["lat"], "lon": p["lon"],
                "timezone": p["tz"], "isp": p["isp"], "org": p["isp"], "as": p["asn"] + " " + p["isp"], "query": ip}
    if host in ("ipapi.co", "freegeoip.app"):
        return {"ip": ip, "network": ip.rsplit(".", 1)[0] + ".0/24", "version": "IPv4", "city": p["city"],
                "region": p["region"], "region_code": p["region_code"], "country": p["cc"], "country_name": p["country"],
                "country_code": p["cc"], "postal": p["postal"], "latitude": p["lat"], "longitude": p["lon"],
                "timezone": p["tz"], "asn": p["asn"], "org": p["isp"]}
    if host == "ipwho.is":
        return {"ip": ip, "success": True, "type": "IPv4", "country": p["country"], "country_code": p["cc"],
                "region": p["region"], "city": p["city"], "latitude": p["lat"], "longitude": p["lon"],
                "postal": p["postal"], "connection": {"asn": int(p["asn"][2:]), "isp": p["isp"]},
                "timezone": {"id": p["tz"]}}
    if host == "api.myip.com":
        return {"ip": ip, "country": p["country"], "cc": p["cc"]}
    if host == "httpbin.org":
        return {"origin": ip}
    if host == "wtfismyip.com":
        return {"YourFuckingIPAddress": ip, "YourFuckingLocation": "%s, %s, %s" % (p["city"], p["region_code"], p["cc"]),
                "YourFuckingHostname": hostname(ip), "YourFuckingISP": p["isp"], "YourFuckingCountryCode": p["cc"]}
    return {"ip": ip}


def answer(ctx, req):
    host, path, ip = req.host, req.path, address(ctx)
    if host in TRACE:
        if path != "/cdn-cgi/trace":
            return None
        body = ("fl=12f1\nh=%s\nip=%s\nts=%.3f\nvisit_scheme=%s\nuag=curl/8.5.0\ncolo=%s\nsliver=none\nhttp=http/1.1\n"
                "loc=%s\ntls=TLSv1.3\nsni=plaintext\nwarp=off\ngateway=off\nrbi=off\nkex=X25519\n"
                % (host, ip, time.time(), req.url.split(":", 1)[0], PLACE["colo"], PLACE["cc"]))
        return Answer("ip lookup", "200 OK", "text/plain", body.encode(), [("Server", "cloudflare")])
    if host == "httpbin.org" and path != "/ip":
        return None
    if host == "ipinfo.io":  # JSON but for /ip
        if path not in ("/", "/json", "/ip"):
            return None
        wants_json = path != "/ip"
    elif host in TEXT:  # text but where JSON is asked for
        wants_json = "json" in path or "format=json" in req.query
    else:
        wants_json = True
    if wants_json and host in JSON:
        return Answer("ip lookup", "200 OK", "application/json; charset=utf-8", json.dumps(as_json(host, ip)).encode(), [])
    return Answer("ip lookup", "200 OK", "text/plain; charset=utf-8", (ip + "\n").encode(), [])
