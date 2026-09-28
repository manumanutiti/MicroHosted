# Uses, by example

Worked procedures for things people run on MicroHosted: what to build, what to
write, what to run, and what the result measured on real hardware. Each one
uses only the documented tools (`mh build`, `mh up` and the other project
verbs, `mh`).

- [A static website in a microVM](#a-static-website-in-a-microvm)

---

## A static website in a microVM

A site generated to static files — Next.js with `output: "export"`, Astro,
Hugo, Eleventy, plain HTML — served by nginx from a ~100 MB Alpine image, kept
running by the orchestrator, reachable from the LAN. The site's files live
**inside the image**: a site of any size goes in (`files:` in a plant spec is
for small per-VM configuration, at most 64 files / 512 KiB), a deploy is a new
image, and a rollback is the previous reference.

```
 browser ──► host :8081 (reverse proxy, or an ingress rule on a managed interface)
                 │
                 ▼
 network web 10.88.0.0/24     site  .11   nginx :8080, the exported site in /srv/www
```

### 1. Export the site

Whatever the generator, the result is a directory of files. For Next.js:

```ts
// next.config.ts
const nextConfig: NextConfig = {
  output: "export",           // static HTML/JS/CSS in out/
  trailingSlash: true,        // /about/ → out/about/index.html
  images: { unoptimized: true },
};
```

- Routes with parameters need `generateStaticParams` (and `dynamicParams =
  false`); metadata routes (`opengraph-image`, `robots`, `sitemap`) need
  `export const dynamic = "force-static"`.
- What needs a server is not exported: `redirects()`, `headers()`, API routes,
  server actions. Redirects move to nginx (below). A site that needs the rest
  runs `next start` instead — `packages: [nodejs]` and `output: "standalone"`,
  at ~150 MB of RAM per VM instead of ~50.

`npm run build` leaves the site in `out/` (tens of MB, hundreds of files is
typical).

### 2. The nginx configuration

```nginx
server {
    listen 8080 default_server;
    root /srv/www;
    absolute_redirect off;          # relative Location: works behind any proxy
    access_log off;                 # see "Disk" below
    gzip on;                        # see "Capacity" below
    gzip_types text/css application/javascript application/json image/svg+xml text/plain text/xml;

    # Old URLs → new pages (what redirects() did).
    location = /index.html     { return 301 /; }
    location = /services.html  { return 301 /services/; }

    location = /opengraph-image { default_type image/png; }   # a file without extension
    location /_next/static/ { expires 1y; add_header Cache-Control "public, immutable"; }

    location / {
        try_files $uri $uri/index.html $uri.html =404;
    }
    error_page 404 /404.html;
}
```

`try_files` serves `/about` and `/about/` from `about/index.html` without a
redirect. Serving `index.html` through `try_files` rather than `index` keeps the
`/index.html` redirect from looping.

### 3. The image spec: `build.yml`

One directory holds everything — the build context and the plant spec:

```
site/
  build.yml       the image
  microse.yml     what runs it
  nginx.conf
  out/            the exported site
```

```yaml
# build.yml
name: site
base: alpine:3.22
packages: [nginx]
files:
  /srv/www/: out/                               # the directory's contents
  /etc/nginx/http.d/default.conf: nginx.conf
run:
  - mkdir -p /run/nginx
  - nginx -t                                    # a broken config fails the build, not the deploy
command: nginx -g 'daemon off;'
health: { command: "wget -qO- -T 2 http://127.0.0.1:8080/ >/dev/null", every: 10s, timeout: 3s, failures: 3 }
mem_mb: 128
```

`command` and `health` travel with the image, so the plant spec does not repeat
them. `mh build` in that directory builds it (on the engine's host; it asks for
`sudo` once) and prints `site:sha-<fingerprint>@sha256:…`; run again with
nothing changed, it builds nothing and prints the same. The image comes out at
~107 MB (Alpine, nginx, a 16 MB site). The plant spec below does this itself.

### 4. The plant spec: `microse.yml`

```yaml
# microse.yml
version: 1
budget: { max_vms: 2, max_mem_mb: 256, workers: 1 }

networks:
  web:
    subnet: 10.88.0.0/24

functions:
  site:
    build: .                                    # ./build.yml
    network: web
    ip: 10.88.0.11
    lifecycle: { mode: persistent, recycle: never }
```

```bash
cd site/
mh plan                         # builds the image if it is not built yet, then:
#   create   function site  (persistent, site:sha-…@sha256:…, command and health from the image)
mh up -d                        # keep it running in the background: replaced if it dies or fails its check
mh status
```

The VM serves about a second after `create`. An image made elsewhere goes in
with `image: name:version@sha256:…` instead of `build:` — typed, or
`image: ${SITE}` with `SITE=…` in a `.env` next to the spec.

### 5. Reaching it from the LAN

Two ways, depending on how the host is set up:

- **An ingress rule** (the host's LAN interface is managed by the engine,
  `--managed-iface`): add to the network
  `allowed_ingress: [{ iface: wlan0, src_ip: 192.168.1.0/24, protocol: tcp, port: 8080, to_ip: 10.88.0.11 }]`
  — DNAT to the VM, no listener on the host. See
  `orchestrator/examples/website.yaml`.
- **A reverse proxy on the host** (an unmanaged interface, TLS termination,
  several sites on one port): the host reaches the network's addresses through
  its bridge, so `proxy_pass http://10.88.0.11:8080;` in the host's nginx (or
  Caddy, or a container with host networking) publishes it. TLS belongs here.

