package network

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"microhosted/internal/events"
	"microhosted/internal/faults"
	"microhosted/internal/store"
	"microhosted/pkg/types"
)

// DefaultNetworkName is the network a VM joins when none is specified. It's
// created automatically at startup if absent.
const DefaultNetworkName = "default"

// defaultSubnet is the CIDR of the auto-created default network.
const defaultSubnet = "172.16.0.0/24"

// ErrInvalidIngress marks an ingress policy the caller got wrong — to_ip
// outside the subnet, a clash with another network's rule — as opposed to a
// failure of the host. The API turns it into a 400.
var ErrInvalidIngress = errors.New("invalid ingress policy")

// managedNet couples a persisted Network with its live IPAM and the set of VMs
// currently attached (so Delete can refuse a network still in use).
type managedNet struct {
	net    *types.Network
	subnet *Subnet
	vms    map[string]bool
	// pending marks a network whose rules are not in force yet (Create is
	// between bringing its bridge up and a successful apply). It holds its
	// name and subnet, but no VM may attach to it: a VM on a bridge the
	// ruleset does not cover would be outside the policy.
	pending bool
}

// Host-side operations, as variables so tests can make them fail without a
// kernel. Production code never reassigns them.
var (
	createBridge  = CreateBridge
	deleteBridge  = DeleteBridge
	applyNftables = ApplyNftables
	// ensureForwarding turns on kernel forwarding and clears Docker's blanket
	// FORWARD drop out of our bridges' way. Best-effort, see applyRules.
	ensureForwarding = func() {
		if err := EnsureIPForward(); err != nil {
			log.Printf("network: enabling ip_forward failed (egress may not work): %v", err)
		}
		if err := EnsureDockerForwarding(); err != nil {
			log.Printf("network: DOCKER-USER coexistence failed (egress may not work under Docker): %v", err)
		}
	}
)

// RulesStatus is the outcome of the last attempt to install the ruleset — what
// the doctor reports when the policy in force may not be the one declared.
type RulesStatus struct {
	OK  bool
	Err string
	At  time.Time
}

// Manager owns the segmented networks: their bridges, per-network IPAM, and the
// nftables ruleset. It's the seam vm.Manager talks to for every network
// concern — attaching a VM, tearing one down, and rebuilding state at startup.
type Manager struct {
	store *store.Store

	// managed are the host interfaces this daemon owns the whole nftables
	// policy for (see ManagedIface). Declared once at startup and never
	// mutated, so they need no lock. Empty means the daemon renders policy
	// for no external interface and rejects every egress rule that names
	// one — taking over an interface the operator did not hand us is not
	// ours to decide.
	managed []ManagedIface

	// applyMu serializes every change to the policy together with its apply:
	// the in-memory change, the `nft -f` and the rollback when it fails, as
	// one step. Without it two concurrent creates could each snapshot the
	// networks, and the older snapshot could be the one installed last. It is
	// taken before mu, never while holding it. AttachVM and friends only take
	// mu, so a slow `nft` never blocks a VM from being created on an existing
	// network.
	applyMu sync.Mutex
	rules   RulesStatus // guarded by mu

	mu   sync.Mutex
	nets map[string]*managedNet // by name
	pool *subnetPool

	// events receives ruleset failures (see SetEvents); nil drops them.
	events *events.Bus
}

// NewManager wires a network Manager to the store it persists networks in,
// and to the interfaces whose nftables policy it was told to own.
func NewManager(st *store.Store, managed []ManagedIface) *Manager {
	return &Manager{
		store:   st,
		managed: managed,
		nets:    make(map[string]*managedNet),
		pool:    newSubnetPool(),
	}
}

// ManagedIfaceNames returns the interfaces this daemon owns the policy for.
// The observability report shows them because "which interfaces am I
// responsible for" should be answerable from the API, not only from the
// unit file.
func (m *Manager) ManagedIfaceNames() []string {
	names := make([]string, 0, len(m.managed))
	for _, i := range m.managed {
		names = append(names, i.Name)
	}
	return names
}

