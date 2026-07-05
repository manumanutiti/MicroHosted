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
	// SQLite allows one writer at a time; with the pool's default of many
	// connections, concurrent writers (e.g. parallel bulk destroy) surface as
	// SQLITE_BUSY errors instead of queueing. A single connection makes the
	// pool itself the queue. Statements here are all point reads/writes, so
	// serialization costs microseconds.
	db.SetMaxOpenConns(1)

	schema := []string{
		`CREATE TABLE IF NOT EXISTS vms (
			id   TEXT PRIMARY KEY,
			data TEXT NOT NULL
		)`,
		// name is UNIQUE so the DB itself enforces one network per name — the
		// manager relies on this instead of a racy check-then-insert.
		`CREATE TABLE IF NOT EXISTS networks (
			id   TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			data TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			id   TEXT PRIMARY KEY,
			data TEXT NOT NULL
		)`,
		// name is UNIQUE so the DB enforces one volume per name — the manager
		// resolves attach requests by name and relies on this to keep them
		// unambiguous, same as networks.
		`CREATE TABLE IF NOT EXISTS volumes (
			id   TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			data TEXT NOT NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("creating schema: %w", err)
		}
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

// SaveSnapshot upserts a snapshot record.
func (s *Store) SaveSnapshot(snap *types.Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot %s: %w", snap.ID, err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO snapshots (id, data) VALUES (?, ?)
		 ON CONFLICT(id) DO UPDATE SET data = excluded.data`,
		snap.ID, string(data),
	); err != nil {
		return fmt.Errorf("saving snapshot %s: %w", snap.ID, err)
	}
	return nil
}

// DeleteSnapshot removes a snapshot record. Idempotent.
func (s *Store) DeleteSnapshot(id string) error {
	if _, err := s.db.Exec(`DELETE FROM snapshots WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting snapshot %s: %w", id, err)
	}
	return nil
}

// ListSnapshots returns every persisted snapshot record, loaded once at
// startup — snapshots are inert files plus this row, so unlike VMs there is
// no liveness to reconcile against.
func (s *Store) ListSnapshots() ([]*types.Snapshot, error) {
	rows, err := s.db.Query(`SELECT data FROM snapshots`)
	if err != nil {
		return nil, fmt.Errorf("querying snapshots: %w", err)
	}
	defer rows.Close()

	var snaps []*types.Snapshot
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scanning snapshot row: %w", err)
		}
		var snap types.Snapshot
		if err := json.Unmarshal([]byte(data), &snap); err != nil {
			return nil, fmt.Errorf("unmarshaling snapshot row: %w", err)
		}
		snaps = append(snaps, &snap)
	}
	return snaps, rows.Err()
}

// SaveVolume upserts a volume record. Called on create and whenever a volume's
// attachment changes (attach on VM create, release on destroy).
func (s *Store) SaveVolume(v *types.Volume) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling volume %s: %w", v.Name, err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO volumes (id, name, data) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, data = excluded.data`,
		v.ID, v.Name, string(data),
	); err != nil {
		return fmt.Errorf("saving volume %s: %w", v.Name, err)
	}
	return nil
}

// DeleteVolume removes a volume record by id. Idempotent.
func (s *Store) DeleteVolume(id string) error {
	if _, err := s.db.Exec(`DELETE FROM volumes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting volume %s: %w", id, err)
	}
	return nil
}

// ListVolumes returns every persisted volume record, loaded once at startup.
func (s *Store) ListVolumes() ([]*types.Volume, error) {
	rows, err := s.db.Query(`SELECT data FROM volumes`)
	if err != nil {
		return nil, fmt.Errorf("querying volumes: %w", err)
	}
	defer rows.Close()

	var vols []*types.Volume
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scanning volume row: %w", err)
		}
		var v types.Volume
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, fmt.Errorf("unmarshaling volume row: %w", err)
		}
		vols = append(vols, &v)
	}
	return vols, rows.Err()
}

// SaveNetwork upserts a network record. The UNIQUE constraint on name means a
// second network with the same name but a different id fails here rather than
// silently duplicating.
func (s *Store) SaveNetwork(n *types.Network) error {
	data, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshaling network %s: %w", n.Name, err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO networks (id, name, data) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, data = excluded.data`,
		n.ID, n.Name, string(data),
	); err != nil {
		return fmt.Errorf("saving network %s: %w", n.Name, err)
	}
	return nil
}

// DeleteNetwork removes a network record by id. Idempotent.
func (s *Store) DeleteNetwork(id string) error {
	if _, err := s.db.Exec(`DELETE FROM networks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting network %s: %w", id, err)
	}
	return nil
}

// ListNetworks returns every persisted network record, for reconciliation at
// startup (recreating bridges + rules).
func (s *Store) ListNetworks() ([]*types.Network, error) {
	rows, err := s.db.Query(`SELECT data FROM networks`)
	if err != nil {
		return nil, fmt.Errorf("querying networks: %w", err)
	}
	defer rows.Close()

	var nets []*types.Network
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scanning network row: %w", err)
		}
		var n types.Network
		if err := json.Unmarshal([]byte(data), &n); err != nil {
			return nil, fmt.Errorf("unmarshaling network row: %w", err)
		}
		nets = append(nets, &n)
	}
	return nets, rows.Err()
}
