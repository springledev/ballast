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
