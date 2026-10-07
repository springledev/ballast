package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
)

func TestBuildPreview(t *testing.T) {
	entry := &catalog.Entry{Service: "photos", Backup: catalog.Backup{
		Dumps:     []catalog.Dump{{Name: "db"}},
		Files:     []catalog.FileSpec{{Path: "/photos", Name: "library", Exclude: []string{"*.tmp"}}, {Path: "/cfg"}},
		StopStart: &catalog.StopStart{Containers: []string{"web", "worker"}},
	}}
	dests := map[string]inventory.Destination{
		"ssd":    {Name: "ssd", Type: backend.Local, KeepDaily: 7},
		"b2":     {Name: "b2", Type: backend.B2, KeepLast: 3},
		"proton": {Name: "proton", Type: backend.ProtonDrive, KeepLast: 5},
	}
	tests := []struct {
		name      string
		targets   []inventory.Target
		wantGoes  map[string][]string
		wantParts map[string][]string
		secondary string
	}{
		{
			name:      "everything everywhere",
			targets:   []inventory.Target{{Destination: "ssd"}, {Destination: "b2"}},
			wantGoes:  map[string][]string{"db": {"ssd", "b2"}, "library": {"ssd", "b2"}, "files": {"ssd", "b2"}},
			wantParts: map[string][]string{"ssd": {"db", "library", "files"}, "b2": {"db", "library", "files"}},
		},
		{
			name:      "library to b2 only, small parts to the second copy",
			targets:   []inventory.Target{{Destination: "ssd", Parts: []string{"db", "files"}}, {Destination: "b2"}, {Destination: "proton", Parts: []string{"db", "files"}}},
			wantGoes:  map[string][]string{"db": {"ssd", "b2", "proton"}, "library": {"b2"}, "files": {"ssd", "b2", "proton"}},
			wantParts: map[string][]string{"ssd": {"db", "files"}, "b2": {"db", "library", "files"}, "proton": {"db", "files"}},
			secondary: "proton",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := buildPreview(inventory.Service{Name: "photos"}, entry, tt.targets, dests)
			for _, p := range pv.Parts {
				if !reflect.DeepEqual(p.Goes, tt.wantGoes[p.Name]) {
					t.Errorf("part %s goes to %v, want %v", p.Name, p.Goes, tt.wantGoes[p.Name])
				}
			}
			for _, tg := range pv.Targets {
				if !reflect.DeepEqual(tg.Parts, tt.wantParts[tg.Destination]) {
					t.Errorf("target %s parts %v, want %v", tg.Destination, tg.Parts, tt.wantParts[tg.Destination])
				}
				if tg.Secondary != (tg.Destination == tt.secondary) {
					t.Errorf("target %s secondary = %v", tg.Destination, tg.Secondary)
				}
				if tg.Secondary && !strings.Contains(tg.Note, "1024 MB") {
					t.Errorf("second copy note %q should state the size limit", tg.Note)
				}
			}
			if !reflect.DeepEqual(pv.StopContainers, []string{"web", "worker"}) {
				t.Errorf("stop containers = %v", pv.StopContainers)
			}
			if !reflect.DeepEqual(pv.Excludes, []string{"*.tmp"}) {
				t.Errorf("excludes = %v", pv.Excludes)
			}
		})
	}
}

func TestKeepsText(t *testing.T) {
	tests := []struct {
		r    inventory.Retention
		want string
	}{
		{inventory.Retention{KeepDaily: 7, KeepWeekly: 4}, "7 daily, 4 weekly"},
		{inventory.Retention{KeepLast: 3}, "the last 3"},
		{inventory.Retention{}, "everything"},
	}
	for _, tt := range tests {
		if got := keepsText(tt.r); got != tt.want {
			t.Errorf("keepsText(%+v) = %q, want %q", tt.r, got, tt.want)
		}
	}
}

// TestOpsPreview: a preview estimates local folders, reports nothing as
// run, and rejects unknown services.
func TestOpsPreview(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	dir := t.TempDir()
	for name, body := range map[string]string{"a.txt": "aaaa", "b.log": "bbbbbbbb"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := inventory.Service{Name: "docs", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": dir}, Excludes: []string{"*.log"}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	pv, err := h.ops.Preview(context.Background(), "docs")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if pv.EstimatedBytes == nil || *pv.EstimatedBytes != 4 {
		t.Errorf("estimated = %v, want 4 bytes (the .log is skipped)", pv.EstimatedBytes)
	}
	if len(pv.Targets) != 1 || pv.Targets[0].Destination != "local" || len(pv.Parts) == 0 {
		t.Errorf("preview = %+v", pv)
	}
	if _, err := h.ops.Preview(context.Background(), "ghost"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("unknown service error = %v, want ErrNotFound", err)
	}
}

// TestOpsPreviewRunsNothing: a preview never starts a run, so nothing
// can be marked SUCCESS (or anything else) by it.
func TestOpsPreviewRunsNothing(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "docs", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": t.TempDir()}}
	if err := h.ops.PutService(context.Background(), svc); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if _, err := h.ops.Preview(context.Background(), "docs"); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	var runs int
	if err := h.ops.DB.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("preview created %d run(s), want 0", runs)
	}
}
