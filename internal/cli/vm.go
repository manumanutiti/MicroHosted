package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"microhosted/pkg/types"
)

var vmGroup = &group{
	name:    "vm",
	aliases: []string{"vms"},
	summary: "Manage microVMs",
	cmds: []*command{
		{name: "create", aliases: []string{"run", "new"}, args: "TEMPLATE", summary: "Create and boot a VM from a template", run: vmCreate},
		{name: "ls", aliases: []string{"list", "ps"}, summary: "List VMs (running ones; -a for all)", run: vmList},
		{name: "inspect", aliases: []string{"show"}, args: "VM...", summary: "Show a VM's full detail as JSON", run: vmInspect},
		{name: "rm", aliases: []string{"remove", "delete"}, args: "VM... | --all", summary: "Destroy VMs (disk included)", run: vmRemove},
		{name: "stop", args: "VM...", summary: "Power a VM off, keeping its disk and IP", run: vmStop},
		{name: "start", args: "VM...", summary: "Boot a stopped VM again", run: vmStart},
		{name: "exec", args: "VM COMMAND...", summary: "Run a command inside a VM (over vsock)", run: vmExec},
		{name: "cp", args: "VM:PATH LOCAL | LOCAL VM:PATH", summary: "Copy a file between a VM and the host", run: vmCopy},
		{name: "logs", args: "VM", summary: "Show a VM's console log (same host only)", run: vmLogs},
		{name: "snapshot", args: "VM", summary: "Snapshot a running VM", run: vmSnapshot},
		{name: "fork", args: "VM", summary: "Clone a running VM into a new one", run: vmFork},
		{name: "restore", args: "VM SNAPSHOT", summary: "Rewind a VM in place to one of its snapshots", run: vmRestore},
	},
}

func vmCreate(e *env, cmd *command, p string, args []string) error {
	var req types.CreateVMRequest
	var volumes []string
	fs := newCmdFlags(e, p, cmd)
	fs.int64Var(&req.VCPUs, "cpus", "c", "vCPUs (default: the template's)")
	fs.Var((*mbValue)(&req.MemMB), "mem", "memory `SIZE`, e.g. 256 (MiB) or 1G (default: the template's)")
	fs.alias("mem", "m")
	fs.Var((*mbValue)(&req.DiskMB), "disk", "disk `SIZE`, only grows the template's, e.g. 2G")
	fs.stringVar(&req.Network, "net", "n", "", "`NETWORK` to attach to (default: \"default\")")
	fs.boolVar(&req.NoNetwork, "no-net", "", "no network at all: reachable over vsock only (exec, cp)")
	fs.listVar(&volumes, "volume", "v", "attach volume `NAME[:GUEST_PATH][:ro]` (repeatable; default path /vol/NAME)")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one TEMPLATE, got %d arguments", len(pos))
	}
	if req.NoNetwork && req.Network != "" {
		return usagef(p, "--net and --no-net are mutually exclusive")
	}
	req.Template = pos[0]
	for _, v := range volumes {
		va, err := parseVolumeSpec(v)
		if err != nil {
			return usagef(p, "%v", err)
		}
		req.Volumes = append(req.Volumes, va)
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var vm types.VMResponse
	if err := c.Do("POST", "/v1/vms", req, &vm); err != nil {
		return err
	}
	announceVM(e, "created", vm)
	return nil
}

// announceVM prints the new VM's ID on stdout — so VM=$(mh run base-alpine)
// works — and the details a person wants next on stderr.
func announceVM(e *env, verb string, vm types.VMResponse) {
	fmt.Fprintln(e.stdout, vm.ID)
	where := "no network"
	switch {
	case vm.Quarantine:
		where = "quarantined, believes it is " + vm.GuestIP
	case vm.Network != "":
		where = vm.Network + " " + vm.GuestIP
	}
	fmt.Fprintf(e.stderr, "%s %s: %s, %s\n", verb, vm.ID, vm.Template, where)
}

