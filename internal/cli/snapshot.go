package cli

import (
	"fmt"
	"sort"
	"strconv"

	"microhosted/pkg/types"
)

var snapshotGroup = &group{
	name:    "snapshot",
	aliases: []string{"snap", "snapshots"},
	summary: "Manage snapshots (frozen memory+disk of a VM)",
	cmds: []*command{
		{name: "create", aliases: []string{"new"}, args: "VM", summary: "Snapshot a running VM", run: snapCreate},
		{name: "ls", aliases: []string{"list"}, summary: "List snapshots", run: snapList},
		{name: "inspect", aliases: []string{"show"}, args: "SNAPSHOT...", summary: "Show a snapshot's detail as JSON", run: snapInspect},
		{name: "rm", aliases: []string{"remove", "delete"}, args: "SNAPSHOT...", summary: "Delete snapshots", run: snapRemove},
		{name: "fork", args: "SNAPSHOT", summary: "Boot a new VM from a snapshot", run: snapFork},
	},
}

func snapCreate(e *env, cmd *command, p string, args []string) error {
	var name string
	fs := newCmdFlags(e, p, cmd)
	fs.stringVar(&name, "name", "", "", "a label for the snapshot, e.g. clean")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one VM")
	}
	return createSnapshot(e, pos[0], name)
}

func snapList(e *env, cmd *command, p string, args []string) error {
	var quiet, asJSON bool
	var vmRef string
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&quiet, "quiet", "q", "print IDs only")
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
	fs.stringVar(&vmRef, "vm", "", "", "only snapshots taken from `VM`")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(p, "unexpected argument %q", pos[0])
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	snaps, err := c.listSnapshots()
	if err != nil {
		return err
	}
	if vmRef != "" {
		// The source VM may be gone (snapshots outlive it), so a full ID is
		// matched as-is rather than resolved against the live VM list.
		id := vmRef
		if resolved, err := c.resolveVM(vmRef); err == nil {
			id = resolved
		}
		kept := snaps[:0]
		for _, s := range snaps {
			if s.SourceVM == id {
				kept = append(kept, s)
			}
		}
		snaps = kept
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].CreatedAt > snaps[j].CreatedAt })
	switch {
	case asJSON:
		return printJSON(e.stdout, snaps)
	case quiet:
		for _, s := range snaps {
			fmt.Fprintln(e.stdout, s.ID)
		}
		return nil
	}
	rows := make([][]string, 0, len(snaps))
	for _, s := range snaps {
		rows = append(rows, []string{
			s.ID, orDash(s.Name), s.SourceVM, s.Template, orDash(s.Network), orDash(s.GuestIP),
			strconv.FormatInt(s.VCPUs, 10), fmtMB(s.MemMB), fmtAgo(s.CreatedAt),
		})
	}
	table(e.stdout, []string{"SNAPSHOT ID", "NAME", "SOURCE VM", "TEMPLATE", "NETWORK", "IP", "VCPU", "MEM", "CREATED"}, rows)
	return nil
}

func snapInspect(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one SNAPSHOT")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var out []types.SnapshotResponse
	for _, ref := range pos {
		id, err := c.resolveSnapshot(ref, "")
		if err != nil {
			return err
		}
		var s types.SnapshotResponse
		if err := c.Do("GET", "/v1/snapshots/"+id, nil, &s); err != nil {
			return err
		}
		out = append(out, s)
	}
	return printInspect(e.stdout, out)
}

func snapRemove(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one SNAPSHOT")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	return eachArg(e, pos, func(ref string) error {
		id, err := c.resolveSnapshot(ref, "")
		if err != nil {
			return err
		}
		if err := c.Do("DELETE", "/v1/snapshots/"+id, nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, id)
		return nil
	})
}

func snapFork(e *env, cmd *command, p string, args []string) error {
	var req types.ForkVMRequest
	var labels []string
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&req.Quarantine, "quarantine", "", "attach the new VM to no network (vsock only); allows many forks of one snapshot")
	forkIdentityFlags(fs, &req, &labels)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one SNAPSHOT")
	}
	if req.Labels, err = parseLabels(labels); err != nil {
		return usagef(p, "%v", err)
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	id, err := c.resolveSnapshot(pos[0], "")
	if err != nil {
		return err
	}
	var vm types.VMResponse
	if err := c.Do("POST", "/v1/snapshots/"+id+"/fork", req, &vm); err != nil {
		return err
	}
	announceVM(e, "forked", vm)
	return nil
}
