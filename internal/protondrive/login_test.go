package protondrive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoginWriteExtractsLinkAndCode(t *testing.T) {
	tests := []struct {
		name, out, link, code string
	}{
		{"link and code", "Open https://account.proton.me/device?c=1 and enter code AB12-CD34\n", "https://account.proton.me/device?c=1", "AB12-CD34"},
		{"link only", "Visit https://account.proton.me/login.\n", "https://account.proton.me/login", ""},
		{"nothing yet", "starting...\n", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &Login{}
			// Written in two chunks, like a real stream.
			half := len(tc.out) / 2
			_, _ = l.Write([]byte(tc.out[:half]))
			_, _ = l.Write([]byte(tc.out[half:]))
			l.finish(nil)
			if i := l.Info(); i.Link != tc.link || i.Code != tc.code {
				t.Errorf("Info() = %+v, want link %q code %q", i, tc.link, tc.code)
			}
		})
	}
}

func TestStartLoginCompletesAndProvesConnection(t *testing.T) {
	c, root := newFake(t)
	l, err := c.StartLogin("/my-files/ballast")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var info LoginInfo
	for time.Now().Before(deadline) {
		if info = l.Info(); info.Done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !info.Done || info.Error != "" {
		t.Fatalf("login did not finish cleanly: %+v", info)
	}
	if info.Link != "https://account.proton.me/device?c=1" || info.Code != "AB12-CD34" {
		t.Errorf("link/code = %q / %q", info.Link, info.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "drive", "my-files", "ballast")); err != nil {
		t.Errorf("folder not created after sign-in: %v", err)
	}
}

func TestStartLoginMissingTool(t *testing.T) {
	c, _ := newFake(t)
	c.Bin = filepath.Join(t.TempDir(), "nope")
	l, err := c.StartLogin("/f")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && !l.Info().Done; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if info := l.Info(); !info.Done || info.Error == "" {
		t.Errorf("want a finished login with an install hint, got %+v", info)
	}
}

func TestLoginDoesNotTakeAHalfWrittenLink(t *testing.T) {
	l := &Login{}
	_, _ = l.Write([]byte("Open https://account.proton.me/de"))
	if got := l.Info().Link; got != "" {
		t.Errorf("Link = %q while the line was still arriving, want none", got)
	}
	_, _ = l.Write([]byte("vice now\n"))
	if got := l.Info().Link; got != "https://account.proton.me/device" {
		t.Errorf("Link = %q once the line was complete", got)
	}
}
