package labels

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for name, ok := range map[string]bool{
		"ot-52":                 true,
		"a":                     true,
		"":                      false,
		"OT":                    false,
		"-ot":                   false,
		"ot_52":                 false,
		"ot/52":                 false, // would not survive a URL path
		strings.Repeat("a", 64): false,
	} {
		if err := ValidateName("network name", name); (err == nil) != ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", name, err, ok)
		} else if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		labels map[string]string
		ok     bool
	}{
		{nil, true},
		{map[string]string{"sensor": "ts-01"}, true},
		{map[string]string{"ot.plant/managed-by": "ot-orch"}, true},
		{map[string]string{"role": ""}, true}, // present, empty value
		{map[string]string{"Sensor": "x"}, false},
		{map[string]string{"sensor": "has space"}, false},
		{map[string]string{"sensor": "-x"}, false},
		{map[string]string{"": "x"}, false},
	} {
		if err := Validate(tc.labels); (err == nil) != tc.ok {
			t.Errorf("Validate(%v) = %v, want ok=%v", tc.labels, err, tc.ok)
		}
	}
	many := make(map[string]string)
	for i := 0; i <= MaxLabels; i++ {
		many[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	if err := Validate(many); err == nil {
		t.Errorf("%d labels accepted, the cap is %d", len(many), MaxLabels)
	}
}

func TestPatch(t *testing.T) {
	s := func(v string) *string { return &v }
	prev := map[string]string{"sensor": "ts-01", "role": "parser"}
	next, err := Patch(prev, map[string]*string{"role": nil, "owner": s("ot")})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"sensor": "ts-01", "owner": "ot"}; !reflect.DeepEqual(next, want) {
		t.Errorf("Patch = %v, want %v", next, want)
	}
	if prev["role"] != "parser" || len(prev) != 2 {
		t.Errorf("Patch wrote into its input: %v", prev)
	}
	if next, err := Patch(prev, map[string]*string{"sensor": nil, "role": nil}); err != nil || next != nil {
		t.Errorf("removing every label = %v, %v; want nil, nil", next, err)
	}
	for _, bad := range []map[string]*string{{"Bad": s("x")}, {"k": s("bad value")}, {"": nil}} {
		if _, err := Patch(prev, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Patch(%v) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestSelector(t *testing.T) {
	sel, err := ParseSelector([]string{"sensor=ts-01", "role=parser,owner=ot"})
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(map[string]string{"sensor": "ts-01", "role": "parser", "owner": "ot", "extra": "x"}) {
		t.Error("a VM with every label (and more) must match")
	}
	if sel.Matches(map[string]string{"sensor": "ts-01", "role": "parser"}) {
		t.Error("a VM missing one label must not match")
	}
	if empty, _ := ParseSelector(nil); !empty.Matches(nil) {
		t.Error("an empty selector selects everything")
	}
	for _, bad := range []string{"sensor", "Sensor=x", "sensor=a b"} {
		if _, err := ParseSelector([]string{bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("selector %q = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := ParseSelector([]string{"sensor=a", "sensor=b"}); err == nil {
		t.Error("a selector asking one key for two values can match nothing; refuse it")
	}
}

// managed-by decides ownership and quota: a patch may repeat it, never add,
// change or remove it.
func TestPatchManagedByFixed(t *testing.T) {
	s := func(v string) *string { return &v }
	owned := map[string]string{ManagedBy: "ot", "role": "a"}
	for name, c := range map[string]struct {
		prev  map[string]string
		patch map[string]*string
		ok    bool
	}{
		"same value":   {owned, map[string]*string{ManagedBy: s("ot"), "role": s("b")}, true},
		"other keys":   {owned, map[string]*string{"role": nil}, true},
		"change":       {owned, map[string]*string{ManagedBy: s("x")}, false},
		"remove":       {owned, map[string]*string{ManagedBy: nil}, false},
		"add":          {map[string]string{"role": "a"}, map[string]*string{ManagedBy: s("ot")}, false},
		"remove unset": {map[string]string{"role": "a"}, map[string]*string{ManagedBy: nil}, true},
	} {
		_, err := Patch(c.prev, c.patch)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok=%v", name, err, c.ok)
		}
	}
}
