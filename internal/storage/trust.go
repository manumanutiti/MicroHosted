package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// StateDir is where the daemon keeps its own state: the SQLite database, the
// template catalog and the disk store. Everything under it belongs to root.
const StateDir = "/var/lib/microhosted"

// Default locations of the daemon's database and catalog.
var (
	DefaultDBPath      = filepath.Join(StateDir, "microhosted.db")
	DefaultCatalogPath = filepath.Join(StateDir, "catalog.json")
)

// CheckRootOnly verifies that nobody but root can change what the daemon will
// read at path: the file itself (when it exists) and every directory leading to
// it, after resolving symlinks, must be owned by root and writable by no one
// else.
//
// The database and the catalog are trusted input for a daemon running as root:
// a VM record names the paths it truncates (its console log), clones and deletes
// (its disk), and a template names the kernel and disk it boots. Whoever can
// write either file — or replace it through a writable directory above it —
// can steer those root operations at any file on the host. Kept in a user's
// checkout, that is every process running as that user.
func CheckRootOnly(path string) error {
	return checkOwnedOnly(path, 0)
}

// checkOwnedOnly is CheckRootOnly for an arbitrary owner, so tests can run it
// unprivileged.
func checkOwnedOnly(path string, owner uint32) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Resolve what exists: the file may not have been created yet (a first
	// start creates the database), but its directory must.
	target := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		target = resolved
	} else if errors.Is(err, os.ErrNotExist) {
		dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Dir(abs), err)
		}
		target = filepath.Join(dir, filepath.Base(abs))
	} else {
		return err
	}

	var chain []string
	for p := target; ; p = filepath.Dir(p) {
		chain = append(chain, p)
		if p == filepath.Dir(p) {
			break
		}
	}
	return checkChain(chain, owner)
}

// checkChain checks each path — the target first, then its directories — for
// owner and for group/other write access. Only the first (the target itself)
// may be missing.
func checkChain(chain []string, owner uint32) error {
	for i, p := range chain {
		fi, err := os.Lstat(p)
		if err != nil {
			if i == 0 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%s: %w", p, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: cannot read its owner", p)
		}
		if st.Uid != owner {
			return fmt.Errorf("%s is owned by uid %d, not %d: that user can change what the daemon trusts", p, st.Uid, owner)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by group or others (mode %v)", p, fi.Mode().Perm())
		}
	}
	return nil
}

// RestrictDB makes the SQLite database and whichever of its side files exist
// (-wal, -shm, -journal) readable and writable by the daemon's user alone.
// SQLite creates side files with the database's own mode, so tightening the
// database keeps the later ones private too. A missing file is not an error.
func RestrictDB(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := chmodRegular(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
