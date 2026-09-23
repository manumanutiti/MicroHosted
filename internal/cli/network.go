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
	summary: "Manage networks (bridge + subnet + in/out policy)",
	cmds: []*command{
		{name: "create", aliases: []string{"new"}, args: "NAME", summary: "Create a network", help: netDirections, examples: netCreateExamples, run: netCreate},
		{name: "ls", aliases: []string{"list"}, summary: "List networks", run: netList},
		{name: "inspect", aliases: []string{"show"}, args: "NAME...", summary: "Show a network's full detail as JSON", run: netInspect},
		{name: "update", aliases: []string{"change", "set"}, args: "NAME", summary: "Change a live network's in/out policy and/or VM↔VM reachability (VMs stay up)", help: netDirections, examples: netUpdateExamples, run: netUpdate},
		{name: "rm", aliases: []string{"remove", "delete"}, args: "NAME...", summary: "Delete networks (-f destroys their VMs first)", run: netRemove},
	},
}

// A network's policy is named by WHO OPENS THE CONNECTION, not by the API's
// egress/ingress: that is the question an operator actually has in mind ("may
// the VM poll the sensor?" vs "may the sensor push to the VM?"). The API keeps
// its names; the old flag spellings keep working, hidden.
const netDirections = `OUT = the VM opens the connection        VM → destination (internet, or a device @IFACE)
      --internet IFACE opens ALL outbound, through that host interface only
IN  = a device opens it towards a VM      device on a managed IFACE → VM
Everything not listed is dropped, both ways.`

const netCreateExamples = `  # isolated: nothing in or out
  mh network create lab
  # the VM may poll the Modbus sensor .52 behind wlan0
  mh network create ot --out tcp:192.168.50.52:502@wlan0
  # the sensor .60 behind wlan0 may publish to the VM 172.16.9.2 (IN needs --subnet)
  mh network create mqtt --subnet 172.16.9.0/24 --in tcp:192.168.50.60:1883@wlan0=172.16.9.2`

const netUpdateExamples = `  # the VM may poll the Modbus sensor .52 behind wlan0
  mh network update ot --out tcp:192.168.50.52:502@wlan0
  # the VM may reach an MQTT broker on the internet (no @IFACE = internet)
  mh network update iot --out tcp:203.0.113.7:8883
  # the sensor .60 behind wlan0 may publish to the VM 172.16.9.2
  mh network update mqtt --in tcp:192.168.50.60:1883@wlan0=172.16.9.2
  # take a rule back
  mh network update ot --rm-out tcp:192.168.50.52:502@wlan0`

// OUT rules on the command line: PROTO:IP[:PORT][@IFACE]
//
//	tcp:203.0.113.7:8883      icmp:203.0.113.7
//	udp:10.0.0.0/24:53        tcp:192.168.50.52:502@wlan0
//
// Only the shape is checked here; what makes a rule valid (canonical CIDR,
// port range, a managed interface) is the daemon's call, and its 400 message
// comes back verbatim.
const ruleSyntax = "PROTO:DEST[:PORT][@IFACE]"

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

// IN rules on the command line: PROTO:SRC:PORT@IFACE=VM_IP
//
//	tcp:192.168.50.60:1883@wlan0=172.16.9.2
//
// read as "tcp from 192.168.50.60 to port 1883 of this host on wlan0 goes to
// the guest 172.16.9.2". Same split as egress: the shape here, validity at the
// daemon.
const ingressSyntax = "PROTO:SRC:PORT@IFACE=VM_IP"

