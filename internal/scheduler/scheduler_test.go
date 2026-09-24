package scheduler

import (
	"context"
	"testing"

	"github.com/springledev/ballast/internal/engine"
)

func TestRunnerFuncAdapter(t *testing.T) {
	var got string
	f := RunnerFunc(func(_ context.Context, service string) (engine.State, error) {
		got = service
		return engine.SUCCESS, nil
	})
	state, err := f.Run(context.Background(), "immich")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Errorf("state = %v, want SUCCESS", state)
	}
	if got != "immich" {
		t.Errorf("service = %q, want %q", got, "immich")
	}
}
