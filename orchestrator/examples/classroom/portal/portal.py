"""The school portal: the lab's services, and the grades from the database.

GET /        the page
GET /health  200 while the portal runs (the database may be down: the page
             says so instead of failing)
"""
import html
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import psycopg2

# name, protocol/port, what it is, how to try it from the student VM
SERVICES = [
    ("ns.school.lan", "DNS 53/udp+tcp", "BIND 9, authoritative for school.lan",
     "dig www.school.lan · dig -x 172.30.80.11 · dig SRV _mqtt._tcp.school.lan"),
    ("db.school.lan", "PostgreSQL 5432/tcp", "the school database",
     "psql -h db -U student school -c 'select * from report'"),
    ("portal.school.lan", "HTTP 80/tcp", "this page", "curl -I http://www.school.lan/"),
    ("ftp.school.lan", "FTP 21/tcp", "course materials, anonymous read-only",
     "curl ftp://ftp.school.lan/ · lftp ftp.school.lan"),
    ("mqtt.school.lan", "MQTT 1883/tcp", "Mosquitto: the lab sensors publish here",
     "mosquitto_sub -h mqtt -t 'school/#' -v"),
    ("ntp.school.lan", "NTP 123/udp", "chrony: the lab's clock", "ntpdig ntp.school.lan"),
]

DSN = "host=db.school.lan dbname=school user=portal connect_timeout=2"


def grades():
    try:
        with psycopg2.connect(DSN) as conn, conn.cursor() as cur:
            cur.execute("SELECT grp, student, subject, grade FROM report ORDER BY grp, student, subject")
            return cur.fetchall(), None
    except psycopg2.Error as e:
        return [], str(e).strip()


def page():
    rows, err = grades()
    esc = html.escape
    svc = "".join(
        f"<tr><td><code>{esc(n)}</code></td><td>{esc(p)}</td><td>{esc(d)}</td><td><code>{esc(t)}</code></td></tr>"
        for n, p, d, t in SERVICES)
    if err:
        grd = f"<p class=err>The database does not answer: {esc(err)}</p>"
    else:
        grd = "<table><tr><th>Group</th><th>Student</th><th>Subject</th><th>Grade</th></tr>" + "".join(
            f"<tr><td>{esc(g)}</td><td>{esc(s)}</td><td>{esc(su)}</td><td>{gr}</td></tr>"
            for g, s, su, gr in rows) + "</table>"
    return f"""<!doctype html><meta charset=utf-8><title>school.lan</title>
<style>body{{font:15px system-ui;margin:2em auto;max-width:60em;padding:0 1em}}
table{{border-collapse:collapse;width:100%;margin-bottom:2em}}td,th{{border-bottom:1px solid #ccc;padding:.4em;text-align:left}}
code{{font-size:13px}}.err{{color:#b00}}</style>
<h1>school.lan</h1>
<p>Every machine of this lab is a microVM. Log in to the student one with
<code>mh exec classroom-student-1 bash</code> and try the commands below.</p>
<h2>Services</h2><table><tr><th>Name</th><th>Protocol</th><th>What</th><th>Try</th></tr>{svc}</table>
<h2>Grades <small>(live from db.school.lan)</small></h2>{grd}"""


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body, ctype = b"ok\n", "text/plain"
        elif self.path == "/":
            body, ctype = page().encode(), "text/html; charset=utf-8"
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 80), Handler).serve_forever()
