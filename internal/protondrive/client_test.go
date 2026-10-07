package protondrive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionFolderAndEnvironment(t *testing.T) {
	c, root := newFake(t)
	if _, _, err := c.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("session folder mode = %v, want 0700", fi.Mode().Perm())
	}
	log, _ := os.ReadFile(filepath.Join(root, "calls.log"))
	want := "unsafe_file|ERROR|" + c.StateDir + "|"
	if !strings.Contains(string(log), want) {
		t.Errorf("CLI environment = %q, want it to contain %q", log, want)
	}
}

func TestSessionFolderTightenedWhenLoose(t *testing.T) {
	c, _ := newFake(t)
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureState(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(c.StateDir)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestVersion(t *testing.T) {
	c, _ := newFake(t)
	v, tested, err := c.Version(context.Background())
	if err != nil || v != "0.8.3" || !tested {
		t.Errorf("Version() = %q, %v, %v; want 0.8.3, true, nil (the update notice must be ignored)", v, tested, err)
	}
}

func TestMissingToolIsNotInstalled(t *testing.T) {
	c, _ := newFake(t)
	c.Bin = filepath.Join(t.TempDir(), "nope")
	_, _, err := c.Version(context.Background())
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
}

func TestSignedOutIsRecognised(t *testing.T) {
	tests := []struct {
		name, text string
		want       bool
	}{
		{"not logged in", "Error: not logged in", true},
		{"expired", "session expired, please sign in", true},
		{"unauthorized", "401 Unauthorized", true},
		{"other", "disk full", false},
		{"update notice only", "A newer version is available, please log in to update", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSignedOut(tc.text); got != tc.want {
				t.Errorf("IsSignedOut(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestListSignedOut(t *testing.T) {
	c, _ := newFake(t)
	t.Setenv("FAKE_SIGNED_OUT", "1")
	_, err := c.List(context.Background(), "/x")
	if !errors.Is(err, ErrSignedOut) {
		t.Errorf("err = %v, want ErrSignedOut", err)
	}
}

func TestParseEntries(t *testing.T) {
	tests := []struct {
		name, in string
		want     int
		wantErr  bool
	}{
		{"array", `[{"name":"a","type":"file","size":1}]`, 1, false},
		{"wrapped", `{"items":[{"name":"a"},{"name":"b"}]}`, 2, false},
		{"empty", `[]`, 0, false},
		{"junk", `nope`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEntries([]byte(tc.in))
			if (err != nil) != tc.wantErr || len(got) != tc.want {
				t.Errorf("parseEntries = %v, %v; want %d entries (err %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestTestConnectionCreatesFolder(t *testing.T) {
	c, root := newFake(t)
	if err := c.TestConnection(context.Background(), "/my-files/ballast"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "drive", "my-files", "ballast")); err != nil {
		t.Errorf("folder was not created: %v", err)
	}
}

func TestTestConnectionSignedOut(t *testing.T) {
	c, _ := newFake(t)
	t.Setenv("FAKE_SIGNED_OUT", "1")
	if err := c.TestConnection(context.Background(), "/f"); !errors.Is(err, ErrSignedOut) {
		t.Errorf("err = %v, want ErrSignedOut", err)
	}
}

func TestFolderFor(t *testing.T) {
	tests := []struct{ in, want string }{{"", "/my-files/ballast"}, {"/a/b/", "/a/b"}, {"/a", "/a"}}
	for _, tc := range tests {
		if got := FolderFor(tc.in); got != tc.want {
			t.Errorf("FolderFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