func parseIngressRule(s string) (types.IngressRule, error) {
	var r types.IngressRule
	bad := func(why string) (types.IngressRule, error) {
		return r, fmt.Errorf("IN rule %q: %s (%s)", s, why, ingressSyntax)
	}
	spec, target, ok := strings.Cut(s, "=")
	if !ok || target == "" {
		return bad("missing =VM_IP")
	}
	spec, iface, ok := strings.Cut(spec, "@")
	if !ok || iface == "" {
		return bad("missing @IFACE")
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 3 || parts[1] == "" {
		return bad("expected PROTO:SRC:PORT before @")
	}
	port, err := strconv.Atoi(parts[2])
	if err != nil {
		return bad(fmt.Sprintf("port %q is not a number", parts[2]))
	}
	r = types.IngressRule{Iface: iface, SrcIP: parts[1], Protocol: strings.ToLower(parts[0]), Port: port, ToIP: target}
	if r.Protocol != "tcp" && r.Protocol != "udp" {
		return bad("protocol must be tcp or udp")
	}
	return r, nil
}

func formatIngressRule(r types.IngressRule) string {
	return fmt.Sprintf("%s:%s:%d@%s=%s", r.Protocol, r.SrcIP, r.Port, r.Iface, r.ToIP)
}

func parseIngressRules(specs []string) ([]types.IngressRule, error) {
	var out []types.IngressRule
	for _, s := range specs {
		r, err := parseIngressRule(s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func describeIngress(n types.NetworkResponse) string {
	if len(n.AllowedIngress) == 0 {
		return "none"
	}
	parts := make([]string, len(n.AllowedIngress))
	for i, r := range n.AllowedIngress {
		parts[i] = formatIngressRule(r)
	}
	return strings.Join(parts, " ")
}

func describeEgress(n types.NetworkResponse) string {
	if n.Egress {
		if n.EgressIface == "" {
			return "CLOSED (internet without an interface: set one with --internet IFACE)"
		}
		return "internet@" + n.EgressIface
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
	var allow, ingress []string
	fs := newCmdFlags(e, p, cmd)
	fs.listVar(&allow, "out", "", "allow an OUT flow: `RULE` = "+ruleSyntax+" (repeatable)")
	fs.stringVar(&req.EgressIface, "internet", "", "", "allow ALL outbound through host interface `IFACE` (NAT), e.g. eth0, instead of --out rules")
	fs.listVar(&ingress, "in", "", "allow an IN flow: `RULE` = "+ingressSyntax+" (repeatable; needs --subnet)")
	fs.stringVar(&req.Subnet, "subnet", "", "", "`CIDR`, e.g. 10.10.0.0/24 (default: a free /24)")
	fs.boolVar(&req.Intra, "intra", "", "let the network's VMs reach each other; off by default")
	fs.hidden("out", "allow")
	fs.hidden("internet", "egress")
	fs.hidden("in", "ingress")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one NAME")
	}
	req.Name = pos[0]
	req.Egress = req.EgressIface != ""
	if req.AllowedEgress, err = parseRules(allow); err != nil {
		return usagef(p, "%v", err)
	}
	if req.Egress && len(req.AllowedEgress) > 0 {
		return usagef(p, "--internet already allows every outbound flow; use either it or --out")
	}
	if req.AllowedIngress, err = parseIngressRules(ingress); err != nil {
		return usagef(p, "%v", err)
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
		rows = append(rows, []string{n.Name, n.Subnet, n.Gateway, n.Bridge, onOff(n.Intra), describeEgress(n), describeIngress(n)})
	}
	table(e.stdout, []string{"NAME", "SUBNET", "GATEWAY", "BRIDGE", "INTRA", "OUT", "IN"}, rows)
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
	var noEgress, intra, noIntra, noIngress bool
	var egressIface string
	var allow, addAllow, rmAllow, ingress, addIngress, rmIngress []string
	fs := newCmdFlags(e, p, cmd)
	fs.listVar(&addAllow, "out", "", "add an OUT `RULE` = "+ruleSyntax+" (repeatable)")
	fs.listVar(&rmAllow, "rm-out", "", "remove an OUT `RULE` (repeatable)")
	fs.listVar(&allow, "set-out", "", "REPLACE all OUT rules with these `RULE`s (repeatable)")
	fs.boolVar(&noEgress, "no-out", "", "close all outbound")
	fs.stringVar(&egressIface, "internet", "", "", "allow ALL outbound through host interface `IFACE`, e.g. eth0 (replaces the OUT rules)")
	fs.listVar(&addIngress, "in", "", "add an IN `RULE` = "+ingressSyntax+" (repeatable)")
	fs.listVar(&rmIngress, "rm-in", "", "remove an IN `RULE` (repeatable)")
	fs.listVar(&ingress, "set-in", "", "REPLACE all IN rules with these `RULE`s (repeatable)")
	fs.boolVar(&noIngress, "no-in", "", "close all inbound")
	fs.boolVar(&intra, "intra", "", "let the network's VMs reach each other")
	fs.boolVar(&noIntra, "no-intra", "", "isolate the network's VMs from each other")
	fs.hidden("out", "add-allow")
	fs.hidden("rm-out", "rm-allow")
	fs.hidden("set-out", "allow")
	fs.hidden("no-out", "no-egress")
	fs.hidden("internet", "egress")
	fs.hidden("in", "add-ingress")
	fs.hidden("rm-in", "rm-ingress")
	fs.hidden("set-in", "ingress")
	fs.hidden("no-in", "no-ingress")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one NAME")
	}
	name := pos[0]

	egress := egressIface != ""
	modes := 0
	for _, set := range []bool{egress, noEgress, len(allow) > 0, len(addAllow)+len(rmAllow) > 0} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return usagef(p, "choose one of --out/--rm-out, --set-out, --no-out or --internet")
	}
	inModes := 0
	for _, set := range []bool{noIngress, len(ingress) > 0, len(addIngress)+len(rmIngress) > 0} {
		if set {
			inModes++
		}
	}
	if inModes > 1 {
		return usagef(p, "choose one of --in/--rm-in, --set-in or --no-in")
	}
	if intra && noIntra {
		return usagef(p, "--intra and --no-intra are mutually exclusive")
	}
	if modes == 0 && inModes == 0 && !intra && !noIntra {
		return usagef(p, "nothing to change: give an OUT flag, an IN flag and/or --intra/--no-intra")
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
	inRules, err := parseIngressRules(ingress)
	if err != nil {
		return usagef(p, "%v", err)
	}
	addInRules, err := parseIngressRules(addIngress)
	if err != nil {
		return usagef(p, "%v", err)
	}
	rmInRules, err := parseIngressRules(rmIngress)
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
		// The API replaces the whole policy; --out/--rm-out are the CLI
		// merging onto what is there now.
		req := types.UpdateNetworkEgressRequest{Egress: egress, EgressIface: egressIface, AllowedEgress: allowRules}
		if len(addRules)+len(rmRules) > 0 {
			cur, err := getNetwork(c, name)
			if err != nil {
				return err
			}
			if cur.Egress {
				return fmt.Errorf("network %s allows all outbound (--internet): set the exact rules with --set-out instead", name)
			}
			if req.AllowedEgress, err = mergeRules(cur.AllowedEgress, addRules, rmRules, formatRule); err != nil {
				return err
			}
		}
		if err := c.Do("PUT", path+"/egress", req, &n); err != nil {
			return err
		}
	}
	if inModes == 1 {
		req := types.UpdateNetworkIngressRequest{AllowedIngress: inRules}
		if len(addInRules)+len(rmInRules) > 0 {
			cur, err := getNetwork(c, name)
			if err != nil {
				return err
			}
			if req.AllowedIngress, err = mergeRules(cur.AllowedIngress, addInRules, rmInRules, formatIngressRule); err != nil {
				return err
			}
		}
		if err := c.Do("PUT", path+"/ingress", req, &n); err != nil {
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
// rule that is not there is an error: a typo in --rm-out must not look like
// a closed hole.
func mergeRules[R comparable](cur, add, rm []R, format func(R) string) ([]R, error) {
	out := make([]R, 0, len(cur)+len(add))
	for _, r := range rm {
		found := false
		for _, c := range cur {
			found = found || c == r
		}
		if !found {
			return nil, fmt.Errorf("rule %s is not in the network's policy", format(r))
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
