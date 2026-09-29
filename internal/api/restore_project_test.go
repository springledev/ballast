package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/keel/runner"
)

func TestRestoreProjectRequiresProjectAndTo(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{To: "/tmp/x"}); !errors.Is(err, ErrValidation) {
		t.Errorf("RestoreProject without a project = %v, want ErrValidation", err)
	}
	if _, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack"}); !errors.Is(err, ErrValidation) {
		t.Errorf("RestoreProject without --to = %v, want ErrValidation", err)
	}
	if _, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack", To: "relative/path"}); !errors.Is(err, ErrValidation) {
		t.Errorf("RestoreProject with a relative --to = %v, want ErrValidation", err)
	}
}

func TestRestoreProjectUnknownProject(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "ghost", To: filepath.Join(t.TempDir(), "out")}); !errors.Is(err, ErrValidation) {
		t.Errorf("RestoreProject on an unknown project = %v, want ErrValidation", err)
	}
}

// TestRestoreProjectIsolatesFailures pins spec rule 3 (isolation of
// failure): a project with one service pointing at a catalog entry
// that is no longer loaded must still restore the other services in
// the project, and must name only the broken one in its error.
func TestRestoreProjectIsolatesFailures(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "good", CatalogEntry: "folder", Destination: "local", Project: "stack",
	}); err != nil {
		t.Fatalf("PutService good: %v", err)
	}
	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "broken", CatalogEntry: "ghost-entry", Destination: "local", Project: "stack",
	}); err != nil {
		t.Fatalf("PutService broken: %v", err)
	}

	to := filepath.Join(t.TempDir(), "restore-out")
	rep, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack", To: to})
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("RestoreProject error = %v, want ErrValidation naming the broken service", err)
	}
	if !strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "good") {
		t.Errorf("error %q should name only the broken service", err.Error())
	}

	byName := map[string]RestoreProjectResult{}
	for _, r := range rep.Results {
		byName[r.Service] = r
	}
	good, ok := byName["good"]
	if !ok || good.Error != "" {
		t.Fatalf("good service result = %+v, want a successful restore", good)
	}
	if _, err := os.Stat(good.RestoredTo); err != nil {
		t.Errorf("good service's restored directory %s does not exist: %v", good.RestoredTo, err)
	}
	broken, ok := byName["broken"]
	if !ok || broken.Error == "" {
		t.Fatalf("broken service result = %+v, want an error", broken)
	}
}

// TestRestoreProjectOrdersByTier pins the dependency order (spec §14
// step 23): compose config first, volumes next, DB dumps last.
func TestRestoreProjectOrdersByTier(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	composeYAML := `
apiVersion: ballast.dev/v1
service: compose-svc
version: 1
backup:
  strategy: compose
  compose:
    project: demo
verify:
  method: archive-integrity
`
	volumeYAML := `
apiVersion: ballast.dev/v1
service: volume-svc
version: 1
backup:
  strategy: volume
  volumes:
    - name: data
verify:
  method: archive-integrity
`
	dumpYAML := `
apiVersion: ballast.dev/v1
service: dump-svc
version: 1
backup:
  strategy: dump
  dumps:
    - name: dump
      container: db
      command: ["true"]
      format: sql
verify:
  method: archive-integrity
`
	reg, err := catalog.LoadRegistry(fstest.MapFS{
		"compose.yaml": &fstest.MapFile{Data: []byte(composeYAML)},
		"volume.yaml":  &fstest.MapFile{Data: []byte(volumeYAML)},
		"dump.yaml":    &fstest.MapFile{Data: []byte(dumpYAML)},
	}, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	h.ops.Catalog = reg

	// Added out of order on purpose: dump, then compose, then volume.
	for _, s := range []inventory.Service{
		{Name: "dump-svc", CatalogEntry: "dump-svc", Destination: "local", Project: "stack"},
		{Name: "compose-svc", CatalogEntry: "compose-svc", Destination: "local", Project: "stack"},
		{Name: "volume-svc", CatalogEntry: "volume-svc", Destination: "local", Project: "stack"},
	} {
		if err := inventory.PutService(h.ops.DB, s); err != nil {
			t.Fatalf("PutService %s: %v", s.Name, err)
		}
	}

	to := filepath.Join(t.TempDir(), "out")
	rep, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack", To: to})
	// volume-svc has no volume archive in the stub's canned snapshot,
	// so it fails — that is expected and covered by the isolation test
	// above; this test only cares about ordering.
	if err == nil {
		t.Log("RestoreProject unexpectedly had no errors")
	}

	got := make([]string, len(rep.Results))
	for i, r := range rep.Results {
		got[i] = r.Kind
	}
	want := []string{"compose-config", "volume", "dump"}
	if !equalStrings(got, want) {
		t.Errorf("restore order (by kind) = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFindVolumeArtifacts(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("work/svc/volume-data.tar")
	mustWrite("work/svc/volume-media.tar")
	mustWrite("work/svc/compose-config.tar") // not a volume artifact
	mustWrite("work/svc/database.sql")       // not a volume artifact

	got, err := findVolumeArtifacts(dir)
	if err != nil {
		t.Fatalf("findVolumeArtifacts: %v", err)
	}
	names := make([]string, len(got))
	for i, a := range got {
		names[i] = a.volumeName
	}
	sort.Strings(names)
	want := []string{"data", "media"}
	if !equalStrings(names, want) {
		t.Errorf("findVolumeArtifacts volume names = %v, want %v", names, want)
	}
}

// restoreVolumeResticStub is a restic stub whose "restore" command
// writes a real volume-*.tar artifact under --target, so
// RestoreProject's volume-extraction path can be exercised end to end
// without a real restic binary.
const restoreVolumeResticStub = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
cmd="$1"
case "$cmd" in
  init) exit 0 ;;
  backup)
    printf '{"message_type":"summary","snapshot_id":"1111111111111111111111111111111111111111111111111111111111111111","files_new":1,"total_files_processed":1}\n'
    ;;
  snapshots)
    printf '[{"time":"2024-01-01T00:00:00Z","id":"1111111111111111111111111111111111111111111111111111111111111111","short_id":"11111111","hostname":"test","tags":["ballast","ballast-service:svc"],"paths":["/stub/data"]}]\n'
    ;;
  restore)
    shift
    target=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--target" ]; then target="$arg"; fi
      prev="$arg"
    done
    mkdir -p "$target/work/svc"
    printf 'FAKE-TAR' > "$target/work/svc/volume-data.tar"
    ;;
  *) echo "unknown stub restic command: $cmd" >&2; exit 2 ;;