// parseVolumeSpec reads docker's -v shape: NAME, NAME:ro, NAME:/mnt/x,
// NAME:/mnt/x:ro.
func parseVolumeSpec(s string) (types.VolumeAttachRequest, error) {
	parts := strings.Split(s, ":")
	va := types.VolumeAttachRequest{Name: parts[0]}
	rest := parts[1:]
	if n := len(rest); n > 0 && (rest[n-1] == "ro" || rest[n-1] == "rw") {
		va.ReadOnly = rest[n-1] == "ro"
		rest = rest[:n-1]
	}
	switch {
	case va.Name == "":
		return va, fmt.Errorf("volume %q: missing name", s)
	case len(rest) > 1:
		return va, fmt.Errorf("volume %q: expected NAME[:GUEST_PATH][:ro]", s)
	case len(rest) == 1:
		if !strings.HasPrefix(rest[0], "/") {
			return va, fmt.Errorf("volume %q: guest path must be absolute", s)
		}
		va.GuestPath = rest[0]
	}
	return va, nil
}

func vmList(e *env, cmd *command, p string, args []string) error {
	var all, quiet, asJSON bool
	var network string
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&all, "all", "a", "include stopped and failed VMs")
	fs.boolVar(&quiet, "quiet", "q", "print IDs only")
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
	fs.stringVar(&network, "net", "n", "", "only VMs on `NETWORK`")
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
	vms, err := c.listVMs()
	if err != nil {
		return err
	}
	shown := vms[:0]
	for _, v := range vms {
		if !all && (v.State == types.VMStateStopped || v.State == types.VMStateFailed) {
			continue
		}
		if network != "" && v.Network != network {
			continue
		}
		shown = append(shown, v)
	}
	// Newest first, like docker ps.
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].CreatedAt > shown[j].CreatedAt })

	switch {
	case asJSON:
		return printJSON(e.stdout, shown)
	case quiet:
		for _, v := range shown {
			fmt.Fprintln(e.stdout, v.ID)
		}
		return nil
	}
	rows := make([][]string, 0, len(shown))
	for _, v := range shown {
		net := orDash(v.Network)
		if v.Quarantine {
			net = "(quarantine)"
		}
		rss, uptime := "-", "-"
		if v.State == types.VMStateRunning {
			rss = fmtMB(v.MemRSSMB)
			uptime = fmtDuration(v.UptimeSeconds)
		}
		rows = append(rows, []string{
			v.ID, v.Template, string(v.State), net, orDash(v.GuestIP),
			strconv.FormatInt(v.VCPUs, 10), fmtMB(v.MemMB), rss, uptime, fmtAgo(v.CreatedAt),
		})
	}
	table(e.stdout, []string{"VM ID", "TEMPLATE", "STATE", "NETWORK", "IP", "VCPU", "MEM", "RSS", "UPTIME", "CREATED"}, rows)
	return nil
}

func vmInspect(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one VM")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var out []types.VMResponse
	for _, ref := range pos {
		id, err := c.resolveVM(ref)
		if err != nil {
			return err
		}
		var vm types.VMResponse
		if err := c.Do("GET", "/v1/vms/"+id, nil, &vm); err != nil {
			return err
		}
		out = append(out, vm)
	}
	return printInspect(e.stdout, out)
}

func vmRemove(e *env, cmd *command, p string, args []string) error {
	var all bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&all, "all", "a", "destroy EVERY VM the daemon manages")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	c, err := e.api()
	switch {
	case err != nil:
		return err
	case all && len(pos) > 0:
		return usagef(p, "give either VM IDs or --all, not both")
	case all:
		var res types.BulkDeleteResponse
		if err := c.Do("DELETE", "/v1/vms", nil, &res); err != nil {
			return err
		}
		return reportBulk(e, res)
	case len(pos) == 0:
		return usagef(p, "expected at least one VM (or --all)")
	}
	return eachArg(e, pos, func(ref string) error {
		id, err := c.resolveVM(ref)
		if err != nil {
			return err
		}
		if err := c.Do("DELETE", "/v1/vms/"+id, nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, id)
		return nil
	})
}

