// Package store is the durable side of the manager's bookkeeping. Without it
// a daemon restart forgets every VM it was tracking — the Firecracker
// processes keep running but become orphans nothing can list, exec into, or
// clean up. It persists the manager's VM records to SQLite so a restart can
// reconcile against reality (see vm.Manager.Reconcile) instead of starting
// blind.
//
// Records are stored as a JSON blob per row rather than a wide column schema
// on purpose: the shape of what's tracked still grows every phase (networks,
// volumes), and a blob keeps the storage layer stable while types.VM evolves.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver (no cgo), registered as "sqlite"

	"microhosted/pkg/types"
)

// Store is a SQLite-backed persistence layer for VM records.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists. The caller owns closing it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite at %s: %w", path, err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS vms (
		id   TEXT PRIMARY KEY,
		data TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveVM upserts a VM record. Called on create and on any state transition
// worth surviving a restart.
func (s *Store) SaveVM(vm *types.VM) error {
	data, err := json.Marshal(vm)
	if err != nil {
		return fmt.Errorf("marshaling vm %s: %w", vm.Config.ID, err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO vms (id, data) VALUES (?, ?)
		 ON CONFLICT(id) DO UPDATE SET data = excluded.data`,
		vm.Config.ID, string(data),
	); err != nil {
		return fmt.Errorf("saving vm %s: %w", vm.Config.ID, err)
	}
	return nil
}

// DeleteVM removes a VM record. Idempotent: deleting a row that isn't there is
// not an error, so it's safe to call on a create rollback where the record was
// never persisted.
func (s *Store) DeleteVM(id string) error {
	if _, err := s.db.Exec(`DELETE FROM vms WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting vm %s: %w", id, err)
	}
	return nil
}

// ListVMs returns every persisted VM record, for reconciliation at startup.
func (s *Store) ListVMs() ([]*types.VM, error) {
	rows, err := s.db.Query(`SELECT data FROM vms`)
	if err != nil {
		return nil, fmt.Errorf("querying vms: %w", err)
	}
	defer rows.Close()

	var vms []*types.VM
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scanning vm row: %w", err)
		}
		var vm types.VM
		if err := json.Unmarshal([]byte(data), &vm); err != nil {
			return nil, fmt.Errorf("unmarshaling vm row: %w", err)
		}
		vms = append(vms, &vm)
	}
	return vms, rows.Err()
}
