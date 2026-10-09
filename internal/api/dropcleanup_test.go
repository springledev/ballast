package api

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
)

// fakeDropRepo is an in-memory repository: the raw size of a set of
// snapshots is the sum of their sizes.
type fakeDropRepo struct {
	snaps     []restic.Snapshot
	size      map[string]uint64
	forgotten []string
	pruned    int
	forgetErr error
}

func (f *fakeDropRepo) Snapshots(context.Context, restic.SnapshotFilter) ([]restic.Snapshot, error) {
	return f.snaps, nil
}

func (f *fakeDropRepo) RawDataSize(_ context.Context, ids []string) (uint64, error) {
	var n uint64
	for _, id := range ids {
		n += f.size[id]
	}
	return n, nil
}

func (f *fakeDropRepo) ForgetSnapshots(_ context.Context, ids []string) error {
	f.forgotten = append(f.forgotten, ids...)
	return f.forgetErr
}

func (f *fakeDropRepo) Prune(context.Context) error { f.pruned++; return nil }

func dropEntry() *catalog.Entry {
	e := &catalog.Entry{}
	e.Backup.Files = []catalog.FileSpec{{Name: "library", Path: "/data/library"}, {Name: "files", Path: "/data/config"}}
	e.Backup.Dumps = []catalog.Dump{{Name: "database", Format: "sql"}}
	return e
}

func dropSnap(id string, day int, paths ...string) restic.Snapshot {
	return restic.Snapshot{ID: id, Time: time.Date(2026, 10, day, 3, 0, 0, 0, time.UTC), Paths: paths}
}

func TestDroppedParts(t *testing.T) {
	all := []string{"database", "library"}
	tests := []struct {
		name     string
		old, now inventory.Target
		want     []string
	}{
		{"all to library only", inventory.Target{}, inventory.Target{Parts: []string{"library"}}, []string{"database"}},
		{"library removed", inventory.Target{Parts: []string{"database", "library"}}, inventory.Target{Parts: []string{"database"}}, []string{"library"}},
		{"parts added", inventory.Target{Parts: []string{"library"}}, inventory.Target{}, nil},
		{"unchanged", inventory.Target{}, inventory.Target{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DroppedParts(tt.old, tt.now, all); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPartsInSnapshot(t *testing.T) {
	got := partsInSnapshot(dropEntry(), []string{"/data/library", "/work/run1/database.sql", "/other"})
	if !got["library"] || !got["database"] || got["files"] || len(got) != 2 {
		t.Errorf("parts = %v, want library and database only", got)
	}
}

func TestPlanDrop(t *testing.T) {
	ctx := context.Background()
	withLib := func(id string, day int) restic.Snapshot {
		return dropSnap(id, day, "/data/library", "/data/config", "/work/database.sql")
	}
	noLib := func(id string, day int) restic.Snapshot { return dropSnap(id, day, "/data/config", "/work/database.sql") }
	sizes := map[string]uint64{"old1": 80, "old2": 5, "new1": 10}
	tests := []struct {
		name        string
		snaps       []restic.Snapshot
		target      inventory.Target
		parts       []string
		wantErr     string
		wantIDs     []string
		wantFreed   uint64
		wantBlocked bool
	}{
		{name: "old ones only", snaps: []restic.Snapshot{withLib("old2", 2), withLib("old1", 1), noLib("new1", 3)},
			target: inventory.Target{Parts: []string{"database"}}, parts: []string{"library"}, wantIDs: []string{"old1", "old2"}, wantFreed: 85},
		{name: "still wanted", snaps: []restic.Snapshot{withLib("old1", 1)}, target: inventory.Target{}, parts: []string{"library"}, wantErr: "still receives"},
		{name: "no parts", snaps: nil, target: inventory.Target{}, parts: nil, wantErr: "at least one part"},
		{name: "nothing newer", snaps: []restic.Snapshot{withLib("old1", 1)}, target: inventory.Target{Parts: []string{"database"}}, parts: []string{"library"}, wantBlocked: true},
		{name: "nothing holds it", snaps: []restic.Snapshot{noLib("new1", 3)}, target: inventory.Target{Parts: []string{"database"}}, parts: []string{"library"}, wantBlocked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeDropRepo{snaps: tt.snaps, size: sizes}
			plan, err := planDrop(ctx, repo, "immich", "local", tt.target, dropEntry(), tt.parts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, ErrValidation) {
					t.Fatalf("err = %v, want validation error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (plan.Blocked != "") != tt.wantBlocked {
				t.Fatalf("blocked = %q", plan.Blocked)
			}
			var ids []string
			for _, s := range plan.Snapshots {
				ids = append(ids, s.ID)
			}
			if !tt.wantBlocked && (!reflect.DeepEqual(ids, tt.wantIDs) || plan.FreedBytes != tt.wantFreed) {
				t.Errorf("ids = %v freed = %d, want %v / %d", ids, plan.FreedBytes, tt.wantIDs, tt.wantFreed)
			}
			if len(repo.forgotten) != 0 || repo.pruned != 0 {
				t.Error("planning must not delete anything")
			}
		})
	}
}

// TestDropPartsForgetsExactlyThePlannedSnapshots: only snapshots from the
// plan are forgotten, then the repository is pruned; an unplanned ID is
// refused before anything is deleted.
func TestDropPartsForgetsExactlyThePlannedSnapshots(t *testing.T) {
	ctx := context.Background()
	newRepo := func() *fakeDropRepo {
		return &fakeDropRepo{
			snaps: []restic.Snapshot{
				dropSnap("old1", 1, "/data/library", "/work/database.sql"),
				dropSnap("new1", 3, "/work/database.sql"),
			},
			size: map[string]uint64{"old1": 50, "new1": 1},
		}
	}
	target := inventory.Target{Parts: []string{"database"}}
	repo := newRepo()
	if _, err := dropParts(ctx, repo, "immich", "local", target, dropEntry(), []string{"library"}, []string{"old1"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repo.forgotten, []string{"old1"}) || repo.pruned != 1 {
		t.Errorf("forgotten = %v pruned = %d", repo.forgotten, repo.pruned)
	}
	for _, ids := range [][]string{{"new1"}, {"old1", "new1"}, {"zzzz"}, nil} {
		repo := newRepo()
		if _, err := dropParts(ctx, repo, "immich", "local", target, dropEntry(), []string{"library"}, ids); err == nil {
			t.Errorf("ids %v should be refused", ids)
		}
		if len(repo.forgotten) != 0 || repo.pruned != 0 {
			t.Errorf("ids %v: refused call deleted something", ids)
		}
	}
}