// reportBulk prints what a bulk delete destroyed, and fails the command if
// anything was left behind.
func reportBulk(e *env, res types.BulkDeleteResponse) error {
	for _, id := range res.Deleted {
		fmt.Fprintln(e.stdout, id)
	}
	if len(res.Failed) == 0 {
		return nil
	}
	ids := make([]string, 0, len(res.Failed))
	for id := range res.Failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(e.stderr, "mh: %s: %s\n", id, res.Failed[id])
	}
	return exitError{code: 1}
}

func vmStop(e *env, cmd *command, p string, args []string) error {
	return vmAction(e, cmd, p, args, "stop")
}
func vmStart(e *env, cmd *command, p string, args []string) error {
	return vmAction(e, cmd, p, args, "start")
}

func vmAction(e *env, cmd *command, p string, args []string, verb string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one VM")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	return eachArg(e, pos, func(ref string) error {
		id, err := c.resolveVM(ref)
		if err != nil {
			return err
		}
		if err := c.Do("POST", "/v1/vms/"+id+"/"+verb, nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, id)
		return nil
	})
}

func vmExec(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parseLeading(args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(p, "expected a VM and a command")
	}
	vmRef, argv := pos[0], pos[1:]
	// Docker's "mh exec VM -- cmd" spelling: the separator is not part of it.
	if argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return usagef(p, "expected a command after --")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	id, err := c.resolveVM(vmRef)
	if err != nil {
		return err
	}
	var res types.ExecResponse
	if err := c.Do("POST", "/v1/vms/"+id+"/exec", types.ExecRequest{Cmd: shellCommand(argv)}, &res); err != nil {
		return err
	}
	fmt.Fprint(e.stdout, res.Output)
	if res.ExitCode != 0 {
		return exitError{code: res.ExitCode}
	}
	return nil
}

// shellCommand builds the string the guest runs with sh -c. A single argument
// is taken as a shell line, so pipes work: mh exec VM 'ps | grep x'. Several
// are an argv, quoted so each stays one word: mh exec VM touch "a b".
func shellCommand(argv []string) string {
	if len(argv) == 1 {
		return argv[0]
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func vmCopy(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef(p, "expected SOURCE and DESTINATION")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	return copyFiles(e, p, pos[0], pos[1], "VM", func(ref string) (string, error) {
		id, err := c.resolveVM(ref)
		return "/v1/vms/" + id + "/files", err
	})
}

// copyFiles implements "cp" for both VMs and volumes: exactly one side is
// REF:/path, the other a local path or "-" for stdin/stdout. endpoint maps REF
// to its files endpoint.
func copyFiles(e *env, p, src, dst, kind string, endpoint func(ref string) (string, error)) error {
	sRef, sPath, sRemote := splitRemote(src)
	dRef, dPath, dRemote := splitRemote(dst)
	if sRemote == dRemote {
		return usagef(p, "exactly one side must be %s:PATH", kind)
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	if sRemote {
		ep, err := endpoint(sRef)
		if err != nil {
			return err
		}
		return download(e, c, ep, sPath, dst)
	}
	ep, err := endpoint(dRef)
	if err != nil {
		return err
	}
	return upload(e, c, ep, src, dPath)
}

// splitRemote recognises REF:/path. A local path never matches: its part
// before the first colon (if any) contains a slash or is empty.
func splitRemote(s string) (ref, p string, ok bool) {
	ref, p, found := strings.Cut(s, ":")
	if !found || ref == "" || strings.Contains(ref, "/") {
		return "", "", false
	}
	return ref, p, true
}

func download(e *env, c *Client, endpoint, remote, local string) error {
	resp, err := c.Raw(endpoint + "?path=" + url.QueryEscape(remote))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return err
	}
	if local == "-" {
		_, err := io.Copy(e.stdout, resp.Body)
		return err
	}
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		local = filepath.Join(local, path.Base(remote))
	}
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(local)
		return err
	}
	return f.Close()
}

func upload(e *env, c *Client, endpoint, local, remote string) error {
	var body io.Reader = e.stdin
	size := int64(-1)
	if local != "-" {
		f, err := os.Open(local)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory: only single files can be copied", local)
		}
		body, size = f, fi.Size()
		if strings.HasSuffix(remote, "/") {
			remote += filepath.Base(local)
		}
	}
	return c.Stream("PUT", endpoint+"?path="+url.QueryEscape(remote), body, size)
}

