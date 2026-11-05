package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/dockerhost"
	keelrunner "github.com/springledev/keel/runner"
)

// TestStartDrillSweep: leftovers are removed and logged, an unreachable
// Docker is logged, and a missing docker binary stays silent.
func TestStartDrillSweep(t *testing.T) {
	const row = "ballast-verify-aaaaaa\tme\t2147483000\t\t2026-11-04 12:00:00 +0000 UTC\tnone"
	tests := []struct {
		name    string
		script  string
		wantLog string
	}{
		{"removes a leftover", "[ \"$1\" = ps ] && printf '%s\\n' '" + row + "'; exit 0", "removed test container ballast-verify-aaaaaa"},
		{"reports an unreachable Docker", "echo down >&2; exit 1", "checking for leftover test containers"},
		{"no docker installed", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "docker")
			if tt.script != "" {
				if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tt.script+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var logw strings.Builder
			a := &app{store: st, logw: &logw, ops: &api.Ops{
				Runner:      &keelrunner.Runner{},
				DrillOwner:  "me",
				DockerHosts: dockerhost.Resolver{DockerBin: bin},
			}}
			select {
			case <-a.startDrillSweep(context.Background()):
			case <-time.After(30 * time.Second):
				t.Fatal("the sweep did not finish")
			}
			got := logw.String()
			if tt.wantLog == "" && got != "" {
				t.Errorf("log = %q, want nothing", got)
			}
			if !strings.Contains(got, tt.wantLog) {
				t.Errorf("log = %q, want it to contain %q", got, tt.wantLog)
			}
		})
	}
}
