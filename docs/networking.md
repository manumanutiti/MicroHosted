# Segmented networking (Phase 1)

Replaces the point-to-point `/30` model (where the host was the gateway for every
VM and two VMs couldn't see each other) with **named networks**, which solve
both "networks between machines" and "isolation against malicious guests" with a
single mechanism.

## Model

A **Network** is a named L2 segment:

- a Linux **bridge** (`mhbr<id>`, where `<id>` is 8 hex chars — fits in the 15
  chars of `IFNAMSIZ`),
- a **subnet** (CIDR, e.g. `172.16.0.0/24`),
- a **gateway** = the host's IP on the bridge (`.1` of the subnet),
- an **egress** flag (internet access yes/no) and optionally **allowed_egress**
  (fine-grained egress),
- an **intra** flag (VM↔VM connectivity within the network; **off by default**).

Every VM that joins a network gets a **TAP enslaved to that bridge** (the TAP
carries no IP; the gateway IP lives on the bridge) and an **IP from the network's
subnet** (per-network IPAM). The guest's gateway is the bridge's.

- VMs on the **same** network → **isolated by default** (deny-by-default): each
  TAP is enslaved as a kernel *isolated bridge port*, which doesn't exchange
  frames with other isolated ports but does with the bridge (the gateway). Only
  with `intra: true` is the network a classic L2 segment where VMs see each
  other. This is done at the L2 level on purpose: same-bridge traffic **doesn't
  go through nftables** (it's pure switching), a forward rule couldn't cut it.
- VMs on **different** networks → isolated (separate bridges + an nftables rule
  that drops cross traffic).
- `no_network: true` remains valid: a VM with no TAP, no IP, vsock only. The most
  hermetic sandbox.

## nftables policy (table `inet microhosted`)

- **guest → host**: DROP. The VM can't reach host services (only its gateway, and
  only for routing if there's egress).
- **cross-segment**: DROP. Traffic from subnet A toward subnet B is dropped.
  Implemented as ONE aggregated rule over sets (`@mhbridges` + a `@mhsame`
  exemption for intra-bridge traffic under br_netfilter), not one rule per bridge
  pair: the ruleset stays O(N) with N networks, key for the network-per-VM
  topology.
- **egress**:
  - `egress: true`  → `MASQUERADE` of the subnet through the default outbound
    interface + FORWARD allowed toward the WAN.
  - `egress: false` → no NAT and FORWARD toward the WAN dropped. **Default**, for
    security (malware-safe): a sample doesn't call home unless explicitly asked.
  - `egress: false` + **`allowed_egress`** → fine-grained egress: only the listed
    flows (destination IP/CIDR + protocol + port) are accepted in `forward`
    **before** the drop toward the WAN, and the subnet is masqueraded so those
    flows get NAT. Everything else still falls into the drop. Typical IoT case: a
    parser that can only talk to its MQTT broker (`203.0.113.7:8883/tcp`) and
    nothing else.

## Fine-grained egress (`allowed_egress`)

Each rule is `{ip, protocol, port}`: `ip` is an IPv4 or IPv4 CIDR **in canonical
form**, `protocol` ∈ {`tcp`, `udp`, `icmp`} (lowercase), and `port` (1–65535) is
required for tcp/udp and forbidden for icmp. It is **mutually exclusive** with
`egress: true` (which already allows everything).

Validation (`ValidateEgressRules`) is a security boundary, not a convenience: the
fields are interpolated as-is into the `nft -f` script, so anything that isn't
strictly canonical IPv4 + a known protocol + a numeric port is rejected in
`POST /v1/networks` — otherwise it would be ruleset injection.

In the `forward` chain, the order per restricted network is: `allowed_egress`
accepts → drop toward the WAN. In `postrouting`, the restricted subnet is
masqueraded as a whole — it's safe because postrouting only sees packets the
`forward` chain already accepted.

### Hot update of intra (`PUT /v1/networks/{name}/intra`)

The `intra` flag can also be changed live: it's persisted and then
`vm.Manager.SyncTapIsolation` walks the network's live TAPs re-applying
`bridge link set ... isolated on/off`. The division of responsibilities is
deliberate: the flag lives in the network manager, but TAP names belong to the
VMs (a fork on Firecracker < 1.12 may reuse the snapshot's TAP, so `tap<id>`
isn't a reliable convention) — that's why the VM manager does the walk. If any
TAP fails to converge when hardening, the endpoint returns 500 naming it: a TAP
that keeps old reachability is a hole, not a log detail.

### Hot update of egress (`PUT /v1/networks/{name}/egress`)

A live network's egress policy can be **replaced** without touching the connected
VMs: bridge, subnet, and IPs don't change, only the ruleset is re-rendered (which
is already declarative and atomic). The body replaces the whole policy, it
doesn't merge.

Key design decision: the `forward` chain matches **statelessly** — it doesn't
have the `ct state established,related accept` that `input` does. Every packet of
a guest-initiated flow re-evaluates the policy on each pass, so when the rules are
hardened, flows opened under the previous policy die on the next packet: a stale
conntrack entry can't sneak through an established accept, and the `conntrack(8)`
binary isn't needed to flush anything. Replies (return traffic from the WAN)
aren't touched by any drop (they're all scoped by bridge `iifname`), they pass via
policy accept.

## Egress and coexistence with the host firewall (READ — a source of subtle bugs)

Internet egress for an `egress: true` network depends on **two host-level
conditions** that MicroHosted resolves automatically. Documented here in depth
because they're the #1 cause of "the network works but there's no internet", and
the behavior changes depending on whether Docker is on the machine or not.

### How egress works

For a guest (e.g. `172.16.2.2`) to reach `8.8.8.8`:

1. The guest sends the packet to its gateway (the bridge's IP, `172.16.2.1`) at
   the L2 level.
2. The host **routes** it (destination isn't local) → it goes through netfilter's
   `forward` hook.
3. The host **masquerades** it (`MASQUERADE`) through its outbound interface
   (`postrouting` hook, NAT), rewriting the source to the host's IP.
4. The reply comes back, conntrack recognizes it (`established`) and unmasquerades
   it.

None of this uses the `input` hook (which is only for traffic destined to the host
itself) — that's why the guest can **route through** the gateway even though it's
forbidden to **talk to** the gateway (`ping 172.16.2.1` is still blocked). This is
correct and intended.

### Condition 1 — kernel IP forwarding

With `net.ipv4.ip_forward = 0` the kernel drops the packet at step 2, before NAT
acts. MicroHosted enables it on its own (`network.EnsureIPForward`,
`internal/network/forward.go`) as soon as **any** network with egress exists. It
never disables it (the host may have it on for other reasons).

### Condition 2 — the host firewall must not drop the FORWARD

**Key netfilter semantics**: on the same hook (e.g. `forward`), **several base
chains** from different tables can coexist, and **all of them are evaluated**. A
`drop` verdict in *any* of them is **final** and drops the packet; an `accept`
only ends *that* chain, it doesn't prevent a later chain from dropping it.
Consequence: **our table `inet microhosted` doing `accept` is NOT enough** if
another host table drops the forward.

#### Without Docker (the usual case)

The `forward` hook's default policy is usually `accept`. Our table applies its
fine-grained policy (drop guest→host, drop cross-segment, drop no-egress→WAN;
accept + masquerade for egress) and **egress works directly**. MicroHosted doesn't
touch any host firewall. Nothing to configure.

#### With Docker (breaks egress until coordinated)

Docker installs, in the `ip filter` table (managed by iptables-nft), a `FORWARD`
chain with **`policy drop`**, and only `accept`s traffic from *its* bridges.
Traffic from *our* bridges falls into that `drop` → **no internet**, even though
`ip_forward=1` and our `masquerade` are fine. (Cross-segment is still blocked all
the same, because there *our* `drop` wins — hence the characteristic symptom:
"inter-VM and isolation OK, but egress doesn't go out".)

Solution (standard pattern, the same one libvirt uses): Docker exposes the
`DOCKER-USER` chain, which it jumps to **before** its own drop logic, precisely so
external tools can allow their traffic. MicroHosted
(`network.EnsureDockerForwarding`) adds there, idempotently:

```
iptables -t filter -I DOCKER-USER -i mhbr+ -j ACCEPT
iptables -t filter -I DOCKER-USER -o mhbr+ -j ACCEPT
```

`mhbr+` is iptables' wildcard for "any `mhbr...` interface", so two static rules
cover all present and future bridges. It runs only if the `DOCKER-USER` chain
exists (Docker present) and only when there's a network with egress. **It doesn't
weaken isolation**: our table `inet microhosted` is still evaluated and its
`drop`s (guest→host, cross-segment, no-egress→WAN) still win by the final-drop
rule. The `accept` in `DOCKER-USER` only prevents Docker's generic `drop` from
getting ahead of our policy.

**Best-effort, never fatal**: if programming `DOCKER-USER` or `ip_forward` fails,
a *warning* is logged in the journal and the daemon stays alive (it doesn't take
down VM management). The `inet microhosted` table, however, is authoritative and
does fail hard. Note: if you **restart the Docker service**, it recreates its
chains and may drop our `DOCKER-USER` rules; they're re-added on their own on the
next network create/delete or microhosted restart.

#### Other firewalls (ufw / firewalld active)

Same principle: if `ufw` or `firewalld` are **active** with the forward default at
`drop`, they can drop egress. MicroHosted does **not** reconfigure them
automatically (only Docker, via its standard extension point). If you use one of
them with egress, allow the forward of the `mhbr+` bridges:

```bash
# ufw: permissive forward policy (or a specific rule for mhbr+)
sudo sed -i 's/^DEFAULT_FORWARD_POLICY=.*/DEFAULT_FORWARD_POLICY="ACCEPT"/' /etc/default/ufw && sudo ufw reload
# firewalld: put the bridges in a zone that allows forward, e.g. trusted
sudo firewall-cmd --permanent --zone=trusted --add-interface=mhbr+ ; sudo firewall-cmd --reload
```

### Behavior matrix (egress: true)

| Host | Foreign `forward` policy | What MicroHosted does | Result |
|------|--------------------------|----------------------|--------|
| No extra firewall | `accept` | nothing (our table + `ip_forward`) | egress OK |
| Docker | `drop` (Docker's chain) | `ip_forward` + auto `DOCKER-USER` accept | egress OK |
| ufw/firewalld active with forward `drop` | `drop` | `ip_forward` (doesn't touch ufw/firewalld) | egress **fails** → apply the fix above |

### Quick diagnosis if egress doesn't go out

```bash
cat /proc/sys/net/ipv4/ip_forward                 # must be 1
sudo nft list table inet microhosted              # is your subnet's 'masquerade' line there?
sudo nft list ruleset | grep -iB1 -A4 'hook forward'  # is there ANOTHER forward chain with 'policy drop'? (Docker/ufw/firewalld)
sudo iptables -t filter -S DOCKER-USER 2>/dev/null    # with Docker: the 2 'mhbr+ ACCEPT' rules must be there
# from the guest: default route + gateway ARP
curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{"cmd":"ip route; ip neigh"}'
```

Mental rule: **broken egress is almost always = another `forward` chain with
`policy drop` (Docker/ufw/firewalld) or `ip_forward=0`**. Isolation (guest→host,
cross-segment) does NOT depend on any of this — our table enforces it and always
wins.

## DNS in the guest (READ — symptom: `ping <IP>` works, `ping <domain>` doesn't)

Characteristic symptom: `ping 8.8.8.8` responds fine, but `ping google.com` gives
`Temporary failure in name resolution`. **It's not a network or firewall problem**
(IP traffic already works, egress is already validated) — it's that the guest
never learns which DNS server to use, or it does but the guest doesn't apply it.
Two pieces, both already solved in this repo's code/base image, documented so any
new image takes them into account.

### Piece 1 — tell the guest which DNS to use (MicroHosted side, already done)

`firecracker-go-sdk`'s `IPConfiguration` accepts up to 2 `Nameservers`, but we
**never passed them** — only IP/gateway. `internal/firecracker/machine.go` now
sets `defaultNameservers = ["1.1.1.1", "8.8.8.8"]` for every VM with a network.

**Why public DNS and not the host's**: the host's resolver (e.g. systemd-resolved's
`127.0.0.53` stub) would be unreachable anyway — `guest→host` is blocked by design
(see the isolation section above). Using 1.1.1.1/8.8.8.8 is consistent with the
model: on an `egress:false` network those IPs are as unreachable as any other
external IP (DNS fails just as any WAN traffic would — correct, malware-safe); on
an `egress:true` network they're reachable via the same NAT that already goes to
the internet, so DNS works with no additional piece.

### Piece 2 — have the guest APPLY those nameservers (image side, in `prepare-image.sh`)

Here's the non-obvious part. The SDK does **not** edit the guest's
`/etc/resolv.conf` directly (it has no way to touch the rootfs filesystem from
outside). What it does is pass IP+gateway+nameservers as a kernel boot parameter
(`ip=`), a mechanism inherited from netboot/nfsroot. The **Linux kernel**, when
booting with that parameter, writes that configuration into `/proc/net/pnp` — a
pseudo-file with a syntax compatible with `/etc/resolv.conf` (`nameserver
X.X.X.X` lines).

But `/proc/net/pnp` having the DNS is useless if **nothing in the guest reads from
it**. Most modern distros (systemd-resolved, netplan, cloud-init, NetworkManager)
manage `/etc/resolv.conf` their own way — usually a symlink to their own stub —
and ignore `/proc/net/pnp` entirely. Without this step, the guest simply has no
nameserver configured, no matter what happens on the MicroHosted side.

**Fix**: `scripts/prepare-image.sh` now, on every image preparation
(unconditional, not dependent on `--no-ssh`/`--no-vsock` — it's basic networking,
not part of access), does:

```bash
# if /etc/resolv.conf was a real file, it's saved as .microhosted-orig
ln -sf /proc/net/pnp /etc/resolv.conf
```

With that, any guest program that reads `/etc/resolv.conf` (the standard way on
Linux) automatically gets the nameservers MicroHosted passed at boot — with no
custom agent, no systemd-resolved, no DHCP.

### Why this doesn't depend on the host (unlike the Docker problem)

Unlike the egress section (which depends on which firewall runs on **the host**),
this depends solely on **the guest image** — it's the same fix on any host, Docker
or not, ufw or not. That's why it lives in `prepare-image.sh` (applied once per
golden rootfs) and not in the daemon.

### If you prepare a new image from scratch (not the one this repo ships)

Always run `scripts/prepare-image.sh` over the rootfs before using it as a
template — it sets up both DNS resolution and access (vsock/SSH). If for some
reason you manage the rootfs by hand without this script, the only minimal
requirement for DNS is that `ln -sf /proc/net/pnp /etc/resolv.conf` line inside the
mounted rootfs.

### Quick diagnosis if DNS doesn't resolve but IPs do

```bash
# did the kernel receive and apply the nameservers?
curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{"cmd":"cat /proc/net/pnp"}'
# does resolv.conf point there?
curl -s -X POST localhost:8080/v1/vms/<id>/exec -d '{"cmd":"ls -la /etc/resolv.conf; cat /etc/resolv.conf"}'
# if /proc/net/pnp has the nameservers but resolv.conf isn't the symlink:
# the image didn't go through prepare-image.sh (or something overwrote it at boot,
# e.g. systemd-resolved) — re-run scripts/prepare-image.sh over the golden rootfs
# and create a NEW VM (already-cloned ones don't change retroactively).
```

**Important note for already-created VMs**: as with any `prepare-image.sh` change,
it only affects rootfs cloned **after** re-running it over the golden template —
`internal/storage.CloneRootfs` copies whatever was there at clone time. A VM that
already existed stays without DNS until it's destroyed and a new one is created
from the updated template.

## Decisions

- **Backend**: `nft` (nftables) for our own table `inet microhosted` — modern and
  isolated from the host firewall. Exception: Docker coexistence uses `iptables`
  (iptables-nft) for the `DOCKER-USER` chain, because that's the interface Docker
  manages and expects (see the egress section).
- **Default network**: on startup, `default` is created if it doesn't exist
  (`172.16.0.0/24`, `egress: false`). A VM with no `network` specified joins
  `default`.
- **Auto subnet**: if not given, a sequential `/24` is assigned from the
  `172.16.0.0/12` pool; the operator can set the subnet by hand.
- **Persistence**: networks are stored in SQLite. On startup, the daemon recreates
  bridges + rules from the persisted state (a network survives a host reboot, not
  just a daemon restart).

## API

```
POST   /v1/networks     {name, subnet?, egress?}
GET    /v1/networks
GET    /v1/networks/{name}
DELETE /v1/networks/{name}          # fails if it has connected VMs
POST   /v1/vms          {..., network: "<name>"}   # empty network = default
```

## Implementation status

- [x] Data model (`types.Network`) + persistence (`store` networks table)
- [x] Per-network IPAM (`network.Subnet`)
- [x] Bridge + enslaved TAP primitives (`network` bridge/tap)
- [x] Per-network nftables rules (`network.ApplyNftables`, declarative)
- [x] Orchestration (`network.Manager`: create/delete network, attach/detach VM) +
      bridge reconcile on startup
- [x] `/v1/networks` endpoints + `network` in VM create; `vm.Manager` moved from
      the `/30` model to bridges
- [x] Egress: auto `ip_forward` + Docker coexistence (`DOCKER-USER`)
- [x] **Validated on hardware (2026-07-02)**: inter-VM same network OK (0.5ms),
      isolation between networks OK, guest↛host OK, egress `false` blocked /
      egress `true` with NAT reaches `8.8.8.8` on a host with Docker, all with
      isolation intact.

> **Phase 1 CLOSED.** The chain of bugs that hardware validation uncovered (all
> leftovers of assumptions from the old `/30` model of "one VM = one isolated
> link", which the shared bridge breaks): hardcoded `/30` mask → broadcast; no
> unique MAC → bridge collision; `ip_forward=0`; Docker dropping the FORWARD. All
> resolved and documented above.
