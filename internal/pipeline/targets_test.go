package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/strategy"
)

// dumpAndFolderYAML is a service with two parts: a database dump
// ("data") and a folder named "library".
const dumpAndFolderYAML = `
apiVersion: ballast.dev/v1
service: photos
version: 1
backup:
  strategy: dump
  dumps:
    - name: data
      container: mycontainer
      command: ["mydump"]
      format: sql
  files:
    - name: library
      path: %q
verify:
  method: archive-integrity
`

// targetHarness is a harness with a second destination and a restic
// stub that records which repository each call used and can refuse to
// upload to any repository whose location contains "bad".
func targetHarness(t *testing.T) (*testHarness, string) {
	t.Helper()
	h := newHarness(t)
	for _, name := range []string{"offsite", "bad-offsite"} {
		if err := os.WriteFile(filepath.Join(h.credDir, "ballast-restic-password."+name), []byte("stub-password\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := inventory.PutDestination(h.db, inventory.Destination{
			Name: name, Location: "/stub-" + name, KeepDaily: 3, KeepWeekly: 1, KeepMonthly: 1,
		}); err != nil {
			t.Fatalf("PutDestination %s: %v", name, err)
		}
	}
	repoLog := filepath.Join(t.TempDir(), "repos.log")
	wrapper := writeStub(t, "restic-wrapper", `#!/bin/sh
printf '%s %s\n' "$RESTIC_REPOSITORY" "$*" >> "`+repoLog+`"
if [ "$1" = backup ]; then
  case "$RESTIC_REPOSITORY" in *bad*) echo "stub: destination unreachable" >&2; exit 1;; esac
fi
exec `+h.resticBin+` "$@"
`)
	h.resticBin = wrapper
	return h, repoLog
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func linesFor(lines []string, repo, sub string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, repo+" ") && strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func TestRunFansOutToEveryTarget(t *testing.T) {
	tests := []struct {
		name      string
		targets   []inventory.Target
		wantState engine.State
		wantErr   []string // substrings of the returned error
		// wantTarget is each destination's expected state.
		wantTarget map[string]engine.State
		// wantRetention lists destinations whose old backups get cleaned up.
		wantRetention []string
	}{
		{
			name:          "two destinations both verified",
			targets:       []inventory.Target{{Destination: "local"}, {Destination: "offsite"}},
			wantState:     engine.SUCCESS,
			wantTarget:    map[string]engine.State{"local": engine.SUCCESS, "offsite": engine.SUCCESS},
			wantRetention: []string{"/stub-repo", "/stub-offsite"},
		},
		{
			name:          "one destination down never hides the other",
			targets:       []inventory.Target{{Destination: "local"}, {Destination: "bad-offsite"}},
			wantState:     engine.FAILED,
			wantErr:       []string{"bad-offsite", "unreachable", "the copy to local is fine"},
			wantTarget:    map[string]engine.State{"local": engine.SUCCESS, "bad-offsite": engine.FAILED},
			wantRetention: []string{"/stub-repo"},
		},
		{
			name:       "primary down and secondary fine is still red",
			targets:    []inventory.Target{{Destination: "bad-offsite"}, {Destination: "offsite"}},
			wantState:  engine.FAILED,
			wantErr:    []string{"bad-offsite", "the copy to offsite is fine"},
			wantTarget: map[string]engine.State{"bad-offsite": engine.FAILED, "offsite": engine.SUCCESS},
		},
		{
			name:       "a part that does not exist fails only that target",
			targets:    []inventory.Target{{Destination: "local"}, {Destination: "offsite", Parts: []string{"photos"}}},
			wantState:  engine.FAILED,
			wantErr:    []string{"no part called \"photos\"", "data, library"},
			wantTarget: map[string]engine.State{"local": engine.SUCCESS, "offsite": engine.FAILED},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, repoLog := targetHarness(t)
			p := h.pipeline()
			p.TargetParallel = 2
			libDir := t.TempDir()
			p.Catalog = registryWith(t, map[string]string{"photos.yaml": yamlf(dumpAndFolderYAML, libDir)})
			svc := inventory.Service{Name: "photos", CatalogEntry: "photos", Destination: tt.targets[0].Destination, Targets: tt.targets}
			if err := inventory.PutService(h.db, svc); err != nil {
				t.Fatalf("PutService: %v", err)
			}
			state, err := p.Run(context.Background(), "photos")
			if state != tt.wantState {
				t.Fatalf("state = %s (err %v), want %s", state, err, tt.wantState)
			}
			for _, want := range tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
			runs, _ := engine.RecentRuns(h.db, "photos", 1)
			rts, err := engine.RunTargets(h.db, runs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]engine.State{}
			for _, r := range rts {
				got[r.Destination] = r.State
			}
			for dest, want := range tt.wantTarget {
				if got[dest] != want {
					t.Errorf("target %s state = %s, want %s (all: %v)", dest, got[dest], want, got)
				}
			}
			lines := readLog(t, repoLog)
			for _, repo := range tt.wantRetention {
				if len(linesFor(lines, repo, "forget")) == 0 {
					t.Errorf("no retention cleanup ran for %s; log:\n%s", repo, strings.Join(lines, "\n"))
				}
			}
			// The dump ran once however many destinations there are.
			dockerLines := readLog(t, h.dockerLog)
			dumps := 0
			for _, l := range dockerLines {
				if strings.Contains(l, "exec") && strings.Contains(l, "mydump") {
					dumps++
				}
			}
			if dumps != 1 {
				t.Errorf("dump ran %d times, want exactly once", dumps)
			}
			// Every destination that was read back has its own result.
			results, _ := engine.VerificationResults(h.db, runs[0].ID)
			for dest, st := range got {
				if st != engine.SUCCESS {
					continue
				}
				found := false
				for _, r := range results {
					if r.Method == "restic-read-back" && r.Target == dest && r.Passed {
						found = true
					}
				}
				if !found {
					t.Errorf("no passing restic-read-back recorded for %s", dest)
				}
			}
		})
	}
}