### 6. Deploying a new version

```bash
npm run build                   # new out/
mh apply                        # builds the new image, hands it to the running up
```

The fingerprint of `out/` changed, so `apply` builds a new image (an unchanged
site builds nothing). The running orchestrator boots a VM from it, checks it and
only then retires the old one; a new version that fails its health check is
rolled back to the previous image. Rolling back by hand: `image:` with the
previous reference (`mh image ls -q`) in place of `build:`, and apply.

### Capacity: what one VM holds

Measured with `ab` from the host against one VM (1 vCPU, 64 MB, x86_64
8-core host), nginx as above except gzip off:

| Request | Concurrency | Result |
|---|---|---|
| small file (27 B) | 50 → 200 | 9 200 → 11 400 req/s, p99 ~55 ms |
| small file, keep-alive | 200 | 19 000 req/s |
| 170 KB page, uncompressed | 10 / 50 | ~73 req/s, capped by the network: 12.5 MB/s |
| 170 KB page, uncompressed | 100 | connections reset |

Two limits show up before the CPU does:

- **The per-VM network ceiling**, 100 Mbit/s per direction by default
  (`--vm-net-mbit` on the daemon, `docs/deploy.md`). Page weight divides it:
  170 KB uncompressed is ~73 pages/s; the same page gzipped (30 KB) is ~400.
  Turn `gzip on` and raise the ceiling if the host's uplink allows.
- **Guest memory for TCP buffers.** The guest kernel sizes its TCP memory from
  its RAM: with 64 MB it allows ~3.3 MB (`/proc/sys/net/ipv4/tcp_mem`), and a
  hundred simultaneous 170 KB downloads exceed it — the kernel resets
  connections (`TCPAbortOnMemory` in `/proc/net/netstat`). gzip shrinks every
  buffer ~5×; `mem_mb: 128` roughly doubles the allowance.

In visitors: a page view is the HTML plus its assets, and browsers cache the
assets. With gzip and 128 MB, one VM serves a few hundred page views per second
within the default network ceiling — far more than a small or medium site gets
at its peak; hundreds of people reading at once is a few page views per second.
Beyond that, raise `--vm-net-mbit`, or run two functions with the same image
behind the proxy.

### Disk: what the VM keeps

- `size_mb` in the image spec sizes the root filesystem (default: its content
  plus a quarter and 32 MB); `disk_mb` (image default) or `resources.disk_mb`
  (plant spec, per function) grows each VM's copy at creation, for space to
  write at run time.
- **Nothing written in the VM survives it**: a replacement (a failed check, a
  new version, `recycle`) boots a fresh copy of the image. A static site has
  nothing to keep; logs are the thing that grows. Alpine's nginx logs every
  request to `/var/log/nginx/access.log` on the root disk — 9 MB in the load
  test above — which is why the configuration turns it off (or send it to the
  host's proxy, which sees every request anyway).
- Data that must outlive VMs goes in a volume (`docs/volumes.md`) — attached
  with `mh`; plant specs do not declare volumes yet.