// UnenforcedRules lists the stored egress and ingress rules that name an
// interface this daemon does not manage — typically because it was restarted without the
// --managed-iface it had when the rules were created. They are NOT rendered
// (see renderNftables): a hole through an interface is only safe inside the
// deny-both-ways policy that declaring it installs, and that policy is gone.
// So the API reports a rule that is not in force. The health check surfaces
// the mismatch instead of leaving an operator to find out from a dead flow.
// Each entry reads "network ot: tcp:192.168.50.52:502@wlan0" (egress) or
// "network ot: ingress tcp:192.168.50.60:1883@wlan0=172.16.9.2".
func (m *Manager) UnenforcedRules() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, mn := range m.nets {
		if mn.net.Egress && mn.net.EgressIface == "" {
			out = append(out, fmt.Sprintf("network %s: egress without egress_iface (blocked until it names one)", mn.net.Name))
		}
		for _, r := range mn.net.AllowedEgress {
			if r.Iface != "" && !isManaged(m.managed, r.Iface) {
				out = append(out, fmt.Sprintf("network %s: %s", mn.net.Name, describeRule(r)))
			}
		}
		for _, r := range mn.net.AllowedIngress {
			if !isManaged(m.managed, r.Iface) {
				out = append(out, fmt.Sprintf("network %s: ingress %s", mn.net.Name, describeIngressRule(r)))
			}
		}
	}
	sort.Strings(out)
	return out
}

func isManaged(managed []ManagedIface, name string) bool {
	for _, mi := range managed {
		if mi.Name == name {
			return true
		}
	}
	return false
}

// describeRule renders a rule in the CLI's PROTO:IP[:PORT][@IFACE] form.
func describeRule(r types.EgressRule) string {
	s := r.Protocol + ":" + r.IP
	if r.Port != 0 {
		s += ":" + strconv.Itoa(r.Port)
	}
	if r.Iface != "" {
		s += "@" + r.Iface
	}
	return s
}

// describeIngressRule renders a rule in the CLI's PROTO:SRC:PORT@IFACE=TO_IP form.
func describeIngressRule(r types.IngressRule) string {
	return fmt.Sprintf("%s:%s:%d@%s=%s", r.Protocol, r.SrcIP, r.Port, r.Iface, r.ToIP)
}

// checkIngress vets rules against what only the Manager knows: the target
// network's subnet (every to_ip must be a guest address in it) and the other
// networks' ingress rules (the prerouting chain is shared, so a clash with any
// of them would leave one rule silently dead). Call with m.mu held; `self` is
// the network being changed, skipped so a replace doesn't clash with itself.
func (m *Manager) checkIngress(self string, subnet *Subnet, rules []types.IngressRule) error {
	for i, r := range rules {
		if err := subnet.CheckGuestIP(r.ToIP); err != nil {
			return fmt.Errorf("%w: rule %d: to_ip %s", ErrInvalidIngress, i, err)
		}
		for _, other := range m.nets {
			if other.net.Name == self {
				continue
			}
			for _, o := range other.net.AllowedIngress {
				if IngressClash(r, o) {
					return fmt.Errorf("%w: rule %d (%s) clashes with network %s's %s",
						ErrInvalidIngress, i, describeIngressRule(r), other.net.Name, describeIngressRule(o))
				}
			}
		}
	}
	return nil
}

