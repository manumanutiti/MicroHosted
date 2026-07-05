package network

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"microhosted/internal/store"
	"microhosted/pkg/types"
)

// DefaultNetworkName is the network a VM joins when none is specified. It's
// created automatically at startup if absent.
const DefaultNetworkName = "default"

// defaultSubnet is the CIDR of the auto-created default network.
const defaultSubnet = "172.16.0.0/24"

// managedNet couples a persisted Network with its live IPAM and the set of VMs
// currently attached (so Delete can refuse a network still in use).
type managedNet struct {
	net    *types.Network
	subnet *Subnet
	vms    map[string]bool
}

// Manager owns the segmented networks: their bridges, per-network IPAM, and the
// nftables ruleset. It's the seam vm.Manager talks to for every network
// concern — attaching a VM, tearing one down, and rebuilding state at startup.
type Manager struct {
	store *store.Store

	mu   sync.Mutex
	nets map[string]*managedNet // by name
	pool *subnetPool
}

// NewManager wires a network Manager to the store it persists networks in.
func NewManager(st *store.Store) *Manager {
	return &Manager{
		store: st,
		nets:  make(map[string]*managedNet),
		pool:  newSubnetPool(),
	}
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

	m.mu.Lock()
	for _, n := range records {
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
	m.mu.Unlock()

	if !hasDefault {
		if _, err := m.Create(types.CreateNetworkRequest{Name: DefaultNetworkName, Subnet: defaultSubnet}); err != nil {
			return fmt.Errorf("creating default network: %w", err)
		}
	}

	return m.applyRules()
}

// Create defines a new network: allocates (or validates) its subnet, brings up
// its bridge, persists it, and reinstalls the nftables ruleset. Name uniqueness
// is enforced by the store's UNIQUE constraint.
func (m *Manager) Create(req types.CreateNetworkRequest) (*types.Network, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("network name is required")
	}
	if req.Egress && len(req.AllowedEgress) > 0 {
		return nil, fmt.Errorf("egress and allowed_egress are mutually exclusive: allowed_egress restricts a network whose egress is otherwise blocked")
	}
	if err := ValidateEgressRules(req.AllowedEgress); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if _, exists := m.nets[req.Name]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("network %q already exists", req.Name)
	}

	cidr := req.Subnet
	var err error
	if cidr == "" {
		cidr, err = m.pool.allocate()
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	subnet, err := ParseSubnet(cidr)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}

	id := uuid.NewString()[:8]
	n := &types.Network{
		ID:            id,
		Name:          req.Name,
		Bridge:        "mhbr" + id,
		Subnet:        cidr,
		Gateway:       subnet.Gateway(),
		Egress:        req.Egress,
		AllowedEgress: req.AllowedEgress,
		Intra:         req.Intra,
		CreatedAt:     time.Now(),
	}

	if err := CreateBridge(n.Bridge, subnet.GatewayCIDR()); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if err := m.store.SaveNetwork(n); err != nil {
		_ = DeleteBridge(n.Bridge)
		m.mu.Unlock()
		return nil, fmt.Errorf("persisting network %s: %w", n.Name, err)
	}
	m.pool.reserve(cidr)
	m.nets[n.Name] = &managedNet{net: n, subnet: subnet, vms: make(map[string]bool)}
	m.mu.Unlock()

	if err := m.applyRules(); err != nil {
		return nil, err
	}
	return n, nil
}

