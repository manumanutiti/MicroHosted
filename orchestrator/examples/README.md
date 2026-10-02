# Examples

Each directory is one project: a `microse.yml` (what runs) and the
`build.yml` of each image it runs, next to it or in a subdirectory. Try one:

```bash
cd orchestrator/examples/hello
mh up            # builds the images the first time (sudo), then runs; Ctrl-C to stop
mh status        # in another terminal: functions, VMs, health
mh down          # remove everything it created
```

`mh up -d` runs it in the background. Every example is commented from top to
bottom: the head of its `microse.yml` says what it shows and what to try.

| Example | What it shows | VMs | Images |
|---|---|---|---|
| [hello](hello/microse.yml) | The three lifecycle modes — persistent, transaction, window — with shell one-liners. Start here. | 1 + cycles | Alpine |
| [website](website/microse.yml) | One VM serving a page to the LAN through an ingress rule; files shipped with `files:` | 1 | Alpine + Python |
| [alpine-nginx](alpine-nginx/build.yml) | **The build.yml reference**: every field, commented. nginx serving a static site | 1 | Alpine |
| [app-ubuntu](app-ubuntu/build.yml) | The same on Ubuntu 24.04: a Python service, a system user, systemd | 1 | Ubuntu |
| [intranet](intranet/microse.yml) | Three services on one segment, clients, a traffic burst, and a segmentation check from another network | 3 + cycles | Alpine + Python |
| [stack](stack/microse.yml) | Three tiers, three images: PostgreSQL, Node.js, nginx — and the build cache (`steps:`) | 3 | Ubuntu, Alpine |
| [ansible](ansible/microse.yml) | A lab to test a playbook: a control node and five Ubuntu machines over SSH | 6 | Ubuntu |
| [classroom](classroom/microse.yml) | A school network for teaching protocols: DNS, PostgreSQL, web portal, FTP, MQTT, NTP and a student client | 7 + cycles | Ubuntu |
| [github-runner](github-runner/microse.yml) | GitHub Actions runners, one job per VM, with Docker and a single-use registration from `secrets:` | 1 | Ubuntu |

Ubuntu images take a few minutes to build the first time (debootstrap and
apt); after that the build cache makes a rebuild a matter of seconds, and
a build with nothing changed builds nothing.

## Addresses

Network names and subnets are shared by the whole host, so every example
has its own and any of them can run side by side:

| Example | Network | Subnet |
|---|---|---|
| hello | demo | 172.30.10.0/24 |
| intranet | office, guest | 172.30.20.0/24, 172.30.21.0/24 |
| website | web-demo | 172.30.30.0/24 |
| alpine-nginx | nginx-demo | 172.30.50.0/24 |
| app-ubuntu | ubuntu-demo | 172.30.51.0/24 |
| stack | stack | 172.30.60.0/24 |
| github-runner | runners | 172.30.61.0/24 |
| ansible | lab | 172.30.70.0/24 |
| classroom | school | 172.30.80.0/24 |

The host reaches every VM by its address (`curl http://172.30.80.12/`);
nothing else does unless the network says so (`allowed_ingress`, see
website).

## Writing your own

Copy the example closest to what you want. The patterns they share:

- **One `build.yml` per kind of machine**; the software comes from the
  distribution's packages (`packages:`), configuration from files next to
  it (`files:`), and anything else from `run:` — checked at build time
  (`nginx -t`, `named-checkconf`, `py_compile`), so a mistake fails the
  build, not the deploy.
- **Ubuntu when the service is an apt package run by systemd** (classroom,
  ansible, stack): no `command:`, systemd starts it; the image's `health:`
  says when it works. **Alpine when a single process is enough** (hello,
  website, alpine-nginx): smaller and faster to boot.
- **Fixed addresses** (`ip:`) for anything others connect to; what is only a
  client (a cycle, a sensor) needs none.
- **What differs per deployment goes in `microse.yml`** — files, secrets,
  resources — and the image stays the same everywhere.
- **Anything a VM writes is lost when it is replaced.** Data that must last
  goes in a volume ([docs/volumes.md](../../docs/volumes.md)).
