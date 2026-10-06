// Package deploy embeds the shipped service files so `ballast install`
// can write them without needing the source tree.
package deploy

import _ "embed"

// UnitFile is the shipped systemd unit (deploy/ballast.service).
//
//go:embed ballast.service
var UnitFile string
