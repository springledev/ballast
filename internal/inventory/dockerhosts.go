package inventory

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/dockerhost"
	"github.com/springledev/ballast/internal/store"
)

// PutDockerHost inserts or updates a Docker host. The name is
// validated and h is normalized (canonical address) before saving; the
// saved form is returned. The built-in "local" host can be pointed at
// a different socket but must stay a local host.
func PutDockerHost(db *sql.DB, h dockerhost.Host) (dockerhost.Host, error) {
	if err := ValidateName(h.Name); err != nil {
		return dockerhost.Host{}, fmt.Errorf("docker host: %w", err)
	}
	if h.Name == dockerhost.LocalName && h.Kind != dockerhost.Local {
		return dockerhost.Host{}, fmt.Errorf("docker host %s: the built-in local host must stay a local host; give a remote host its own name", h.Name)
	}
	if err := h.Normalize(); err != nil {
		return dockerhost.Host{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		INSERT INTO docker_hosts (name, kind, address, credential, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			kind = excluded.kind,
			address = excluded.address,
			credential = excluded.credential,
			updated_at = excluded.updated_at`,
		h.Name, string(h.Kind), h.Address, h.Credential, now, now,
	)
	if err != nil {
		return dockerhost.Host{}, fmt.Errorf("saving docker host %s: %w", h.Name, err)
	}
	return h, nil
}

func scanDockerHost(row rowScanner) (dockerhost.Host, error) {
	var h dockerhost.Host
	var kind string
	if err := row.Scan(&h.Name, &kind, &h.Address, &h.Credential); err != nil {
		return dockerhost.Host{}, err
	}
	h.Kind = dockerhost.Kind(kind)
	return h, nil
}

// GetDockerHost returns the named Docker host, or ErrNotFound.
func GetDockerHost(db *sql.DB, name string) (dockerhost.Host, error) {
	h, err := scanDockerHost(db.QueryRow(`SELECT name, kind, address, credential FROM docker_hosts WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return dockerhost.Host{}, fmt.Errorf("docker host %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return dockerhost.Host{}, fmt.Errorf("getting docker host %s: %w", name, err)
	}
	return h, nil
}

// ListDockerHosts returns every Docker host, the built-in local host
// first, then by name.
func ListDockerHosts(db *sql.DB) ([]dockerhost.Host, error) {
	rows, err := db.Query(`SELECT name, kind, address, credential FROM docker_hosts ORDER BY name != 'local', name`)
	if err != nil {
		return nil, fmt.Errorf("listing docker hosts: %w", err)
	}
	defer rows.Close()
	out := []dockerhost.Host{}
	for rows.Next() {
		h, err := scanDockerHost(rows)
		if err != nil {
			return nil, fmt.Errorf("listing docker hosts: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing docker hosts: %w", err)
	}
	return out, nil
}

// DeleteDockerHost removes a Docker host. The built-in local host
// cannot be removed; a host still used by a service fails with
// ErrInUse. The check and delete share one transaction, as in
// DeleteDestination.
func DeleteDockerHost(db *sql.DB, name string) (err error) {
	if name == dockerhost.LocalName {
		return fmt.Errorf("docker host %s: the built-in local host can't be removed", name)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("deleting docker host %s: %w", name, err)
	}
	defer func() { err = store.RollbackOnError(tx, err) }()

	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM services WHERE docker_host = ?`, name).Scan(&count); err != nil {
		return fmt.Errorf("checking docker host %s usage: %w", name, err)
	}
	if count > 0 {
		return fmt.Errorf("docker host %s: %w (used by %d service(s))", name, ErrInUse, count)
	}
	res, err := tx.Exec(`DELETE FROM docker_hosts WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("deleting docker host %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("docker host %s: %w", name, ErrNotFound)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("deleting docker host %s: %w", name, err)
	}
	return nil
}
