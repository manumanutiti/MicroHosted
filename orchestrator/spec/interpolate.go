package spec

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvFile is the file next to a plant spec whose variables it may use, as
// docker compose reads .env: the process environment wins over it.
const EnvFile = ".env"

// Var sources: where an interpolated variable's value came from.
const (
	FromEnvironment = "environment"
	FromEnvFile     = EnvFile
	FromDefault     = "default"
)

// Lookup finds a variable's value and where it came from.
type Lookup func(name string) (value, source string, ok bool)

// EnvLookup looks name up in the process environment, then in the
// variables read from an .env file.
func EnvLookup(file map[string]string) Lookup {
	return func(name string) (string, string, bool) {
		if v, ok := os.LookupEnv(name); ok {
			return v, FromEnvironment, true
		}
		if v, ok := file[name]; ok {
			return v, FromEnvFile, true
		}
		return "", "", false
	}
}

// ReadEnvFile reads KEY=VALUE lines from dir/.env: blank lines and #
// comments skipped, an optional "export " prefix, a value optionally in
// matching quotes. A missing file is no variables.
func ReadEnvFile(dir string) (map[string]string, error) {
	p := filepath.Join(dir, EnvFile)
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !validVarName(k) {
			return nil, fmt.Errorf("%s:%d: want NAME=VALUE", p, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vars[k] = v
	}
	return vars, sc.Err()
}

func validVarName(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for _, c := range s {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// interpolate replaces ${NAME} in every value of the YAML document (never in
// keys or comments), as docker compose does:
//
//	${NAME}          NAME's value; an error if it is unset
//	${NAME:-word}    word if NAME is unset or empty (${NAME-word}: only unset)
//	${NAME:?message} an error with message if NAME is unset or empty
//	$${              a literal ${
//
// Only the braced form is recognised: a bare $NAME is left alone, so shell
// commands ($(hostname), $ip) need no escaping. It returns the document to
// parse — data itself when nothing changed, so error lines stay the file's —
// and the variables used, by source.
func interpolate(data []byte, lookup Lookup) ([]byte, map[string]string, error) {
	if !bytes.Contains(data, []byte("${")) {
		return data, nil, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, err
	}
	used := map[string]string{}
	var errs []error
	changed := false
	reported := map[string]bool{}
	var walk func(n *yaml.Node, isKey bool)
	walk = func(n *yaml.Node, isKey bool) {
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, false)
			}
		case yaml.MappingNode:
			for i, c := range n.Content {
				walk(c, i%2 == 0)
			}
		case yaml.ScalarNode:
			if isKey || !strings.Contains(n.Value, "${") {
				return
			}
			v, err := expand(n.Value, lookup, used)
			if err != nil {
				// One report per problem: a variable used ten times and
				// unset is one thing to fix.
				if !reported[err.Error()] {
					reported[err.Error()] = true
					errs = append(errs, fmt.Errorf("line %d: %w", n.Line, err))
				}
				return
			}
			if v != n.Value {
				n.Value, changed = v, true
				// A value that became a number or a bool is still the
				// string the variable held.
				if n.Tag != "!!str" && n.Style == 0 {
					n.Style = yaml.DoubleQuotedStyle
				}
			}
		}
	}
	walk(&root, false)
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	if !changed {
		return data, used, nil
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, nil, err
	}
	return out, used, nil
}

func expand(s string, lookup Lookup, used map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		if i > 0 && s[i-1] == '$' { // $${ → ${
			b.WriteString(s[:i])
			b.WriteString("{")
			s = s[i+2:]
			continue
		}
		b.WriteString(s[:i])
		end := strings.Index(s[i:], "}")
		if end < 0 {
			return "", fmt.Errorf("%q: unclosed ${", s[i:])
		}
		expr := s[i+2 : i+end]
		s = s[i+end+1:]
		name, op, arg := expr, "", ""
		if k := strings.IndexAny(expr, ":-?"); k > 0 {
			name, op = expr[:k], expr[k:k+1]
			if op == ":" && k+1 < len(expr) && (expr[k+1] == '-' || expr[k+1] == '?') {
				op = expr[k : k+2]
			}
			arg = expr[k+len(op):]
		}
		if !validVarName(name) {
			return "", fmt.Errorf("${%s}: not a variable name", expr)
		}
		v, src, ok := lookup(name)
		empty := !ok || (strings.HasPrefix(op, ":") && v == "")
		switch {
		case !empty:
			used[name] = src
		case op == ":-" || op == "-":
			v = arg
			used[name] = FromDefault
		case op == ":?" || op == "?":
			return "", fmt.Errorf("${%s}: %s", name, orText(arg, "required"))
		default:
			return "", fmt.Errorf("${%s} is not set: export it, put %s=… in %s next to the spec, or give a default: ${%s:-…}", name, name, EnvFile, name)
		}
		b.WriteString(v)
	}
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