// UpdateEgress replaces a live network's egress policy and reinstalls the
// nftables ruleset. The point is firewall changes without touching attached
// VMs: bridge, subnet and allocated IPs are untouched, only the rules change.
// New flows obey the new policy immediately; flows established under the old
// policy are cut on the next packet too, because the forward chain matches
// statelessly (see renderNftables).
func (m *Manager) UpdateEgress(name string, req types.UpdateNetworkEgressRequest) (*types.Network, error) {
	if req.Egress && len(req.AllowedEgress) > 0 {
		return nil, fmt.Errorf("egress and allowed_egress are mutually exclusive: allowed_egress restricts a network whose egress is otherwise blocked")
	}
	if err := ValidateEgressRules(req.AllowedEgress); err != nil {
		return nil, err
	}

	m.mu.Lock()
	mn, ok := m.nets[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("network %q not found", name)
	}
	// Copy-on-write: persist the updated record before swapping it in, so a
	// store failure leaves both memory and disk on the old policy.
	updated := *mn.net
	updated.Egress = req.Egress
	updated.AllowedEgress = req.AllowedEgress
	if err := m.store.SaveNetwork(&updated); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("persisting network %s: %w", name, err)
	}
	mn.net = &updated
	m.mu.Unlock()

	if err := m.applyRules(); err != nil {
		return nil, err
	}
	return &updated, nil
}

// UpdateIntra flips a live network's VM↔VM policy. Persist-first copy-on-write
// like UpdateEgress; no nftables re-render (intra isn't in the ruleset — it's
// bridge-port isolation). The caller must then converge the network's LIVE
// taps via vm.Manager.SyncTapIsolation: tap devices belong to VMs, which this
// manager doesn't track by name. New taps pick the flag up on their own.
func (m *Manager) UpdateIntra(name string, intra bool) (*types.Network, error) {
	m.mu.Lock()
	mn, ok := m.nets[name]
	if !ok {
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
	m.mu.Lock()
	mn, ok := m.nets[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("network %q not found", name)
	}
	if len(mn.vms) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("network %q still has %d VM(s) attached", name, len(mn.vms))
	}

	if err := DeleteBridge(mn.net.Bridge); err != nil {
		m.mu.Unlock()
		return err
	}
	if err := m.store.DeleteNetwork(mn.net.ID); err != nil {
		m.mu.Unlock()
		return err
	}
	delete(m.nets, name)
	m.mu.Unlock()

	return m.applyRules()
}

// AttachVM allocates an IP for vmID on the named network and returns the guest
// IP, the gateway it routes through, the bridge its TAP must be enslaved to,
// and the subnet prefix length the guest must use.
func (m *Manager) AttachVM(networkName, vmID string) (guestIP, gateway, bridge string, prefixLen int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[networkName]
	if !ok {
		return "", "", "", 0, fmt.Errorf("network %q not found", networkName)
	}
	ip, err := mn.subnet.Allocate(vmID)
	if err != nil {
		return "", "", "", 0, err
	}
	mn.vms[vmID] = true
	return ip, mn.net.Gateway, mn.net.Bridge, mn.subnet.Prefix(), nil
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
// with its origin VM on the same bridge.
func (m *Manager) ClaimVM(networkName, vmID, ip string) (gateway, bridge string, prefixLen int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mn, ok := m.nets[networkName]
	if !ok {
		return "", "", 0, fmt.Errorf("network %q not found", networkName)
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
	if !ok {
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
		out = append(out, mn.net)
	}
	return out
}

// applyRules snapshots the current networks and reinstalls the nftables
// ruleset. Kept out of the locked sections that mutate m.nets so a slow `nft`
// call doesn't hold the manager lock.
func (m *Manager) applyRules() error {
	m.mu.Lock()
	snapshot := make([]types.Network, 0, len(m.nets))
	anyEgress := false
	for _, mn := range m.nets {
		snapshot = append(snapshot, *mn.net)
		// Restricted networks (AllowedEgress) need forwarding + NAT plumbing
		// just like full-egress ones; only the ruleset differs.
		if mn.net.Egress || len(mn.net.AllowedEgress) > 0 {
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
		if err := EnsureIPForward(); err != nil {
			log.Printf("network: enabling ip_forward failed (egress may not work): %v", err)
		}
		if err := EnsureDockerForwarding(); err != nil {
			log.Printf("network: DOCKER-USER coexistence failed (egress may not work under Docker): %v", err)
		}
	}
	return ApplyNftables(snapshot)
}
