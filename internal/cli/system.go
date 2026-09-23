package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"microhosted/pkg/types"
)

var systemGroup = &group{
	name:    "system",
	aliases: []string{"sys"},
	summary: "Platform health and host report",
	cmds: []*command{
		{name: "health", summary: "Run the platform's health checks (exit 1 if degraded)", run: sysHealth},
		{name: "info", summary: "Host, storage and fleet report", run: sysInfo},
		{name: "events", summary: "Follow what happens to VMs and the host (died, replaced, ruleset failed…)", run: sysEvents},
		{name: "doctor", summary: "Find residue and drift between the daemon and the host (exit 1 if any)", run: sysDoctor, help: doctorHelp},
	},
}

const doctorHelp = `Compares what the daemon believes exists with what the host actually has:
Firecracker processes, TAPs, bridges, jail dirs, cgroups, disk clones, console
logs, snapshot dirs, IP leases, volume claims, and whether the last firewall
ruleset applied. Read-only: it changes nothing. Residue is cleaned up by
restarting the daemon (its startup undoes interrupted creates and sweeps
processes, jail dirs and cgroups); disks it only reports.`

var templateGroup = &group{
	name:    "template",
	aliases: []string{"templates", "tpl"},
	summary: "Browse the template catalog",
	cmds: []*command{
		{name: "ls", aliases: []string{"list"}, summary: "List templates VMs can be created from", run: tplList},
	},
}

func sysHealth(e *env, cmd *command, p string, args []string) error {
	var asJSON bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(p, "unexpected argument %q (to look at one VM: mh inspect VM)", pos[0])
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	// 503 is the daemon's way of saying "degraded", with the same body: an
	// answer to print, not a failure to report.
	resp, err := c.Raw("/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return checkStatus(resp)
	}
	var h types.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return err
	}
	if asJSON {
		if err := printJSON(e.stdout, h); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(e.stdout, "status: %s\n\n", h.Status)
		printChecks(e.stdout, h.Checks)
	}
	if h.Status != "ok" {
		return exitError{code: 1}
	}
	return nil
}

func printChecks(w io.Writer, checks []types.HealthCheck) {
	rows := make([][]string, 0, len(checks))
	for _, ch := range checks {
		mark := "ok"
		if !ch.OK {
			mark = "FAIL"
		}
		rows = append(rows, []string{ch.Name, mark, ch.Detail})
	}
	table(w, []string{"CHECK", "", "DETAIL"}, rows)
}

func sysInfo(e *env, cmd *command, p string, args []string) error {
	var asJSON bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
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
	var s types.SystemResponse
	if err := c.Do("GET", "/v1/system", nil, &s); err != nil {
		return err
	}
	if asJSON {
		return printJSON(e.stdout, s)
	}

	w := e.stdout
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	d, h, st, f := s.Daemon, s.Host, s.Storage, s.Fleet
	fc := orDash(d.FirecrackerVersion)
	if d.NetworkOverrides {
		fc += " (network_overrides: simultaneous forks OK)"
	}
	cow := "NO copy-on-write: every VM is a full copy"
	if st.COW {
		cow = "copy-on-write"
	}
	fmt.Fprintf(tw, "Status:\t%s\n", s.Status)
	fmt.Fprintf(tw, "Daemon:\tpid %d, up %s\n", d.PID, fmtDuration(d.UptimeSeconds))
	fmt.Fprintf(tw, "Firecracker:\t%s\n", fc)
	fmt.Fprintf(tw, "Host:\t%s, kernel %s, %d CPUs, load %.2f %.2f %.2f\n", orDash(h.Hostname), orDash(h.Kernel), h.CPUs, h.Load1, h.Load5, h.Load15)
	fmt.Fprintf(tw, "Memory:\t%s used of %s, %s available\n", fmtMB(h.Memory.UsedMB), fmtMB(h.Memory.TotalMB), fmtMB(h.Memory.AvailableMB))
	fmt.Fprintf(tw, "Store:\t%s (%s, %s): %s used of %s, %s free\n", st.Path, st.FSType, cow, fmtMB(st.UsedMB), fmtMB(st.TotalMB), fmtMB(st.FreeMB))
	fmt.Fprintf(tw, "VMs:\t%d (%d running, %d stopped); running VMs promised %d vCPU, %s\n", f.VMs.Total, f.VMs.Running, f.VMs.Stopped, f.Allocated.VCPUs, fmtMB(f.Allocated.MemMB))
	fmt.Fprintf(tw, "Other:\t%d networks, %d snapshots, %d volumes (%d attached), %d templates\n", f.Networks, f.Snapshots, f.Volumes.Total, f.Volumes.Attached, f.Templates)
	if len(f.ManagedIfaces) > 0 {
		fmt.Fprintf(tw, "Managed ifaces:\t%s\n", strings.Join(f.ManagedIfaces, ", "))
	}
	tw.Flush()

	fmt.Fprintln(w)
	printChecks(w, s.Checks)

	if len(st.Breakdown) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(st.Breakdown))
		for _, b := range st.Breakdown {
			rows = append(rows, []string{b.What, fmtMB(b.SizeMB), b.Path})
		}
		table(w, []string{"STORE USAGE", "SIZE", "PATH"}, rows)
	}

	fmt.Fprintln(w)
	pp := d.Paths
	table(w, []string{"PATH", "WHERE"}, [][]string{
		{"store", pp.Store}, {"goldens", pp.Goldens}, {"kernels", pp.Kernels},
		{"snapshots", pp.Snapshots}, {"volumes", pp.Volumes}, {"chroot_base", pp.ChrootBase},
		{"database", pp.Database}, {"catalog", pp.Catalog},
		{"firecracker", pp.Firecracker}, {"jailer", pp.Jailer},
	})
	return nil
}