// Reconcile rebuilds network state from the store at startup: recreates any
// bridge a host reboot wiped, re-registers each network's subnet, ensures the
// default network exists, and installs the nftables ruleset. Call before
// vm.Manager.Reconcile, which then re-reserves each adopted VM's IP.
func (m *Manager) Reconcile() error {
	records, err := m.store.ListNetworks()
	if err != nil {
		return fmt.Errorf("loading networks: %w", err)
	}

	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	for _, n := range records {
		m.migrateEgressIface(n)
		subnet, err := ParseSubnet(n.Subnet)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("network %s has invalid subnet %q: %w", n.Name, n.Subnet, err)
		}
		m.pool.reserve(n.Subnet)
		m.nets[n.Name] = &managedNet{net: n, subnet: subnet, vms: make(map[string]bool)}

		if !BridgeExists(n.Bridge) {
			if err := CreateBridge(n.Bridge, subnet.GatewayCIDR()); err != nil {
				m.mu.Unlock()
				return fmt.Errorf("recreating bridge for network %s: %w", n.Name, err)
			}
			log.Printf("network reconcile: recreated bridge %s for network %s", n.Bridge, n.Name)
		}
	}
	_, hasDefault := m.nets[DefaultNetworkName]
	known := make(map[string]bool, len(m.nets))
	for _, mn := range m.nets {
		known[mn.net.Bridge] = true
	}
	m.mu.Unlock()

	// A bridge of ours with no record is what a crash between bringing a
	// network's bridge up and persisting it leaves. It is already dark (the
	// ruleset drops unknown bridges); removing it just stops it accumulating.
	if bridges, err := listLinks(BridgePrefix); err != nil {
		log.Printf("network reconcile: listing bridges: %v", err)
	} else {
		for _, br := range bridges {
			if known[br] {
				continue
			}
			if err := deleteBridge(br); err != nil {
				log.Printf("network reconcile: removing orphan bridge %s: %v", br, err)
				continue
			}
			log.Printf("network reconcile: removed orphan bridge %s (no network record)", br)
		}
	}

	if !hasDefault {
		if _, err := m.create(types.CreateNetworkRequest{Name: DefaultNetworkName, Subnet: defaultSubnet}); err != nil {
			return fmt.Errorf("creating default network: %w", err)
		}
	}

	for _, r := range m.UnenforcedRules() {
		log.Printf("WARNING: egress rule NOT applied — %s. "+
			"An interface-scoped rule needs --managed-iface for that interface (make install-service MANAGED_IFACE=...); "+
			"full egress needs its exit interface (mh network update NAME --internet IFACE); or remove the rule", r)
	}
	return m.applyRules()
}

// migrateEgressIface gives an Egress network persisted before EgressIface
// existed the interface its old policy meant: it was masqueraded "out the
// host's default route", so that route's interface is the one it keeps. If
// there is no default route, or it goes through an interface this daemon
// manages, the network is left without one — rendered closed and reported by
// UnenforcedRules — rather than guessed. Call with m.mu held.
func (m *Manager) migrateEgressIface(n *types.Network) {
	if !n.Egress || n.EgressIface != "" {
		return
	}
	iface, err := defaultRouteIface()
	if err != nil || ValidateEgressPolicy(true, iface, nil, m.ManagedIfaceNames()) != nil {
		log.Printf("network reconcile: network %s has egress but no egress_iface, and the default route gives none usable (%v): its egress stays CLOSED until one is set", n.Name, err)
		return
	}
	n.EgressIface = iface
	if err := m.store.SaveNetwork(n); err != nil {
		log.Printf("network reconcile: persisting egress_iface %s for network %s: %v (applied for this run only)", iface, n.Name, err)
	}
	log.Printf("network reconcile: network %s had egress with no exit interface; pinned to %s, the default route's", n.Name, iface)
}

// Create defines a new network: allocates (or validates) its subnet, brings up
// its bridge, installs the nftables ruleset with it, and only then persists it
// and makes it attachable. If the ruleset cannot be applied the network is
// rolled back entirely — a bridge the policy in force does not cover is the
// one thing that must never be handed a VM. Name uniqueness is enforced by the
// store's UNIQUE constraint and, before that, by the in-memory map.
func (m *Manager) Create(req types.CreateNetworkRequest) (*types.Network, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	return m.create(req)
}

