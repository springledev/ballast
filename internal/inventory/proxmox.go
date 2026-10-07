package inventory

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/proxmox"
)

// PutProxmoxConnection saves Ballast's one Proxmox connection (spec
// §17.3), replacing any earlier one. conn must already be Normalized.
func PutProxmoxConnection(db *sql.DB, conn proxmox.Connection) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	insecure := 0
	if conn.Insecure {
		insecure = 1
	}
	_, err := db.Exec(`
		INSERT INTO proxmox_connection (id, host, token_id, credential, insecure, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host,
			token_id = excluded.token_id,
			credential = excluded.credential,
			insecure = excluded.insecure,
			updated_at = excluded.updated_at`,
		conn.Host, conn.TokenID, conn.Credential, insecure, now, now,
	)
	if err != nil {
		return fmt.Errorf("saving Proxmox connection: %w", err)
	}
	return nil
}

// GetProxmoxConnection returns Ballast's Proxmox connection, or
// ErrNotFound if none is configured yet.
func GetProxmoxConnection(db *sql.DB) (proxmox.Connection, error) {
	var c proxmox.Connection
	var insecure int
	err := db.QueryRow(`SELECT host, token_id, credential, insecure FROM proxmox_connection WHERE id = 1`).
		Scan(&c.Host, &c.TokenID, &c.Credential, &insecure)
	if errors.Is(err, sql.ErrNoRows) {
		return proxmox.Connection{}, fmt.Errorf("the Proxmox connection: %w", ErrNotFound)
	}
	if err != nil {
		return proxmox.Connection{}, fmt.Errorf("getting Proxmox connection: %w", err)
	}
	c.Insecure = insecure != 0
	return c, nil
}

// DeleteProxmoxConnection removes Ballast's Proxmox connection. A
// service still attached to a guest is not affected — its
// proxmox_node/proxmox_vmid are left as inventory until the service is
// edited, but nothing can orchestrate against them without a
// connection.
func DeleteProxmoxConnection(db *sql.DB) error {
	res, err := db.Exec(`DELETE FROM proxmox_connection WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("deleting Proxmox connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("the Proxmox connection: %w", ErrNotFound)
	}
	return nil
}

// RecordProxmoxDump remembers that Ballast created a dump volume, so
// pruning may delete it later. Only recorded volumes are ever pruned.
func RecordProxmoxDump(db *sql.DB, service, node, storage, volume string, runID, size int64) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO proxmox_dumps (volume, service, node, storage, run_id, size, created_at, pruned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '')`,
		volume, service, node, storage, runID, size, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("service %s: recording Proxmox dump %s: %w", service, volume, err)
	}
	return nil
}

// OwnedProxmoxDumps lists the dump volumes service created on storage
// that have not been pruned yet.
func OwnedProxmoxDumps(db *sql.DB, service, node, storage string) ([]string, error) {
	rows, err := db.Query(`SELECT volume FROM proxmox_dumps WHERE service = ? AND node = ? AND storage = ? AND pruned_at = '' ORDER BY created_at`, service, node, storage)
	if err != nil {
		return nil, fmt.Errorf("service %s: listing recorded Proxmox dumps: %w", service, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("service %s: listing recorded Proxmox dumps: %w", service, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MarkProxmoxDumpsPruned stops tracking volumes that were deleted (or
// are already gone from the storage).
func MarkProxmoxDumpsPruned(db *sql.DB, node string, volumes []string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, v := range volumes {
		if _, err := db.Exec(`UPDATE proxmox_dumps SET pruned_at = ? WHERE node = ? AND volume = ?`, now, node, v); err != nil {
			return fmt.Errorf("marking Proxmox dump %s pruned: %w", v, err)
		}
	}
	return nil
}
