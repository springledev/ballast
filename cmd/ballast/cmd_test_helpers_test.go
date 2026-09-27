package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resticStubScript is a minimal, hermetic fake restic CLI: enough for
// init/backup/snapshots/ls/dump/restore/forget to succeed against one
// fixed fake snapshot, so CLI commands that touch a destination
// (destination add --init/test, snapshots, restore) are testable
// without a real restic binary or network.
const resticStubScript = `#!/bin/sh
cmd="$1"
case "$cmd" in
  init) exit 0 ;;
  backup)
    printf '{"message_type":"summary","snapshot_id":"1111111111111111111111111111111111111111111111111111111111111111","files_new":1,"total_files_processed":1}\n'
    ;;
  snapshots)
    printf '[{"time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","tags":["ballast","ballast-service:svc"],"paths":["/stub/data"]}]\n'
    ;;
  ls)
    printf '{"message_type":"snapshot","time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","paths":["/stub/data"]}\n'
    printf '{"message_type":"node","name":"sample.txt","type":"file","path":"/stub/data/sample.txt","size":11}\n'
    ;;
  dump) printf 'sample-content' ;;
  restore) exit 0 ;;
  forget) printf '[]' ;;
  version) printf 'restic 0.17.3 compiled with go1.23.3 on linux/amd64\n' ;;
  *) echo "unknown stub restic command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

// stripLogLines removes every "ballast: ..." log line from execCLI's
// combined stdout+stderr output, leaving only the command's own
// output (e.g. for parsing --json output as pure JSON).
func stripLogLines(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ballast:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// resetCLIFlags restores every cobra flag-bound package var to its
// documented default. See execCLI's call site for why this is
// needed: pflag leaves a flag var at its last-set value across
// repeated Execute() calls when a later invocation simply omits that
// flag, which would otherwise leak state between tests sharing this
// one test binary's rootCmd.
func resetCLIFlags() {
	triggerToken, triggerTokenFile = "", ""

	statusService, statusJSON, statusVerify = "", false, false
	snapshotsJSON = false
	restoreSnapshot, restoreFiles, restoreTo = "", nil, ""

	serviceCatalogEntry, serviceDestination = "", ""
	serviceParams, serviceContainers, serviceSecrets = nil, nil, nil

	destLocation, destPasswordCredential, destInit = "", "", false
	destKeepDaily, destKeepWeekly, destKeepMonthly = 7, 4, 6

	notifyEnabled, notifyMaxAgeHours, notifyHeartbeat = true, 0, false

	secretValueFlag = ""

	verifyAll, verifyJSON = false, false
}

// withStubRestic prepends a directory holding a fake "restic"
// executable to $PATH, so commands that build a restic.Repository via
// wiring's openApp (which resolves "restic" against $PATH) work
// hermetically in tests.
func withStubRestic(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "restic")
	if err := os.WriteFile(path, []byte(resticStubScript), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
