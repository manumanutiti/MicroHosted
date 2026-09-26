package jailer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func withIDFiles(t *testing.T, passwd, group, subuid, subgid string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldP, oldG, oldSU, oldSG := passwdFile, groupFile, subuidFile, subgidFile
	passwdFile, groupFile = write("passwd", passwd), write("group", group)
	subuidFile, subgidFile = write("subuid", subuid), write("subgid", subgid)
	t.Cleanup(func() { passwdFile, groupFile, subuidFile, subgidFile = oldP, oldG, oldSU, oldSG })
}

func TestValidateIDRange(t *testing.T) {
	withIDFiles(t,
		"root:x:0:0::/root:/bin/bash\nmanu:x:1001:1001::/home/manu:/bin/bash\n",
		"root:x:0:\nusers:x:100:\n",
		"manu:165536:65536\n",
		"manu:165536:65536\n")
	d := Defaults{IDBase: DefaultIDBase, IDCount: DefaultIDCount}
	if err := d.ValidateIDRange(); err != nil {
		t.Fatalf("default range rejected on a typical host: %v", err)
	}
	// Overlapping a subordinate range (rootless containers) is refused.
	d = Defaults{IDBase: 200000, IDCount: 1000}
	if err := d.ValidateIDRange(); !errors.Is(err, ErrIDRangeOverlap) {
		t.Fatalf("subuid overlap: %v", err)
	}
	// So is a range reaching an account, or one in the classic low ids.
	withIDFiles(t, "svc:x:1900000010:1::/:/sbin/nologin\n", "", "", "")
	d = Defaults{IDBase: DefaultIDBase, IDCount: 100}
	if err := d.ValidateIDRange(); !errors.Is(err, ErrIDRangeOverlap) {
		t.Fatalf("passwd overlap: %v", err)
	}
	if err := (Defaults{IDBase: 100, IDCount: 10}).ValidateIDRange(); err == nil {
		t.Fatal("accepted a range among system ids")
	}
	if err := (Defaults{IDBase: DefaultIDBase, IDCount: 1 << 30}).ValidateIDRange(); err == nil {
		t.Fatal("accepted a range reaching 2^31")
	}
}

// LiveUIDs sees at least this process.
func TestLiveUIDsIncludesSelf(t *testing.T) {
	uids, err := LiveUIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !uids[os.Getuid()] {
		t.Fatalf("own uid %d missing", os.Getuid())
	}
}