// create is Create with applyMu already held (Reconcile holds it while it
// creates the default network).
func (m *Manager) create(req types.CreateNetworkRequest) (*types.Network, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("network name is required")
	}
	if err := ValidateEgressPolicy(req.Egress, req.EgressIface, req.AllowedEgress, m.ManagedIfaceNames()); err != nil {
		return nil, err
	}
	if err := ValidateIngressRules(req.AllowedIngress, m.ManagedIfaceNames()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidIngress, err)
	}
	// An auto-allocated subnet is unknown to the caller, so no to_ip could
	// have been chosen inside it on purpose.
	if len(req.AllowedIngress) > 0 && req.Subnet == "" {
		return nil, fmt.Errorf("%w: allowed_ingress needs an explicit subnet (every to_ip must fall inside it); "+
			"or create the network first and add the rules with PUT /v1/networks/{name}/ingress", ErrInvalidIngress)
	}

	m.mu.Lock()
	if _, exists := m.nets[req.Name]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("network %q already exists", req.Name)
	}

	cidr := req.Subnet
	var err error
	autoAllocated := cidr == ""
	if autoAllocated {
		cidr, err = m.pool.allocate()
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	// An auto-allocated subnet is held in the pool from here on, and every
	// early return gives it back. A caller-chosen one is not ours to release
	// yet — it may be another network's, which is what the checks below
	// refuse.
	fail := func(err error) (*types.Network, error) {
		if autoAllocated {
			m.pool.release(cidr)
		}
		m.mu.Unlock()
		return nil, err
	}
	subnet, err := ParseSubnet(cidr)
	if err != nil {
		return fail(err)
	}
	if other := m.overlapping(subnet); other != "" {
		// Two bridges with overlapping addresses make the host's routing to
		// either of them a coin toss.
		return fail(fmt.Errorf("subnet %s overlaps network %s's", cidr, other))
	}
	if err := m.checkIngress(req.Name, subnet, req.AllowedIngress); err != nil {
		return fail(err)
	}

	id := uuid.NewString()[:8]
	n := &types.Network{
		ID:             id,
		Name:           req.Name,
		Bridge:         BridgePrefix + id,
		Subnet:         cidr,
		Gateway:        subnet.Gateway(),
		Egress:         req.Egress,
		EgressIface:    req.EgressIface,
		AllowedEgress:  req.AllowedEgress,
		AllowedIngress: req.AllowedIngress,
		Intra:          req.Intra,
		CreatedAt:      time.Now(),
	}
	m.pool.reserve(cidr)
	m.nets[n.Name] = &managedNet{net: n, subnet: subnet, vms: make(map[string]bool), pending: true}
	m.mu.Unlock()

	// Undo everything above; the bridge is removed only once no ruleset can
	// still be relying on it.
	rollback := func() {
		m.mu.Lock()
		delete(m.nets, n.Name)
		m.pool.release(cidr)
		m.mu.Unlock()
		if err := deleteBridge(n.Bridge); err != nil {
			log.Printf("network %s: rolling back bridge %s: %v (dark: the ruleset drops unknown bridges)", n.Name, n.Bridge, err)
		}
	}

	// The bridge comes up outside the ruleset in force, and that is safe:
	// the ruleset drops any bridge it does not list, so it is dark until the
	// apply below lists it.
	if err := createBridge(n.Bridge, subnet.GatewayCIDR()); err != nil {
		rollback()
		return nil, err
	}
	if err := faults.Check("network.create.after-bridge"); err != nil {
		rollback()
		return nil, err
	}
	if err := m.applyRules(); err != nil {
		rollback()
		return nil, err
	}
	err = faults.Check("network.create.after-apply")
	if err == nil {
		err = m.store.SaveNetwork(n)
	}
	if err != nil {
		rollback()
		if rerr := m.applyRules(); rerr != nil {
			log.Printf("network %s: re-applying the ruleset after a failed create: %v", n.Name, rerr)
		}
		return nil, fmt.Errorf("persisting network %s: %w", n.Name, err)
	}

	m.mu.Lock()
	m.nets[n.Name].pending = false
	m.mu.Unlock()
	return n, nil
}

