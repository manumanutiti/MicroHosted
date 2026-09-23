package types

import "time"

// Event types. vm.* concern one VM (VM, Name, Labels and Network describe it
// as it was when the event happened); the rest concern the host.
const (
	EventVMCreated       = "vm.created"        // created or forked and running; Data: template | snapshot
	EventVMStarted       = "vm.started"        // a stopped VM booted again (Start, autostart)
	EventVMStopped       = "vm.stopped"        // powered off on purpose (Stop)
	EventVMDied          = "vm.died"           // its process died on its own; Reason says why, as far as the host can tell
	EventVMRestored      = "vm.restored"       // rewound in place to one of its snapshots; Data: snapshot
	EventVMDestroyed     = "vm.destroyed"      // gone, disk included
	EventVMQuarantined   = "vm.quarantined"    // cut off its network in place; Network is the one it left
	EventVMReplaced      = "vm.replaced"       // its function moved to another VM; Data: replacement, old (quarantine|stop|destroy)
	EventVMReplaceFailed = "vm.replace_failed" // cut off, but the replacement did not boot: the function is down; Reason
	EventVMAutostartFail = "vm.autostart_failed"
	EventRulesetFailed   = "network.ruleset_failed" // nft -f refused a ruleset; the policy in force may not be the declared one

	// EventReset is not something that happened on the host: it tells a
	// subscriber that it missed events it can no longer get — the daemon
	// restarted (new epoch), or it resumed from further back than the daemon
	// keeps — and must re-read the state it cares about (GET /v1/vms…) instead
	// of trusting its picture. Reason says which.
	EventReset = "reset"
)

// Event is one entry of GET /v1/events. Seq grows by one per event within an
// Epoch; the epoch changes every time the daemon starts, when Seq restarts.
// "<epoch>:<seq>" is the SSE id a subscriber hands back (Last-Event-ID) to
// resume where it left off.
type Event struct {
	Epoch   string            `json:"epoch"`
	Seq     uint64            `json:"seq"`
	Time    time.Time         `json:"time"`
	Type    string            `json:"type"`
	VM      string            `json:"vm,omitempty"`
	Name    string            `json:"name,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Network string            `json:"network,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Data    map[string]string `json:"data,omitempty"`
}
