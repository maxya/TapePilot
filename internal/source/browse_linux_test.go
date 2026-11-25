package source

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBrowseRestrictsDirectoryResolution(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if e := os.Mkdir(filepath.Join(root, "allowed"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(root, "redirect")); e != nil {
		t.Fatal(e)
	}
	rows, e := Browse(root, ".")
	if e != nil || len(rows) != 2 || !rows[0].Directory || rows[1].Directory {
		t.Fatal(rows, e)
	}
	for _, name := range []string{"redirect", "../outside", "allowed/../redirect"} {
		if _, e := Browse(root, name); !errors.Is(e, ErrUnsafe) {
			t.Fatal(name, e)
		}
	}
}
