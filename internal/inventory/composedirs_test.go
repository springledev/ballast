package inventory

import (
	"errors"
	"testing"

	"github.com/springledev/keel/dockerhost"
)

func TestComposeDirs(t *testing.T) {
	db := openDB(t).DB()
	tests := []struct {
		name    string
		dir     ComposeDir
		wantErr bool
	}{
		{"project ok", ComposeDir{DockerHost: dockerhost.LocalName, Path: "/srv/blog/", Kind: ComposeDirProject}, false},
		{"scan ok", ComposeDir{DockerHost: dockerhost.LocalName, Path: "/srv/stacks", Kind: ComposeDirScan}, false},
		{"relative path", ComposeDir{DockerHost: dockerhost.LocalName, Path: "srv", Kind: ComposeDirScan}, true},
		{"bad kind", ComposeDir{DockerHost: dockerhost.LocalName, Path: "/srv", Kind: "x"}, true},
		{"unknown host", ComposeDir{DockerHost: "nope", Path: "/srv", Kind: ComposeDirScan}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PutComposeDir(db, tt.dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("PutComposeDir err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	list, err := ListComposeDirs(db, dockerhost.LocalName)
	if err != nil || len(list) != 2 || list[0].Path != "/srv/blog" {
		t.Fatalf("ListComposeDirs = %+v, %v; want 2 folders, first /srv/blog (cleaned)", list, err)
	}
	if err := DeleteComposeDir(db, dockerhost.LocalName, "/srv/blog"); err != nil {
		t.Fatalf("DeleteComposeDir: %v", err)
	}
	if err := DeleteComposeDir(db, dockerhost.LocalName, "/srv/blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}
