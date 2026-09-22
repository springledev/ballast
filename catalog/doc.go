// Package catalog holds Ballast's bundled service catalog entries:
// YAML data files, one per supported service (spec §6 — "catalog as
// data, not code"). Adding support for a new service means adding a
// YAML file here, not Go code. The entries are validated against the
// versioned JSON schema by the tests in this directory and loaded by
// internal/catalog.
package catalog
