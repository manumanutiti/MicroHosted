package vm

import (
	"errors"
	"testing"

	"microhosted/pkg/types"
)

var testCeil = types.IOLimits{DiskMiBs: 100, DiskIOPS: 4000, NetMbit: 100}

func TestEffectiveIO(t *testing.T) {
	cases := []struct {
		name string
		req  *types.IOLimits
		ceil types.IOLimits
		want types.IOLimits
	}{
		{"no request takes the ceiling", nil, testCeil, testCeil},
		{"zero fields take the ceiling", &types.IOLimits{NetMbit: 10}, testCeil,
			types.IOLimits{DiskMiBs: 100, DiskIOPS: 4000, NetMbit: 10}},
		// A record asking more than a ceiling lowered since it was created.
		{"never above a lowered ceiling", &types.IOLimits{DiskMiBs: 80, DiskIOPS: 3000, NetMbit: 90},
			types.IOLimits{DiskMiBs: 50, DiskIOPS: 1000, NetMbit: 20},
			types.IOLimits{DiskMiBs: 50, DiskIOPS: 1000, NetMbit: 20}},
		{"lifted ceiling keeps the request", &types.IOLimits{DiskMiBs: 30}, types.IOLimits{},
			types.IOLimits{DiskMiBs: 30}},
		{"lifted ceiling, no request: unlimited", nil, types.IOLimits{}, types.IOLimits{}},
	}
	for _, c := range cases {
		if got := EffectiveIO(c.req, c.ceil); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestValidateIOLimits(t *testing.T) {
	ok := []*types.IOLimits{nil, {}, {DiskMiBs: 100}, {DiskIOPS: 1, NetMbit: 100}}
	for _, r := range ok {
		if err := ValidateIOLimits(r, testCeil); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := []*types.IOLimits{{DiskMiBs: 101}, {DiskIOPS: 4001}, {NetMbit: 1000}, {DiskMiBs: -1}}
	for _, r := range bad {
		if err := ValidateIOLimits(r, testCeil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", r, err)
		}
	}
	// With the ceiling lifted anything non-negative is a valid limit.
	if err := ValidateIOLimits(&types.IOLimits{NetMbit: 10000}, types.IOLimits{}); err != nil {
		t.Errorf("lifted ceiling: %v", err)
	}
}

func TestLowerIOLimits(t *testing.T) {
	snap := &types.IOLimits{DiskMiBs: 50, NetMbit: 20}
	got := lowerIOLimits(snap, &types.IOLimits{DiskMiBs: 80, DiskIOPS: 500, NetMbit: 10})
	want := types.IOLimits{DiskMiBs: 50, DiskIOPS: 500, NetMbit: 10}
	if got == nil || *got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if lowerIOLimits(nil, nil) != nil {
		t.Error("nil + nil is not nil")
	}
	// The result never aliases its inputs.
	if c := lowerIOLimits(snap, nil); c == snap {
		t.Error("result aliases the snapshot's limits")
	}
}

func TestClampIOLimits(t *testing.T) {
	got := clampIOLimits(&types.IOLimits{DiskMiBs: 500, NetMbit: 10}, testCeil)
	if want := (types.IOLimits{DiskMiBs: 100, NetMbit: 10}); *got != want {
		t.Errorf("got %+v, want %+v", *got, want)
	}
	if err := ValidateIOLimits(got, testCeil); err != nil {
		t.Errorf("a clamped request does not validate: %v", err)
	}
}
