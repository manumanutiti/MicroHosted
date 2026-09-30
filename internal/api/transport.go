package api

import (
	"context"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
)

// GuardTCP puts in front of h what a TCP listener needs and the Unix socket
// does not: a browser can reach a port — any page it has open can send
// requests to 127.0.0.1 — but it cannot open a socket file. Two checks:
//
//   - Host must be an IP literal, "localhost", or one of extraHosts. A DNS
//     rebinding page talks to the port under its own name (Host:
//     rebind.attacker.example) and would otherwise read every GET — the VM
//     list, /v1/system, a file off a guest. An IP literal cannot be rebound:
//     a page with that origin is already served by this host.
//   - a JSON body must be sent as application/json (see decodeJSON). A
//     browser sends text/plain or a form cross-origin without a preflight;
//     only a script allowed by CORS — which this API never grants — can send
//     application/json.
//
// Cross-origin writes are refused for every listener by NewServer's
// http.CrossOriginProtection; these two close what that one leaves to a
// browser lacking Sec-Fetch-Site and Origin, and the reads it never covers.
func GuardTCP(h http.Handler, extraHosts []string) http.Handler {
	allowed := make(map[string]bool, len(extraHosts)+1)
	allowed["localhost"] = true
	for _, host := range extraHosts {
		if host = normalizeHost(host); host != "" {
			allowed[host] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOnly(r.Host)
		if net.ParseIP(host) == nil && !allowed[host] {
			log.Printf("refused %s %s: Host %q is not this daemon's (--addr-hosts)", r.Method, r.URL.Path, r.Host)
			writeError(w, http.StatusForbidden, fmt.Errorf("host %q is not allowed: use an IP address or localhost, or start the daemon with --addr-hosts", r.Host))
			return
		}
		next := r.WithContext(context.WithValue(r.Context(), jsonTypeRequired{}, true))
		h.ServeHTTP(w, next)
	})
}

// jsonTypeRequired marks a request whose JSON body must be labelled
// application/json (see GuardTCP and decodeJSON).
type jsonTypeRequired struct{}

// checkJSONType enforces the Content-Type of a JSON body on the requests
// GuardTCP marked. On the Unix socket anything goes, so `curl -d` works as
// the docs show it.
func checkJSONType(r *http.Request) error {
	if required, _ := r.Context().Value(jsonTypeRequired{}).(bool); !required {
		return nil
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return fmt.Errorf("a JSON body must be sent with Content-Type: application/json (got %q)", r.Header.Get("Content-Type"))
	}
	return nil
}

// hostOnly is the name part of a Host header: no port, no IPv6 brackets,
// lower case, no trailing dot.
func hostOnly(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return normalizeHost(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
