package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Alertmanager container reads this material as its own user through a
// read-only bind mount, and on Linux a bind mount keeps the writing user's
// ownership. So every component of the path has to be traversable by a user
// that owns none of it, not only the files: a directory without x for other is
// a server that will not start, and it is invisible on Docker Desktop, where
// the mount is remapped to whichever user the container runs as.
func TestWriteIsReadableByAnotherUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")

	// Created tight first, because MkdirAll leaves an existing directory's mode
	// alone: a checkout that generated this material before would otherwise
	// keep a directory the container cannot traverse for good.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}

	if err := write(dir); err != nil {
		t.Fatalf("write: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if mode := info.Mode().Perm(); mode&0o005 != 0o005 {
		t.Errorf("directory mode is %#o, which another user cannot read and traverse", mode)
	}

	for _, name := range []string{"ca.pem", "server.pem", "server-key.pem"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode&0o004 != 0o004 {
			t.Errorf("%s mode is %#o, which another user cannot read", name, mode)
		}
	}
}
