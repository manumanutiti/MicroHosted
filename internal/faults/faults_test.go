package faults

import "testing"

func TestParse(t *testing.T) {
	got, err := Parse("vm.create.after-clone:error, vm.create.after-boot:crash")
	if err != nil {
		t.Fatal(err)
	}
	if got["vm.create.after-clone"] != modeError || got["vm.create.after-boot"] != modeCrash {
		t.Errorf("Parse = %v", got)
	}
	for _, bad := range []string{"vm.create.after-clone", "vm.create.after-clon:error", "vm.create.after-clone:panic"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if got, err := Parse(""); err != nil || len(got) != 0 {
		t.Errorf("empty spec = %v, %v", got, err)
	}
}

func TestCheckInactiveIsNil(t *testing.T) {
	for _, p := range Points {
		if err := Check(p); err != nil {
			t.Errorf("%s fired with no spec: %v", p, err)
		}
	}
}

func TestCheckError(t *testing.T) {
	prev := active
	t.Cleanup(func() { active = prev })
	active = map[string]mode{"vm.create.after-clone": modeError}
	if err := Check("vm.create.after-clone"); err == nil {
		t.Error("active error point returned nil")
	}
	if err := Check("vm.create.after-boot"); err != nil {
		t.Errorf("inactive point fired: %v", err)
	}
}
