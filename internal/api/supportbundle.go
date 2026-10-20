package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/secret"
)

// BundleFile is one file in a support bundle.
type BundleFile struct {
	// Name is the file name inside the archive.
	Name string
	// Data is the file content, with every secret already removed.
	Data []byte
}

// minBundleSecretLen is the shortest credential-file value that is also
// scrubbed from the bundle; shorter values would mangle ordinary words.
const minBundleSecretLen = 6

// maxBundleCredentialBytes caps how much of one credential file is read
// when collecting values to scrub (an SSH key is a few kilobytes).
const maxBundleCredentialBytes = 64 << 10

// SupportBundle gathers what someone needs to help with a problem:
// version, the doctor report, status, recent alert decisions, the
// database version, restic and Docker versions and catalog entry names.
// Nothing is sent anywhere. Every file is scrubbed of every known
// secret, and of the contents of every credential file, before it is
// returned (rule 2). A part that cannot be gathered is replaced by a
// plain-English note so one problem never empties the whole bundle.
func (o *Ops) SupportBundle(ctx context.Context) []BundleFile {
	scrub := o.bundleScrubber()
	var files []BundleFile
	add := func(name, text string) {
		files = append(files, BundleFile{Name: name, Data: []byte(scrub.Redact(text))})
	}
	addJSON := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			add(name, fmt.Sprintf("could not be written: %v\n", err))
			return
		}
		add(name, string(b)+"\n")
	}

	add("version.txt", "Ballast "+o.version()+"\n")

	if o.DB != nil {
		if v, err := store.SchemaVersionOf(o.DB); err != nil {
			add("schema.txt", "Could not read the database version: "+err.Error()+"\n")
		} else {
			add("schema.txt", fmt.Sprintf("database version %d\n", v))
		}
	} else {
		add("schema.txt", "No database is open.\n")
	}

	report, err := o.Doctor(ctx)
	if err != nil {
		add("doctor.json", "The doctor report could not be completed: "+err.Error()+"\n")
	} else {
		addJSON("doctor.json", report)
	}

	statuses, err := o.Status(ctx, "", false)
	if err != nil {
		add("status.json", "Status could not be read: "+err.Error()+"\n")
		add("alert-decisions.json", "Status could not be read, so there are no alert decisions to show.\n")
	} else {
		addJSON("status.json", statuses)
		decisions := map[string]any{}
		for _, s := range statuses {
			decisions[s.Service] = s.AlertDecisions
		}
		addJSON("alert-decisions.json", decisions)
	}

	pre := o.CheckPreflight(ctx)
	var tools strings.Builder
	if pre.ResticInstalled {
		fmt.Fprintf(&tools, "restic: %s\n", pre.ResticVersion)
	} else {
		fmt.Fprintf(&tools, "restic: not usable (%s)\n", pre.ResticError)
	}
	if pre.DockerInstalled {
		tools.WriteString("docker: installed\n")
	} else {
		fmt.Fprintf(&tools, "docker: not usable (%s)\n", pre.DockerError)
	}
	for _, c := range report.Checks {
		if c.Area == "docker-host" {
			fmt.Fprintf(&tools, "docker host %s: %s\n", c.Name, c.Detail)
		}
	}
	add("tools.txt", tools.String())

	var names strings.Builder
	for _, c := range o.ListCatalog() {
		names.WriteString(c.Service + "\n")
	}
	add("catalog.txt", names.String())
	return files
}

// bundleScrubber returns a redactor that knows every secret the app has
// loaded plus the contents of every file in the credential directories,
// so a secret added after start-up is covered too.
func (o *Ops) bundleScrubber() *secret.Set {
	set := o.Redact.With()
	for _, dir := range o.CredentialSearchDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if info, err := e.Info(); err != nil || info.Size() > maxBundleCredentialBytes {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			v := strings.TrimSpace(string(b))
			if len(v) >= minBundleSecretLen {
				set.Add(v)
			}
		}
	}
	return set
}