// overlapping names the network whose subnet overlaps s, or "" if none. Call
// with m.mu held.
func (m *Manager) overlapping(s *Subnet) string {
	for _, mn := range m.nets {
		if mn.subnet.Overlaps(s) {
			return mn.net.Name
		}
	}
	return ""
}

// UpdateEgress replaces a live network's egress policy and reinstalls the
// nftables ruleset. The point is firewall changes without touching attached
// VMs: bridge, subnet and allocated IPs are untouched, only the rules change.
// New flows obey the new policy immediately; flows established under the old
// policy are cut on the next packet too, because the forward chain matches
// statelessly (see renderNftables).
func (m *Manager) UpdateEgress(name string, req types.UpdateNetworkEgressRequest) (*types.Network, error) {
	if err := ValidateEgressPolicy(req.Egress, req.EgressIface, req.AllowedEgress, m.ManagedIfaceNames()); err != nil {
		return nil, err
	}
	return m.updatePolicy(name, func(n *types.Network) error {
		n.Egress = req.Egress
		n.EgressIface = req.EgressIface
		n.AllowedEgress = req.AllowedEgress
		return nil
	})
}

// updatePolicy swaps a live network's record for a modified copy, installs the
// ruleset and persists — in that order, so that what the API reports is the
// policy in force: if the apply fails, memory goes back to the old record and
// the store never saw the new one; if persisting fails, the old policy is
// re-applied. change runs with m.mu held and may reject the update.
func (m *Manager) updatePolicy(name string, change func(*types.Network) error) (*types.Network, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	mn, ok := m.nets[name]
	if !ok || mn.pending {
		m.mu.Unlock()
		return nil, fmt.Errorf("network %q not found", name)
	}
	prev := mn.net
	updated := *prev
	if err := change(&updated); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	mn.net = &updated
	m.mu.Unlock()

	revert := func() {
		m.mu.Lock()
		mn.net = prev
		m.mu.Unlock()
	}
	if err := m.applyRules(); err != nil {
		revert()
		return nil, err
	}
	if err := m.store.SaveNetwork(&updated); err != nil {
		revert()
		if rerr := m.applyRules(); rerr != nil {
			log.Printf("network %s: re-applying the previous policy after a failed save: %v", name, rerr)
		}
		return nil, fmt.Errorf("persisting network %s: %w", name, err)
	}
	return &updated, nil
}

// UpdateIngress replaces a live network's ingress policy and reinstalls the
// ruleset, like UpdateEgress: attached VMs are untouched, and a removed rule
// cuts its live flows on their next packet (the forward legs are the only
// accept; the DNAT binding a flow keeps in conntrack leads into the drop).
func (m *Manager) UpdateIngress(name string, rules []types.IngressRule) (*types.Network, error) {
	if err := ValidateIngressRules(rules, m.ManagedIfaceNames()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidIngress, err)
	}
	return m.updatePolicy(name, func(n *types.Network) error {
		if err := m.checkIngress(name, m.nets[name].subnet, rules); err != nil {
			return err
		}
		n.AllowedIngress = rules
		return nil
	})
}

// UpdateIntra flips a live network's VM↔VM policy. Persist-first copy-on-write
// like UpdateEgress; no nftables re-render (intra isn't in the ruleset — it's
// bridge-port isolation). The caller must then converge the network's LIVE
// taps via vm.Manager.SyncTapIsolation: tap devices belong to VMs, which this
// manager doesn't track by name. New taps pick the flag up on their own.
func (m *Manager) UpdateIntra(name string, intra bool) (*types.Network, error) {
	m.mu.Lock()
	mn, ok := m.nets[name]
	if !ok || mn.pending {
		m.mu.Unlock()
		return nil, fmt.Errorf("network %q not found", name)
	}
	updated := *mn.net
	updated.Intra = intra
	if err := m.store.SaveNetwork(&updated); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("persisting network %s: %w", name, err)
	}
	mn.net = &updated
	m.mu.Unlock()
	return &updated, nil
}

