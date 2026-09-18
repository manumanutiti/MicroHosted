package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"microhosted/pkg/types"
)

var networkGroup = &group{
	name:    "network",
	aliases: []string{"net", "networks"},
	summary: "Manage networks (bridge + subnet + egress policy)",
	cmds: []*command{
		{name: "create", aliases: []string{"new"}, args: "NAME", summary: "Create a network", run: netCreate},
		{name: "ls", aliases: []string{"list"}, summary: "List networks", run: netList},
		{name: "inspect", aliases: []string{"show"}, args: "NAME...", summary: "Show a network's full detail as JSON", run: netInspect},
		{name: "update", aliases: []string{"change", "set"}, args: "NAME", summary: "Change a live network's egress policy and/or VM↔VM reachability", run: netUpdate},
		{name: "rm", aliases: []string{"remove", "delete"}, args: "NAME...", summary: "Delete networks (-f destroys their VMs first)", run: netRemove},
	},
}

// Egress rules on the command line: PROTO:IP[:PORT][@IFACE]
//
//	tcp:203.0.113.7:8883      icmp:203.0.113.7
//	udp:10.0.0.0/24:53        tcp:192.168.50.52:502@wlan0
//
// Only the shape is checked here; what makes a rule valid (canonical CIDR,
// port range, a managed interface) is the daemon's call, and its 400 message
// comes back verbatim.
const ruleSyntax = "PROTO:IP[:PORT][@IFACE], e.g. tcp:203.0.113.7:8883, icmp:10.0.0.1, tcp:192.168.50.52:502@wlan0"

func parseRule(s string) (types.EgressRule, error) {
	var r types.EgressRule
	spec, iface, hasIface := strings.Cut(s, "@")
	if hasIface && iface == "" {
		return r, fmt.Errorf("rule %q: empty interface after @ (%s)", s, ruleSyntax)
	}
	r.Iface = iface
	proto, rest, ok := strings.Cut(spec, ":")
	if !ok || rest == "" {
		return r, fmt.Errorf("rule %q: expected %s", s, ruleSyntax)
	}
	r.Protocol = strings.ToLower(proto)
	switch r.Protocol {
	case "tcp", "udp":
		i := strings.LastIndex(rest, ":")
		if i < 0 {
			return r, fmt.Errorf("rule %q: %s needs a port (%s)", s, r.Protocol, ruleSyntax)
		}
		port, err := strconv.Atoi(rest[i+1:])
		if err != nil {
			return r, fmt.Errorf("rule %q: port %q is not a number", s, rest[i+1:])
		}
		r.IP, r.Port = rest[:i], port
	case "icmp":
		if strings.Contains(rest, ":") {
			return r, fmt.Errorf("rule %q: icmp takes no port", s)
		}
		r.IP = rest
	default:
		return r, fmt.Errorf("rule %q: protocol must be tcp, udp or icmp", s)
	}
	return r, nil
}

func formatRule(r types.EgressRule) string {
	s := r.Protocol + ":" + r.IP
	if r.Port != 0 {
		s += ":" + strconv.Itoa(r.Port)
	}
	if r.Iface != "" {
		s += "@" + r.Iface
	}
	return s
}

