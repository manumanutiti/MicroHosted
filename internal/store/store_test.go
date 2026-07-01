package store

import (
	"path/filepath"
	"testing"
	"time"

	"microhosted/pkg/types"
)

func sampleVM(id string) *types.VM {
	return &types.VM{
		Config: types.VMConfig{
			ID:           id,
			TemplateName: "ubuntu-22.04",
			Kernel:       "/img/vmlinux",
			Rootfs:       "/img/" + id + ".ext4",
			VCPUs:        2,
			MemMB:        512,
			TapDevice:    "tap" + id,
			GuestIP:      "172.16.0.14",
			HostIP:       "172.16.0.13",
			GatewayIP:    "172.16.0.13",
		},
		State:     types.VMStateRunning,
		PID:       4242,
		VsockPath: "/srv/jailer/firecracker/" + id + "/root/v.sock",
		LogPath:   "/img/" + id + ".log",
		CreatedAt: time.Now().Truncate(time.Second),
	}
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSaveListRoundTrip(t *testing.T) {
	st := openTemp(t)
	want := sampleVM("abc12345")
	if err := st.SaveVM(want); err != nil {
		t.Fatalf("SaveVM: %v", err)
	}

	got, err := st.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if got[0].Config.ID != want.Config.ID || got[0].Config.HostIP != want.Config.HostIP {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got[0].Config, want.Config)
	}
	if got[0].PID != want.PID || got[0].State != want.State {
		t.Fatalf("round-trip lost fields: got pid=%d state=%s", got[0].PID, got[0].State)
	}
	if !got[0].CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("CreatedAt not preserved: got %v want %v", got[0].CreatedAt, want.CreatedAt)
	}
}

func TestSaveIsUpsert(t *testing.T) {
	st := openTemp(t)
	vm := sampleVM("dup00001")
	if err := st.SaveVM(vm); err != nil {
		t.Fatalf("SaveVM: %v", err)
	}
	vm.State = types.VMStateStopped
	if err := st.SaveVM(vm); err != nil {
		t.Fatalf("SaveVM (update): %v", err)
	}

	got, err := st.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("upsert should not duplicate rows: got %d", len(got))
	}
	if got[0].State != types.VMStateStopped {
		t.Fatalf("upsert did not update state: got %s", got[0].State)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	st := openTemp(t)
	// Deleting a record that was never saved (create-rollback path) is fine.
	if err := st.DeleteVM("never-existed"); err != nil {
		t.Fatalf("DeleteVM on missing row: %v", err)
	}

	vm := sampleVM("del00001")
	if err := st.SaveVM(vm); err != nil {
		t.Fatalf("SaveVM: %v", err)
	}
	if err := st.DeleteVM(vm.Config.ID); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	got, err := st.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 records after delete, got %d", len(got))
	}
}

func TestStatePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.db")

	st1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if err := st1.SaveVM(sampleVM("persist01")); err != nil {
		t.Fatalf("SaveVM: %v", err)
	}
	st1.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer st2.Close()
	got, err := st2.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(got) != 1 || got[0].Config.ID != "persist01" {
		t.Fatalf("record did not survive reopen: %+v", got)
	}
}
