package notify

import "github.com/springledev/keel/runner"

// LoadCredentialValue reads the trimmed contents of a root-only
// credential file (systemd LoadCredential= semantics via
// $CREDENTIALS_DIRECTORY, spec §8) and returns it as the secret value
// itself — e.g. an ntfy topic URL or webhook URL.
//
// ntfy/webhook delivery happens over net/http inside the Ballast
// process, so the channel needs the secret value in memory rather
// than a file path in an env var like internal/runner.FileEnv builds.
// This is a thin wrapper over the shared runner.LoadCredentialValue
// (also used by internal/pipeline to resolve catalog
// {{ .Secrets.NAME }} values), kept here under its original name so
// existing callers are unaffected.
func LoadCredentialValue(dir, name string) (string, error) {
	return runner.LoadCredentialValue(dir, name)
}