esac
exit 0
`

// dockerRunStub is a fake docker CLI that drains stdin (so a streamed
// "docker run -i" never blocks) and records every invocation.
const dockerRunStub = "#!/bin/sh\n" +
	`printf '%s\n' "$*" >> "$STUB_LOG"` + "\n" +
	`if [ "$1" = "run" ]; then cat > /dev/null; fi` + "\n" +
	"exit 0\n"

// TestRestoreProjectRestoresVolumeIntoNewVolume covers the one truly
// special case in a full-stack restore: a volume-strategy service's
// archive is not just extracted to disk like every other kind, it is
// also streamed into a freshly created Docker volume (never an
// existing one — spec principle "never restore over live data").
func TestRestoreProjectRestoresVolumeIntoNewVolume(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	stubDir := t.TempDir()
	resticBin := filepath.Join(stubDir, "restic-stub")
	if err := os.WriteFile(resticBin, []byte(restoreVolumeResticStub), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	h.ops.Repo = func(dest inventory.Destination) (*restic.Repository, error) {
		return restic.Open(dest.Location, restic.Options{
			Runner:       &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+filepath.Join(h.credDir, "restic.log"))},
			PasswordFile: filepath.Join(h.credDir, dest.PasswordCredential),
			ResticBin:    resticBin,
		})
	}

	dockerDir := t.TempDir()
	dockerBin := filepath.Join(dockerDir, "docker")
	dockerLog := filepath.Join(dockerDir, "docker.log")
	if err := os.WriteFile(dockerBin, []byte(dockerRunStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dockerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.ops.Runner = &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+dockerLog)}

	volumeYAML := `
apiVersion: ballast.dev/v1
service: volume-svc
version: 1
backup:
  strategy: volume
  volumes:
    - name: data
verify:
  method: archive-integrity
