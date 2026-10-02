package inventory

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
	"time"
)

// Compose folder kinds (spec §18.4).
const (
	// ComposeDirProject is one stack's own folder, registered by hand.
	ComposeDirProject = "project"
	// ComposeDirScan is a parent folder whose sub-folders are checked
	// for stacks that are defined but not running.
	ComposeDirScan = "scan"
)

// ComposeDir is a folder on a Docker host that Ballast looks in for
// compose stacks.
type ComposeDir struct {
	DockerHost string `json:"docker_host"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
}

// PutComposeDir saves a compose folder for a Docker host. The path
// must be absolute; the host must exist.
func PutComposeDir(db *sql.DB, d ComposeDir) (ComposeDir, error) {
	if d.Kind != ComposeDirProject && d.Kind != ComposeDirScan {
		return ComposeDir{}, fmt.Errorf("compose folder %q: kind must be %q or %q", d.Path, ComposeDirProject, ComposeDirScan)
	}
	if !strings.HasPrefix(d.Path, "/") {
		return ComposeDir{}, fmt.Errorf("compose folder %q: give the full path on the Docker host, starting with /", d.Path)
	}
	d.Path = path.Clean(d.Path)
	if _, err := GetDockerHost(db, d.DockerHost); err != nil {
		return ComposeDir{}, err
	}
	_, err := db.Exec(`
		INSERT INTO compose_dirs (docker_host, path, kind, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(docker_host, path) DO UPDATE SET kind = excluded.kind`,
		d.DockerHost, d.Path, d.Kind, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return ComposeDir{}, fmt.Errorf("saving compose folder %s on docker host %s: %w", d.Path, d.DockerHost, err)
	}
	return d, nil
}

// ListComposeDirs returns a Docker host's compose folders, by path.
func ListComposeDirs(db *sql.DB, dockerHost string) ([]ComposeDir, error) {
	rows, err := db.Query(`SELECT docker_host, path, kind FROM compose_dirs WHERE docker_host = ? ORDER BY path`, dockerHost)
	if err != nil {
		return nil, fmt.Errorf("listing compose folders on docker host %s: %w", dockerHost, err)
	}
	defer rows.Close()
	out := []ComposeDir{}
	for rows.Next() {
		var d ComposeDir
		if err := rows.Scan(&d.DockerHost, &d.Path, &d.Kind); err != nil {
			return nil, fmt.Errorf("listing compose folders on docker host %s: %w", dockerHost, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing compose folders on docker host %s: %w", dockerHost, err)
	}
	return out, nil
}

// DeleteComposeDir removes a compose folder, or returns ErrNotFound.
func DeleteComposeDir(db *sql.DB, dockerHost, p string) error {
	res, err := db.Exec(`DELETE FROM compose_dirs WHERE docker_host = ? AND path = ?`, dockerHost, path.Clean(p))
	if err != nil {
		return fmt.Errorf("removing compose folder %s on docker host %s: %w", p, dockerHost, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("compose folder %s on docker host %s: %w", p, dockerHost, ErrNotFound)
	}
	return nil
}
