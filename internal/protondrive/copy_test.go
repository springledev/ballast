package protondrive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) []File {
	t.Helper()
	dir := t.TempDir()
	var out []File
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, File{Name: name, Path: p})
	}
	return out
}

func req(t *testing.T, files []File, at time.Time) Request {
	return Request{Service: "vault", RunTime: at, Folder: "/my-files/ballast", Files: files, KeepLast: 7, WorkDir: t.TempDir()}
}

var t0 = time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

func TestCopyVerified(t *testing.T) {
	c, root := newFake(t)
	files := writeFiles(t, map[string]string{"db.sql.gz": "dump", "conf.tar": "config"})
	res := c.Copy(context.Background(), req(t, files, t0))
	if !res.Verified || res.SignedOut || res.ReadBack == "" || len(res.Uploaded) != 2 {
		t.Fatalf("Copy = %+v, want verified with 2 uploads and a read-back", res)
	}
	got, err := os.ReadFile(filepath.Join(root, "drive", "my-files", "ballast", "vault", "2026-03-01_020000", "db.sql.gz"))
	if err != nil || string(got) != "dump" {
		t.Errorf("uploaded file = %q, %v", got, err)
	}
}

func TestCopyReadBackRotates(t *testing.T) {
	c, _ := newFake(t)
	files := writeFiles(t, map[string]string{"a": "1", "b": "2", "c": "3"})
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		res := c.Copy(context.Background(), req(t, files, t0.Add(time.Duration(i)*time.Minute)))
		seen[res.ReadBack] = true
	}
	if len(seen) < 2 {
		t.Errorf("read-back always checked %v; it should rotate", seen)
	}
}

func TestCopyProblemsAreAmberNeverVerified(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantIn  string
		signed  bool
		verdict bool
	}{
		{"signed out", map[string]string{"FAKE_SIGNED_OUT": "1"}, "sign in again", true, false},
		{"size mismatch", map[string]string{"FAKE_SHORT": "1"}, "cannot be trusted", false, false},
		{"corrupt download", map[string]string{"FAKE_CORRUPT": "1"}, "does not match", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFake(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			res := c.Copy(context.Background(), req(t, writeFiles(t, map[string]string{"a.bin": "payload"}), t0))
			if res.Verified != tc.verdict || res.SignedOut != tc.signed || !strings.Contains(res.Detail, tc.wantIn) {
				t.Errorf("Copy = %+v, want verified=%v signedOut=%v detail containing %q", res, tc.verdict, tc.signed, tc.wantIn)
			}
		})
	}
}

func TestCopySizeCapSkipsAndLists(t *testing.T) {
	c, root := newFake(t)
	files := writeFiles(t, map[string]string{"small": "x", "big": strings.Repeat("y", 5000)})
	r := req(t, files, t0)
	r.MaxBytes = 100
	res := c.Copy(context.Background(), r)
	if !res.Verified || len(res.Uploaded) != 1 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "big") || !strings.Contains(res.Detail, "Not copied") {
		t.Fatalf("Copy = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "drive", "my-files", "ballast", "vault", "2026-03-01_020000", "big")); err == nil {
		t.Error("the oversized file was uploaded anyway")
	}
}

func TestCopyNothingToSendIsNotVerified(t *testing.T) {
	c, _ := newFake(t)
	r := req(t, writeFiles(t, map[string]string{"big": strings.Repeat("y", 500)}), t0)
	r.MaxBytes = 10
	r.SkippedFolders = []string{"library"}
	res := c.Copy(context.Background(), r)
	if res.Verified || !strings.Contains(res.Detail, "Nothing was copied") || !strings.Contains(res.Detail, "library") {
		t.Errorf("Copy = %+v", res)
	}
}

func TestCopyKeepLastRemovesOnlyBallastRunFolders(t *testing.T) {
	c, root := newFake(t)
	svc := filepath.Join(root, "drive", "my-files", "ballast", "vault")
	for _, d := range []string{"2026-01-01_000000", "2026-01-02_000000", "2026-01-03_000000", "my-own-notes", "2026-01-04"} {
		if err := os.MkdirAll(filepath.Join(svc, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(svc, "readme.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := req(t, writeFiles(t, map[string]string{"a": "1"}), t0)
	r.KeepLast = 2
	if res := c.Copy(context.Background(), r); !res.Verified || strings.Contains(res.Detail, "could not") {
		t.Fatalf("Copy = %+v", res)
	}
	have := map[string]bool{}
	ents, _ := os.ReadDir(svc)
	for _, e := range ents {
		have[e.Name()] = true
	}
	for _, want := range []string{"2026-03-01_020000", "2026-01-03_000000", "my-own-notes", "2026-01-04", "readme.txt"} {
		if !have[want] {
			t.Errorf("%s was removed but must stay; have %v", want, have)
		}
	}
	for _, gone := range []string{"2026-01-01_000000", "2026-01-02_000000"} {
		if have[gone] {
			t.Errorf("%s should have been removed", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "drive", ".trash")); err != nil {
		t.Error("old folders must be trashed before they are deleted")
	}
}

func TestCopyRejectsBadNames(t *testing.T) {
	c, _ := newFake(t)
	tests := []struct {
		name  string
		files []File
	}{
		{"path in name", []File{{Name: "../x", Path: "/etc/hostname"}}},
		{"duplicate", []File{{Name: "a", Path: "/etc/hostname"}, {Name: "a", Path: "/etc/hostname"}}},
		{"missing file", []File{{Name: "a", Path: "/nonexistent/file"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if res := c.Copy(context.Background(), req(t, tc.files, t0)); res.Verified || res.Detail == "" {
				t.Errorf("Copy = %+v, want an unverified result with a reason", res)
			}
		})
	}
}

func TestLockSerialisesAndHonoursContext(t *testing.T) {
	c, _ := newFake(t)
	release, err := c.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Lock(ctx); err == nil {
		t.Error("a second lock on the same session folder must wait, then give up when its context ends")
	}
}
