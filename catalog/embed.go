package catalog

import "embed"

// Bundled embeds every catalog YAML file shipped in the binary (spec
// §6: bundled entries live in catalog/, embedded via go:embed). It is
// the base layer internal/catalog.LoadRegistry loads before any user
// catalog directory is applied on top.
//
//go:embed *.yaml
var Bundled embed.FS
