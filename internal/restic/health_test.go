package restic

import (
	"context"
	"strings"
	"testing"
)

func TestValidSubset(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"5%", true}, {"1/10", true}, {"500M", true}, {"2g", true},
		{"", false}, {"%", false}, {"abc", false}, {"5%;rm", false}, {"1/", false}, {"--x", false},
	} {
		if err := ValidSubset(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidSubset(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestCheckArgs(t *testing.T) {
	for _, tc := range []struct {
		subset, want string
	}{{"", "check"}, {"5%", "check --read-data-subset=5%"}} {
		stub := newResticStub(t)
		repo := stub.openStub(t)
		if err := repo.Check(context.Background(), tc.subset); err != nil {
			t.Fatalf("Check(%q): %v", tc.subset, err)
		}
		inv := stub.invocations(t)
		if len(inv) != 1 || !strings.HasPrefix(inv[0], tc.want) {
			t.Errorf("Check(%q) ran %v, want %q", tc.subset, inv, tc.want)
		}
	}
}

func TestCheckFailureAndBadSubset(t *testing.T) {
	stub := newResticStub(t)
	repo := stub.withEnv(t, stub.openStub(t), "STUB_FAIL_CMD=check", "STUB_FAIL_CODE=1")
	if err := repo.Check(context.Background(), ""); err == nil {
		t.Error("a failing restic check must return an error")
	}
	if err := stub.openStub(t).Check(context.Background(), "5%;x"); err == nil {
		t.Error("an invalid subset must be refused before running restic")
	}
}

func TestLocksAndUnlock(t *testing.T) {
	stub := newResticStub(t)
	repo := stub.withEnv(t, stub.openStub(t), "STUB_LOCK_IDS=aaa bbb")
	locks, err := repo.Locks(context.Background())
	if err != nil {
		t.Fatalf("Locks: %v", err)
	}
	if len(locks) != 2 || locks[0].Hostname != "nas" || locks[0].PID != 4242 || !locks[0].Exclusive || locks[0].Time.Year() != 2026 {
		t.Fatalf("unexpected locks: %+v", locks)
	}
	if err := repo.Unlock(context.Background()); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	for _, line := range stub.invocations(t) {
		if strings.Contains(line, "--remove-all") {
			t.Errorf("unlock must never use --remove-all: %q", line)
		}
		if strings.HasPrefix(line, "unlock") && strings.Contains(line, "retry-lock") {
			t.Errorf("unlock takes no --retry-lock: %q", line)
		}
	}
}

func TestNoLocks(t *testing.T) {
	stub := newResticStub(t)
	locks, err := stub.openStub(t).Locks(context.Background())
	if err != nil || len(locks) != 0 {
		t.Fatalf("Locks = %v, %v; want none", locks, err)
	}
}
