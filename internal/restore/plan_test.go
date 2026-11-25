package restore

import (
	"archive/tar"
	"context"
	"github.com/maxya/TapePilot/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func member(name string, kind byte, target string) store.Member {
	return store.Member{Path: name, RawPath: name, Type: string(kind), LinkTarget: target, Mode: 0644}
}
func TestUnsafeArchiveGraphCorpus(t *testing.T) {
	for _, test := range []struct {
		name    string
		members []store.Member
	}{
		{"absolute", []store.Member{member("/outside", tar.TypeReg, "")}},
		{"traversal", []store.Member{member("a/../../outside", tar.TypeReg, "")}},
		{"duplicate", []store.Member{member("a", tar.TypeReg, ""), member("a", tar.TypeReg, "")}},
		{"canonical duplicate", []store.Member{member("a", tar.TypeReg, ""), member("./a", tar.TypeReg, "")}},
		{"parent conflict", []store.Member{member("a", tar.TypeReg, ""), member("a/b", tar.TypeReg, "")}},
		{"symlink ancestor", []store.Member{member("a", tar.TypeSymlink, "dir"), member("a/b", tar.TypeReg, "")}},
		{"symlink outside", []store.Member{member("a", tar.TypeSymlink, "../outside")}},
		{"symlink chain escape", []store.Member{member("a", tar.TypeSymlink, "."), member("b", tar.TypeSymlink, "a/../outside")}},
		{"symlink cycle", []store.Member{member("a", tar.TypeSymlink, "b"), member("b", tar.TypeSymlink, "a")}},
		{"hardlink outside", []store.Member{member("a", tar.TypeLink, "../outside")}},
		{"hardlink missing", []store.Member{member("a", tar.TypeLink, "missing")}},
		{"hardlink cycle", []store.Member{member("a", tar.TypeLink, "b"), member("b", tar.TypeLink, "a")}},
		{"special device", []store.Member{member("a", tar.TypeChar, "")}},
		{"fifo", []store.Member{member("a", tar.TypeFifo, "")}},
		{"invalid UTF8", []store.Member{member(string([]byte{255}), tar.TypeReg, "")}},
		{"long name", []store.Member{member(strings.Repeat("x", 4097), tar.TypeReg, "")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := graph(test.members); err == nil {
				t.Fatal("unsafe graph accepted")
			}
		})
	}
}
func TestContainedLinksAndLegacyRoot(t *testing.T) {
	members := []store.Member{member("./", tar.TypeDir, ""), member("./d/", tar.TypeDir, ""), member("./d/file", tar.TypeReg, ""), member("./d/link", tar.TypeSymlink, "../d/file"), member("./copy", tar.TypeLink, "./d/file")}
	if _, _, err := graph(members); err != nil {
		t.Fatal(err)
	}
}
func TestStrictOptionsRejectAliasesAndDuplicates(t *testing.T) {
	for _, raw := range []string{`{"Archive_ID":"x"}`, `{"archive_id":"a","archive_id":"b"}`, `{"source":"/etc"}`, `{} {}`, `null`} {
		if _, err := strictOptions([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}
func TestPostcheckPreservesOrdinaryModesAndStripsPrivilegedBits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	m := member("file", tar.TypeReg, "")
	m.Size = 7
	m.Mode = 04755
	p := Plan{selected: map[string]store.Member{"file": m}, all: map[string]store.Member{"file": m}}
	if err = postcheck(context.Background(), root, p); err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatal("unsafe or changed mode", info.Mode())
	}
}