`
	reg, err := catalog.LoadRegistry(fstest.MapFS{"volume.yaml": &fstest.MapFile{Data: []byte(volumeYAML)}}, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	h.ops.Catalog = reg

	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "volume-svc", CatalogEntry: "volume-svc", Destination: "local", Project: "stack",
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	to := filepath.Join(t.TempDir(), "out")
	rep, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack", To: to})
	if err != nil {
		t.Fatalf("RestoreProject: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("results = %v, want 1", rep.Results)
	}
	res := rep.Results[0]
	if res.Error != "" {
		t.Fatalf("volume-svc result error: %s", res.Error)
	}
	if len(res.Volumes) != 1 {
		t.Fatalf("restored volumes = %v, want 1", res.Volumes)
	}
	if !strings.HasPrefix(res.Volumes[0], "data-restore-") {
		t.Errorf("restored volume name = %q, want a data-restore-<random> name (never the original)", res.Volumes[0])
	}

	log, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("reading docker stub log: %v", err)
	}
	if !strings.Contains(string(log), "run") {
		t.Errorf("docker stub log = %q, want a docker run invocation", log)
	}
}

// TestRestoreProjectWholeStackRebuildsFromOneDate is the spec §17.9
// acceptance criterion for this step: "A whole compose project
// (config + volumes + DB) rebuilds from one date selection." All
// three kinds of service in the same project — compose config, a
// Docker volume, and a DB dump — are restored together from a single
// RestoreProject call carrying one --as-of date, and every one of
// them must come back with no error, in dependency order (compose,
// then volume, then dump), with the volume additionally landing in a
// freshly created Docker volume.
func TestRestoreProjectWholeStackRebuildsFromOneDate(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")

	stubDir := t.TempDir()
	resticBin := filepath.Join(stubDir, "restic-stub")
	if err := os.WriteFile(resticBin, []byte(restoreVolumeResticStub), 0o755); err != nil {
		t.Fatalf("writing restic stub: %v", err)
	}
	h.ops.Repo = func(dest inventory.Destination) (*restic.Repository, error) {
		return restic.Open(dest.Location, restic.Options{
			Runner:       &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+filepath.Join(h.credDir, "restic.log"))},
			PasswordFile: filepath.Join(h.credDir, dest.PasswordCredential),
			ResticBin:    resticBin,
		})
	}

	dockerDir := t.TempDir()
	dockerBin := filepath.Join(dockerDir, "docker")
	dockerLog := filepath.Join(dockerDir, "docker.log")
	if err := os.WriteFile(dockerBin, []byte(dockerRunStub), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dockerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.ops.Runner = &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+dockerLog)}

	composeYAML := `
apiVersion: ballast.dev/v1
service: compose-svc
version: 1
backup:
  strategy: compose
  compose:
    project: demo
verify:
  method: archive-integrity
`
	volumeYAML := `
apiVersion: ballast.dev/v1
service: volume-svc
version: 1
backup:
  strategy: volume
  volumes:
    - name: data
verify:
  method: archive-integrity
`
	dumpYAML := `
apiVersion: ballast.dev/v1
service: dump-svc
version: 1
backup:
  strategy: dump
  dumps:
    - name: dump
      container: db
      command: ["true"]
      format: sql
verify:
  method: archive-integrity
`
	reg, err := catalog.LoadRegistry(fstest.MapFS{
		"compose.yaml": &fstest.MapFile{Data: []byte(composeYAML)},
		"volume.yaml":  &fstest.MapFile{Data: []byte(volumeYAML)},
		"dump.yaml":    &fstest.MapFile{Data: []byte(dumpYAML)},
	}, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	h.ops.Catalog = reg

	// Added out of dependency order on purpose: RestoreProject must
	// still restore them compose -> volume -> dump.
	for _, s := range []inventory.Service{
		{Name: "dump-svc", CatalogEntry: "dump-svc", Destination: "local", Project: "stack"},
		{Name: "volume-svc", CatalogEntry: "volume-svc", Destination: "local", Project: "stack"},
		{Name: "compose-svc", CatalogEntry: "compose-svc", Destination: "local", Project: "stack"},
	} {
		if err := inventory.PutService(h.ops.DB, s); err != nil {
			t.Fatalf("PutService %s: %v", s.Name, err)
		}
	}

	// The stub's one canned snapshot is dated 2024-01-01; asOf a later
	// date still finds it ("at or before"), and one date is shared by
	// every service in the request — there is no per-service date.
	asOf := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	to := filepath.Join(t.TempDir(), "out")
	rep, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack", AsOf: &asOf, To: to})
	if err != nil {
		t.Fatalf("RestoreProject: %v (results: %+v)", err, rep.Results)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("results = %+v, want 3", rep.Results)
	}

	wantOrder := []string{"compose-config", "volume", "dump"}
	for i, r := range rep.Results {
		if r.Error != "" {
			t.Errorf("service %s (%s) failed: %s", r.Service, r.Kind, r.Error)
		}
		if r.Kind != wantOrder[i] {
			t.Errorf("result[%d] kind = %q, want %q (dependency order: config, volumes, DB)", i, r.Kind, wantOrder[i])
		}
		if r.SnapshotID == "" {
			t.Errorf("service %s: SnapshotID is empty", r.Service)
		}
		if _, statErr := os.Stat(r.RestoredTo); statErr != nil {
			t.Errorf("service %s: restored directory %s does not exist: %v", r.Service, r.RestoredTo, statErr)
		}
	}

	volResult := rep.Results[1]
	if len(volResult.Volumes) != 1 || !strings.HasPrefix(volResult.Volumes[0], "data-restore-") {
		t.Errorf("volume-svc restored volumes = %v, want one data-restore-<random> volume", volResult.Volumes)
	}

	log, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("reading docker stub log: %v", err)
	}
	if !strings.Contains(string(log), "run") {
		t.Errorf("docker stub log = %q, want a docker run invocation for the volume restore", log)
	}
}