func tplList(e *env, cmd *command, p string, args []string) error {
	var quiet, asJSON bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&quiet, "quiet", "q", "print names only")
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
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
	var tpls []types.Template
	if err := c.Do("GET", "/v1/templates", nil, &tpls); err != nil {
		return err
	}
	sort.Slice(tpls, func(i, j int) bool { return tpls[i].Name < tpls[j].Name })
	switch {
	case asJSON:
		return printJSON(e.stdout, tpls)
	case quiet:
		for _, t := range tpls {
			fmt.Fprintln(e.stdout, t.Name)
		}
		return nil
	}
	rows := make([][]string, 0, len(tpls))
	notReady := 0
	for _, t := range tpls {
		disk := "-"
		if t.DiskMB > 0 {
			disk = fmtMB(t.DiskMB)
		}
		// A template whose golden is absent is listed (the catalog knows how to
		// build it) but cannot boot, so say so in the row rather than let `mh
		// run` be the one to find out. Keyed on Missing, not on !Ready: a daemon
		// older than these fields sends neither, and "no answer" must not read
		// as "nothing is built" — it would condemn every template on the host.
		status := "ready"
		if t.Missing != "" {
			status = "NOT BUILT"
			notReady++
		}
		rows = append(rows, []string{t.Name, status, strconv.FormatInt(t.VCPUs, 10), fmtMB(t.MemMB), disk, t.Description})
	}
	table(e.stdout, []string{"TEMPLATE", "STATUS", "VCPU", "MEM", "DISK", "DESCRIPTION"}, rows)
	if notReady > 0 {
		fmt.Fprintf(e.stderr, "\n%d template(s) NOT BUILT: their golden rootfs/kernel is not in this host's store.\n", notReady)
		fmt.Fprintf(e.stderr, "Build one with: make prepare-image            # base-alpine (default)\n")
		fmt.Fprintf(e.stderr, "                make prepare-image FLAVOR=ubuntu\n")
	}
	return nil
}

func sysDoctor(e *env, cmd *command, p string, args []string) error {
	var asJSON bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
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
	var rep types.DoctorReport
	if err := c.Do("GET", "/v1/doctor", nil, &rep); err != nil {
		return err
	}
	if asJSON {
		if err := printJSON(e.stdout, rep); err != nil {
			return err
		}
	} else {
		for _, op := range rep.InFlight {
			fmt.Fprintf(e.stdout, "in flight (skipped): %s\n", op)
		}
		if rep.Clean {
			fmt.Fprintln(e.stdout, "clean: the daemon and the host agree")
		} else {
			rows := make([][]string, 0, len(rep.Findings))
			for _, f := range rep.Findings {
				rows = append(rows, []string{f.Kind, f.Object, f.Detail})
			}
			table(e.stdout, []string{"KIND", "OBJECT", "DETAIL"}, rows)
		}
	}
	if !rep.Clean {
		return exitError{code: 1}
	}
	return nil
}