func TestRunSendsOnlyTheChosenParts(t *testing.T) {
	h, repoLog := targetHarness(t)
	p := h.pipeline()
	libDir := t.TempDir()
	p.Catalog = registryWith(t, map[string]string{"photos.yaml": yamlf(dumpAndFolderYAML, libDir)})
	svc := inventory.Service{Name: "photos", CatalogEntry: "photos", Destination: "local", Targets: []inventory.Target{
		{Destination: "local", Parts: []string{"data"}},
		{Destination: "offsite", Parts: []string{"library"}},
	}}
	if err := inventory.PutService(h.db, svc); err != nil {
		t.Fatal(err)
	}
	if state, err := p.Run(context.Background(), "photos"); state != engine.SUCCESS {
		t.Fatalf("state = %s, err = %v", state, err)
	}
	lines := readLog(t, repoLog)
	local := strings.Join(linesFor(lines, "/stub-repo", "backup"), "\n")
	offsite := strings.Join(linesFor(lines, "/stub-offsite", "backup"), "\n")
	if !strings.Contains(local, "data") || strings.Contains(local, libDir) {
		t.Errorf("local backup = %q, want the database dump and not the library %s", local, libDir)
	}
	if !strings.Contains(offsite, libDir) || strings.Contains(offsite, "data.sql") {
		t.Errorf("offsite backup = %q, want only the library %s", offsite, libDir)
	}
}

func TestSelectParts(t *testing.T) {
	res := &strategy.Result{
		Artifacts: []strategy.Artifact{{Name: "db", Path: "/w/db.sql"}},
		Paths:     []string{"/lib", "/other"},
		PathParts: map[string]string{"/lib": "library"},
	}
	tests := []struct {
		name      string
		parts     []string
		wantPaths []string
		wantArts  int
		wantErr   string
	}{
		{name: "no parts means everything", wantPaths: []string{"/lib", "/other"}, wantArts: 1},
		{name: "database only", parts: []string{"db"}, wantArts: 1},
		{name: "named folder only", parts: []string{"library"}, wantPaths: []string{"/lib"}},
		{name: "unnamed folders are the files part", parts: []string{"files"}, wantPaths: []string{"/other"}},
		{name: "unknown part", parts: []string{"nope"}, wantErr: "db, files, library"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths, arts, err := selectParts(res, inventory.Target{Destination: "d", Parts: tt.parts})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(paths, ",") != strings.Join(tt.wantPaths, ",") || len(arts) != tt.wantArts {
				t.Errorf("paths = %v, arts = %d; want %v, %d", paths, len(arts), tt.wantPaths, tt.wantArts)
			}
		})
	}
}

func TestClearStaleLocksWithoutHealthIsNoop(t *testing.T) {
	p := &Pipeline{}
	p.clearStaleLocks(context.Background(), "svc", &runTarget{})
}