func parseRules(specs []string) ([]types.EgressRule, error) {
	var out []types.EgressRule
	for _, s := range specs {
		r, err := parseRule(s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func describeEgress(n types.NetworkResponse) string {
	if n.Egress {
		return "all"
	}
	if len(n.AllowedEgress) == 0 {
		return "none"
	}
	parts := make([]string, len(n.AllowedEgress))
	for i, r := range n.AllowedEgress {
		parts[i] = formatRule(r)
	}
	return strings.Join(parts, " ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func netCreate(e *env, cmd *command, p string, args []string) error {
	var req types.CreateNetworkRequest
	var allow []string
	fs := newCmdFlags(e, p, cmd)
	fs.stringVar(&req.Subnet, "subnet", "", "", "`CIDR`, e.g. 10.10.0.0/24 (default: a free /24)")
	fs.boolVar(&req.Egress, "egress", "", "full internet egress (NAT); off by default")
	fs.listVar(&allow, "allow", "", "only allow this outbound flow: `RULE` = "+ruleSyntax+" (repeatable)")
	fs.boolVar(&req.Intra, "intra", "", "let the network's VMs reach each other; off by default")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one NAME")
	}
	req.Name = pos[0]
	if req.AllowedEgress, err = parseRules(allow); err != nil {
		return usagef(p, "%v", err)
	}
	if req.Egress && len(req.AllowedEgress) > 0 {
		return usagef(p, "--egress already allows everything; use either it or --allow")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var n types.NetworkResponse
	if err := c.Do("POST", "/v1/networks", req, &n); err != nil {
		return err
	}
	printNetworks(e, []types.NetworkResponse{n})
	return nil
}

func printNetworks(e *env, nets []types.NetworkResponse) {
	rows := make([][]string, 0, len(nets))
	for _, n := range nets {
		rows = append(rows, []string{n.Name, n.Subnet, n.Gateway, n.Bridge, onOff(n.Intra), describeEgress(n)})
	}
	table(e.stdout, []string{"NAME", "SUBNET", "GATEWAY", "BRIDGE", "INTRA", "EGRESS"}, rows)
}

func netList(e *env, cmd *command, p string, args []string) error {
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
	var nets []types.NetworkResponse
	if err := c.Do("GET", "/v1/networks", nil, &nets); err != nil {
		return err
	}
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
	switch {
	case asJSON:
		return printJSON(e.stdout, nets)
	case quiet:
		for _, n := range nets {
			fmt.Fprintln(e.stdout, n.Name)
		}
		return nil
	}
	printNetworks(e, nets)
	return nil
}

func getNetwork(c *Client, name string) (types.NetworkResponse, error) {
	var n types.NetworkResponse
	return n, c.Do("GET", "/v1/networks/"+url.PathEscape(name), nil, &n)
}

func netInspect(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one NAME")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var out []types.NetworkResponse
	for _, name := range pos {
		n, err := getNetwork(c, name)
		if err != nil {
			return err
		}
		out = append(out, n)
	}
	return printInspect(e.stdout, out)
}

func netUpdate(e *env, cmd *command, p string, args []string) error {
	var egress, noEgress, intra, noIntra bool
	var allow, addAllow, rmAllow []string
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&egress, "egress", "", "full internet egress (replaces any --allow rules)")
	fs.boolVar(&noEgress, "no-egress", "", "cut all egress")
	fs.listVar(&allow, "allow", "", "REPLACE the rules with these: `RULE` = "+ruleSyntax+" (repeatable)")
	fs.listVar(&addAllow, "add-allow", "", "add a `RULE` to the current ones (repeatable)")
	fs.listVar(&rmAllow, "rm-allow", "", "remove a `RULE` from the current ones (repeatable)")
	fs.boolVar(&intra, "intra", "", "let the network's VMs reach each other")
	fs.boolVar(&noIntra, "no-intra", "", "isolate the network's VMs from each other")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one NAME")
	}
	name := pos[0]

	modes := 0
	for _, set := range []bool{egress, noEgress, len(allow) > 0, len(addAllow)+len(rmAllow) > 0} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return usagef(p, "choose one of --egress, --no-egress, --allow, or --add-allow/--rm-allow")
	}
	if intra && noIntra {
		return usagef(p, "--intra and --no-intra are mutually exclusive")
	}
	if modes == 0 && !intra && !noIntra {
		return usagef(p, "nothing to change: give an egress flag and/or --intra/--no-intra")
	}
	allowRules, err := parseRules(allow)
	if err != nil {
		return usagef(p, "%v", err)
	}
	addRules, err := parseRules(addAllow)
	if err != nil {
		return usagef(p, "%v", err)
	}
	rmRules, err := parseRules(rmAllow)
	if err != nil {
		return usagef(p, "%v", err)
	}

	c, err := e.api()
	if err != nil {
		return err
	}
	path := "/v1/networks/" + url.PathEscape(name)
	var n types.NetworkResponse
	if modes == 1 {
		// The API replaces the whole policy; --add-allow/--rm-allow are the
		// CLI merging onto what is there now.
		req := types.UpdateNetworkEgressRequest{Egress: egress, AllowedEgress: allowRules}
		if len(addRules)+len(rmRules) > 0 {
			cur, err := getNetwork(c, name)
			if err != nil {
				return err
			}
			if cur.Egress {
				return fmt.Errorf("network %s has full egress: set the exact rules with --allow instead", name)
			}
			if req.AllowedEgress, err = mergeRules(cur.AllowedEgress, addRules, rmRules); err != nil {
				return err
			}
		}
		if err := c.Do("PUT", path+"/egress", req, &n); err != nil {
			return err
		}
	}
	if intra || noIntra {
		if err := c.Do("PUT", path+"/intra", types.UpdateNetworkIntraRequest{Intra: intra}, &n); err != nil {
			return err
		}
	}
	printNetworks(e, []types.NetworkResponse{n})
	return nil
}

// mergeRules returns cur minus rm plus add (skipping duplicates). Removing a
// rule that is not there is an error: a typo in --rm-allow must not look like
// a closed hole.
func mergeRules(cur, add, rm []types.EgressRule) ([]types.EgressRule, error) {
	out := make([]types.EgressRule, 0, len(cur)+len(add))
	for _, r := range rm {
		found := false
		for _, c := range cur {
			found = found || c == r
		}
		if !found {
			return nil, fmt.Errorf("rule %s is not in the network's policy", formatRule(r))
		}
	}
next:
	for _, c := range cur {
		for _, r := range rm {
			if c == r {
				continue next
			}
		}
		out = append(out, c)
	}
	for _, a := range add {
		dup := false
		for _, o := range out {
			dup = dup || o == a
		}
		if !dup {
			out = append(out, a)
		}
	}
	return out, nil
}

func netRemove(e *env, cmd *command, p string, args []string) error {
	var force bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&force, "force", "f", "destroy the network's VMs first")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one NAME")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	return eachArg(e, pos, func(name string) error {
		path := "/v1/networks/" + url.PathEscape(name)
		if force {
			var res types.BulkDeleteResponse
			if err := c.Do("DELETE", path+"/vms", nil, &res); err != nil {
				return err
			}
			if len(res.Failed) > 0 {
				return fmt.Errorf("%d VM(s) could not be destroyed, network kept: %v", len(res.Failed), res.Failed)
			}
		}
		if err := c.Do("DELETE", path, nil, nil); err != nil {
			var ae *APIError
			if !force && errors.As(err, &ae) && ae.Status == http.StatusConflict {
				return fmt.Errorf("%w (-f destroys its VMs first)", err)
			}
			return err
		}
		fmt.Fprintln(e.stdout, name)
		return nil
	})
}