// Delete removes a network. It refuses while any VM is still attached — the
// caller must destroy those VMs first.
func (m *Manager) Delete(name string) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	mn, ok := m.nets[name]
	if !ok || mn.pending {
		m.mu.Unlock()
		return fmt.Errorf("network %q not found", name)
	}
	if len(mn.vms) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("network %q still has %d VM(s) attached", name, len(mn.vms))
	}

	if err := deleteBridge(mn.net.Bridge); err != nil {
		m.mu.Unlock()
		return err
	}
	if err := m.store.DeleteNetwork(mn.net.ID); err != nil {
		m.mu.Unlock()
		return err
	}
	delete(m.nets, name)
	m.pool.release(mn.net.Subnet)
	m.mu.Unlock()

	// The bridge is gone, so a failure here leaves a stale entry in the
	// ruleset for a device that no longer exists — closed, not open.
	return m.applyRules()
}

// AttachVM allocates an IP for vmID on the named network and returns the guest
// IP, the gateway it routes through, the bridge its TAP must be enslaved to,
// and the subnet prefix length the guest must use.
func (m *Manager) AttachVM(networkName, vmID string) (guestIP, gateway, bridge string, prefixLen int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[networkName]
	if !ok || mn.pending {
		return "", "", "", 0, fmt.Errorf("network %q not found", networkName)
	}
	ip, err := mn.subnet.AllocateAvoiding(vmID, pinnedIPs(mn.net))
	if err != nil {
		return "", "", "", 0, err
	}
	mn.vms[vmID] = true
	return ip, mn.net.Gateway, mn.net.Bridge, mn.subnet.Prefix(), nil
}

// pinnedIPs are the addresses of n that ingress rules point at (to_ip). An
// address belongs to the function behind it, not to whichever VM holds it
// now: when that VM goes (destroyed, quarantined), AttachVM must not hand the
// address to an unrelated new VM, which would silently receive the traffic
// meant for the function. Only ClaimVM — a replacement asking for it by
// address — takes a pinned address. Derived from the policy in force, so it
// follows every ingress update and rollback without state of its own.
func pinnedIPs(n *types.Network) map[string]bool {
	if len(n.AllowedIngress) == 0 {
		return nil
	}
	pinned := make(map[string]bool, len(n.AllowedIngress))
	for _, r := range n.AllowedIngress {
		pinned[r.ToIP] = true
	}
	return pinned
}

// ReserveVM re-registers an adopted VM's IP on its network at startup, so the
// address isn't later handed to a new VM. Missing network is a soft error (the
// VM is still adopted; its network is just inconsistent) — logged, not fatal.
func (m *Manager) ReserveVM(networkName, vmID, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[networkName]
	if !ok {
		log.Printf("network reconcile: vm %s references unknown network %q", vmID, networkName)
		return
	}
	if err := mn.subnet.Reserve(vmID, ip); err != nil {
		log.Printf("network reconcile: reserving %s for vm %s on %s: %v", ip, vmID, networkName, err)
		return
	}
	mn.vms[vmID] = true
}