func vmLogs(e *env, cmd *command, p string, args []string) error {
	var follow bool
	var tail int64
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&follow, "follow", "f", "keep printing as the log grows (Ctrl-C to stop)")
	fs.int64Var(&tail, "tail", "n", "print only the last `N` lines")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one VM")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	id, err := c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	var vm types.VMResponse
	if err := c.Do("GET", "/v1/vms/"+id, nil, &vm); err != nil {
		return err
	}
	if vm.LogPath == "" {
		return fmt.Errorf("VM %s has no console log", id)
	}
	// The API reports where the log is, not its contents: this reads the file
	// directly, so it only works on the daemon's host.
	f, err := os.Open(vm.LogPath)
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s: permission denied — try: sudo tail -f %s", vm.LogPath, vm.LogPath)
	}
	if err != nil {
		return fmt.Errorf("%w (the log is read from the local disk: mh logs only works on the daemon's host)", err)
	}
	defer f.Close()
	return tailFile(e.stdout, f, tail, follow)
}

// tailFile prints the last n lines of f (all of it when n <= 0) and, with
// follow, keeps polling for appended data.
func tailFile(w io.Writer, f *os.File, n int64, follow bool) error {
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if n > 0 {
		lines := strings.SplitAfter(string(data), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if int64(len(lines)) > n {
			lines = lines[int64(len(lines))-n:]
		}
		data = []byte(strings.Join(lines, ""))
	}
	if _, err := w.Write(data); err != nil || !follow {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		k, err := f.Read(buf)
		if k > 0 {
			if _, werr := w.Write(buf[:k]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func vmSnapshot(e *env, cmd *command, p string, args []string) error {
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

func createSnapshot(e *env, vmRef, name string) error {
	c, err := e.api()
	if err != nil {
		return err
	}
	id, err := c.resolveVM(vmRef)
	if err != nil {
		return err
	}
	var snap types.SnapshotResponse
	if err := c.Do("POST", "/v1/vms/"+id+"/snapshot", types.CreateSnapshotRequest{Name: name}, &snap); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, snap.ID)
	return nil
}

func vmFork(e *env, cmd *command, p string, args []string) error {
	var req types.ForkVMRequest
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&req.Quarantine, "quarantine", "", "attach the clone to no network (vsock only) — needed while the source VM holds its IP")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one VM")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	id, err := c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	var vm types.VMResponse
	if err := c.Do("POST", "/v1/vms/"+id+"/fork", req, &vm); err != nil {
		return err
	}
	announceVM(e, "forked", vm)
	return nil
}

func vmRestore(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef(p, "expected a VM and one of its snapshots (ID or name)")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	id, err := c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	snap, err := c.resolveSnapshot(pos[1], id)
	if err != nil {
		return err
	}
	if err := c.Do("POST", "/v1/vms/"+id+"/restore", types.RestoreVMRequest{Snapshot: snap}, nil); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, id)
	return nil
}

// ---------------------------------------------------------------------------
// Sizes and times.
// ---------------------------------------------------------------------------

// mbValue is a size flag in MiB that also takes M/G suffixes: 512, 512M, 2G.
type mbValue int64

func (m *mbValue) String() string { return strconv.FormatInt(int64(*m), 10) }

func (m *mbValue) Set(s string) error {
	v, err := parseMB(s)
	*m = mbValue(v)
	return err
}

func parseMB(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{{"GIB", 1024}, {"GB", 1024}, {"G", 1024}, {"MIB", 1}, {"MB", 1}, {"M", 1}} {
		if strings.HasSuffix(u, suf.s) {
			u, mult = strings.TrimSuffix(u, suf.s), suf.m
			break
		}
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size: use MiB (512) or a suffix (512M, 2G)", s)
	}
	return n * mult, nil
}

func fmtMB(mb int64) string {
	if mb >= 1024 {
		if mb%1024 == 0 {
			return fmt.Sprintf("%dG", mb/1024)
		}
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

func fmtDuration(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", sec)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

func fmtAgo(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return fmtDuration(int64(time.Since(t).Seconds())) + " ago"
}
