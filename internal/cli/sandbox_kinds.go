package cli

// The kinds of alert the image's tools report (mh-sandbox-report --tsv,
// mh-sandbox-watch: "alert KIND COUNT WHAT BY"). The guest names the kind;
// this table is what the CLI knows about it: how serious, under which
// heading, in one line for a person. A kind not in it is still shown, under
// its own name: a newer image may know more than this CLI.

type severity int

const (
	sevInfo severity = iota // worth knowing: ordinary code does it too
	sevWarn                 // unusual for ordinary code
	sevHigh                 // what malicious code does
)

func (s severity) String() string {
	switch s {
	case sevHigh:
		return "high"
	case sevWarn:
		return "warn"
	}
	return "info"
}

type alertKind struct {
	Severity severity
	Title    string // the heading it is shown under
	Means    string // one line: why it matters
}

var alertKinds = map[string]alertKind{}

// kindOf is what the CLI knows of kind, or a generic entry for one it does
// not: shown, never dropped.
func kindOf(kind string) alertKind {
	if k, ok := alertKinds[kind]; ok {
		return k
	}
	return alertKind{Severity: sevWarn, Title: kind, Means: "reported by the image (unknown to this mh: update it)"}
}
