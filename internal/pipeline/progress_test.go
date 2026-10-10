package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/progress"
)

// TestRunPublishesUploadProgress: while restic uploads, the tracker
// holds the latest numbers for that destination; the run still passes
// VERIFYING to SUCCESS and the tracker is empty afterwards.
func TestRunPublishesUploadProgress(t *testing.T) {
	h := newHarness(t)
	wrapper := writeStub(t, "restic-progress", `#!/bin/sh
if [ "$1" = backup ]; then
  echo '{"message_type":"status","percent_done":0.5,"total_bytes":200,"bytes_done":100,"seconds_remaining":60}'
  sleep 2
fi
exec `+h.resticBin+` "$@"
`)
	h.resticBin = wrapper
	p := h.pipeline()
	p.Progress = &progress.Tracker{}
	libDir := t.TempDir()
	p.Catalog = registryWith(t, map[string]string{"photos.yaml": yamlf(dumpAndFolderYAML, libDir)})
	if err := inventory.PutService(h.db, inventory.Service{Name: "photos", CatalogEntry: "photos", Destination: "local"}); err != nil {
		t.Fatal(err)
	}
	var (
		mu  sync.Mutex
		saw []progress.Upload
	)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if ups := p.Progress.ForService("photos"); len(ups) > 0 {
				mu.Lock()
				saw = ups
				mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	state, err := p.Run(context.Background(), "photos")
	close(done)
	if state != engine.SUCCESS {
		t.Fatalf("state = %s (%v), want SUCCESS", state, err)
	}
	runs, _ := engine.RecentRuns(h.db, "photos", 1)
	if tr := transitionsOf(t, h.db, runs[0].ID); !containsInOrder(tr, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions %v do not pass VERIFYING before SUCCESS", tr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(saw) != 1 || saw[0].BytesDone != 100 || saw[0].TotalBytes != 200 || saw[0].Percent != 50 {
		t.Errorf("progress seen during upload = %+v, want 100 of 200 (50%%)", saw)
	}
	if left := p.Progress.ForService("photos"); len(left) != 0 {
		t.Errorf("progress left after the run = %+v, want none", left)
	}
}