// ClaimVM attaches vmID to the named network at one specific address — the
// fork-from-snapshot path, where the guest's IP is frozen inside the restored
// memory and can't be reallocated. Unlike ReserveVM (adopt-on-restart, which
// tolerates re-registering because the address was already this VM's), this
// fails if the address is held by anyone else, so a fork can never collide
// with its origin VM on the same bridge. It is also the only way to take a
// pinned address (see pinnedIPs): a create asking for guest_ip, or a fork.
func (m *Manager) ClaimVM(networkName, vmID, ip string) (gateway, bridge string, prefixLen int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[networkName]
	if !ok || mn.pending {
		return "", "", 0, fmt.Errorf("network %q not found", networkName)
	}
	if err := mn.subnet.CheckGuestIP(ip); err != nil {
		return "", "", 0, fmt.Errorf("%w: %v", ErrBadAddress, err)
	}
	if err := mn.subnet.ReserveExclusive(vmID, ip); err != nil {
		return "", "", 0, err
	}
	mn.vms[vmID] = true
	return mn.net.Gateway, mn.net.Bridge, mn.subnet.Prefix(), nil
}

// DetachVM releases vmID's IP back to its network's pool.
func (m *Manager) DetachVM(networkName, vmID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mn, ok := m.nets[networkName]; ok {
		mn.subnet.Release(vmID)
		delete(mn.vms, vmID)
	}
}

// Intra reports whether the named network allows VM↔VM traffic — the flag the
// TAP-creation paths turn into bridge-port isolation. Unknown network → false,
// the most restrictive answer (the attach itself will fail anyway).
func (m *Manager) Intra(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[name]
	return ok && mn.net.Intra
}

// Get returns a network by name.
func (m *Manager) Get(name string) (*types.Network, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[name]
	if !ok || mn.pending {
		return nil, false
	}
	return mn.net, true
}

// List returns every network.
func (m *Manager) List() []*types.Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*types.Network, 0, len(m.nets))
	for _, mn := range m.nets {
		if !mn.pending {
			out = append(out, mn.net)
		}
	}
	return out
}

// applyRules snapshots the current networks and reinstalls the nftables
// ruleset. Callers hold applyMu, so the snapshot installed last is always the
// newest; mu is only held to take the snapshot, so a slow `nft` call doesn't
// block VM attaches.
func (m *Manager) applyRules() error {
	m.mu.Lock()
	snapshot := make([]types.Network, 0, len(m.nets))
	anyEgress := false
	for _, mn := range m.nets {
		snapshot = append(snapshot, *mn.net)
		// Restricted networks (AllowedEgress) need forwarding + NAT plumbing
		// just like full-egress ones; only the ruleset differs. So does
		// ingress: a DNATed flow is routed, not delivered to the host.
		if mn.net.Egress || len(mn.net.AllowedEgress) > 0 || len(mn.net.AllowedIngress) > 0 {
			anyEgress = true
		}
	}
	m.mu.Unlock()

	// The masquerade rules are useless without kernel forwarding — turn it on
	// as soon as any network wants egress, and clear Docker's blanket FORWARD
	// drop out of our bridges' way if Docker is present. Both are best-effort:
	// a failure here degrades egress but must NOT crash-loop the daemon (which
	// would take down every VM's management), so it's logged, not propagated.
	// The nftables apply below is the authoritative part and still errors hard.
	if anyEgress {
		ensureForwarding()
	}
	err := applyNftables(snapshot, m.managed)
	m.mu.Lock()
	m.rules = RulesStatus{OK: err == nil, At: time.Now()}
	if err != nil {
		m.rules.Err = err.Error()
	}
	m.mu.Unlock()
	if err != nil {
		m.events.Publish(types.Event{Type: types.EventRulesetFailed, Reason: err.Error()})
	}
	return err
}

// SetEvents connects the manager to the event bus, for ruleset failures. Call
// before Reconcile; without it nothing is published.
func (m *Manager) SetEvents(b *events.Bus) {
	m.events = b
}

// Rules reports the outcome of the last ruleset install.
func (m *Manager) Rules() RulesStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rules
}

// Leases returns, per network, the VM IDs holding an address on it — what the
// doctor compares against the VMs the manager tracks.
func (m *Manager) Leases() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]string, len(m.nets))
	for name, mn := range m.nets {
		for id := range mn.vms {
			out[name] = append(out[name], id)
		}
	}
	return out
}
