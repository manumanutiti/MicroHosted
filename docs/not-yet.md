# Not yet — features weighed and deferred

What was studied, why it is not built, and the approach chosen for when it
is. Each entry says what would make it worth doing. The roadmap's
"Deliberately not yet" lists the larger items; this file keeps the reasoning
for the smaller ones, so the next person does not start from zero.

- [Egress by domain name (DNS allowlist)](#egress-by-domain-name-dns-allowlist) — 2026-10-01

---

## Egress by domain name (DNS allowlist)

**Status: not planned for now (decided 2026-10-01).** Too much work, and too
much threat model to study, for what it brings today.

### What it would be

`--out` takes addresses (`tcp:1.2.3.4:443`), not names. Allowing
`registry.npmjs.org` or `api.stripe.com` by name is what long-running VMs
would want: a service that may talk to one API, an OT sensor to its broker.

### Why addresses cannot simply stand in

Resolving the name when the rule is written and storing the addresses breaks
within hours: registries, APIs and GitHub sit behind CDNs whose addresses
rotate.

### The chosen approach, when it is built: a resolver in its own microVM

A DNS resolver the guests must use, whose answers feed the policy:

1. A network's guests can resolve only through it; port 53 anywhere else is
   dropped.
2. An allowed name is resolved upstream; its addresses enter an nftables set
   of that network, expiring with the record's TTL (with a floor: the
   `forward` chain matches statelessly, so an address leaving the set cuts a
   live connection).
3. A name that is not allowed gets NXDOMAIN and is **never forwarded
   upstream**.
4. The egress rule accepts destinations in the set. The kernel still
   decides, as today.

The resolver runs **inside a microVM, not on the host**. On the host it would
mean a new listener on every bridge (a hole in the `input` chain, which drops
everything from guests today) parsing packets written by the code being
contained. In a VM, the host keeps listening to nothing: it dials the
resolver over vsock, as it dials every guest, and reads back one line per
address (`network address ttl`) that is trivial to validate. The resolver
answers a guest only after the host acknowledges the address is in the set,
or the guest's first connection would race the rule.

Rejected: a resolver process on the host (as libvirt's dnsmasq), even
unprivileged and in a memory-safe language — it is the one design that adds
attack surface to the host.

### What it would bring

- **No DNS tunnel.** Today a network allowed `udp:8.8.8.8:53` can carry data
  out in subdomains (`<secret>.attacker.example`); a name that is not allowed
  would never leave the resolver.
- **Names in `mh flows`**: "tried to resolve paste.ee" is the most legible
  signal there is.
- **No DNS rebinding.** An allowed name answering with a private address
  (127.0.0.1, 169.254.169.254, the host's LAN) never enters the set.

### What it would not bring — the limit to state up front

**Allowing a name is allowing its addresses.** `registry.npmjs.org` is on
Cloudflare: allowing it allows Cloudflare's addresses, shared by countless
other sites. Code that can host something on the same CDN can connect to an
allowed address and name another host in TLS (SNI). Holding a connection to
its name takes a proxy that checks the SNI of each connection — a later
stage, with a threat model of its own.

### What is open, to study first

- How the guests' port 53 reaches the resolver VM across segments (a DNAT
  leg, like ingress) without opening anything else between networks.
- One resolver per host or per network: RAM (~40 MB each) against a shared
  component every network depends on.
- Bounds: names and addresses per network, queries per second per guest.
- TTL floor, and what a connection does when its address expires.
- What the resolver VM itself may reach: its upstream resolvers only.

### Why not now, and what would change it

For the sandbox (orchestrator/examples/sandbox) it brings little: code runs
without a network, and dependencies are fetched by the package manager — not
the code — with the internet but not the LAN, before the network is cut.
It would be worth doing when long-running VMs need to reach services by name
(an API behind a CDN, a cloud broker), or when DNS exfiltration from a
network with a resolver allowed becomes a case to close.
