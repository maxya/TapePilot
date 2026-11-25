package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPlanLiteralNamesAndStability(t *testing.T) {
	root := t.TempDir()
	spool := t.TempDir()
	for _, name := range []string{"-leading", "line\nname", "café"} {
		if e := os.WriteFile(filepath.Join(root, name), []byte("hello"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	p, e := Create(context.Background(), []Root{{"s", root}}, "s", "", spool, nil, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	list, e := os.ReadFile(filepath.Join(p.Directory, "paths.nul"))
	if e != nil {
		t.Fatal(e)
	}
	if string(list) != "-leading\x00café\x00line\nname\x00" || p.Count != 3 || p.LogicalBytes != 15 {
		t.Fatalf("invalid list %q or totals %#v", list, p)
	}
	if e = p.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(root, "-leading"), []byte("changed"), 0600)
	if !errors.Is(p.Validate(context.Background()), ErrChanged) {
		t.Fatal("changed source accepted")
	}
}
func TestRejectUnsafeAndBoundedSources(t *testing.T) {
	root := t.TempDir()
	spool := t.TempDir()
	os.Mkdir(filepath.Join(root, "dir"), 0700)
	os.WriteFile(filepath.Join(root, "dir", "file"), []byte("x"), 0600)
	os.Symlink("dir", filepath.Join(root, "alias"))
	for _, selection := range []string{"../escape", "/absolute", "alias/file", "dir/../dir", "."} {
		if p, e := Create(context.Background(), []Root{{"s", root}}, "s", selection, spool, nil, DefaultLimits()); e == nil {
			p.Close()
			t.Errorf("accepted %q", selection)
		}
	}
	limits := DefaultLimits()
	limits.Entries = 1
	if p, e := Create(context.Background(), []Root{{"s", root}}, "s", "dir", spool, nil, limits); e == nil {
		p.Close()
		t.Fatal("accepted count overflow")
	}
	limits = DefaultLimits()
	limits.DirectoryEntries = 1
	if p, e := Create(context.Background(), []Root{{"s", root}}, "s", "", spool, nil, limits); e == nil {
		p.Close()
		t.Fatal("accepted directory overflow")
	}
	if e := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	if p, e := Create(context.Background(), []Root{{"s", root}}, "s", "fifo", spool, nil, DefaultLimits()); e == nil {
		p.Close()
		t.Fatal("accepted special")
	}
	p, e := Create(context.Background(), []Root{{"s", root}}, "s", "dir/file", spool, nil, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	p.ExpiresAt = time.Now().Add(-time.Second)
	if !errors.Is(p.Validate(context.Background()), ErrChanged) {
		t.Fatal("expired plan accepted")
	}
}

func TestMountBoundariesIncludeBindMounts(t *testing.T) {
	table := "40 1 8:1 / /source rw - ext4 /dev/x rw\n41 40 8:1 /other /source/a\\040b rw - ext4 /dev/x rw\n42 1 8:2 / /elsewhere rw - ext4 /dev/y rw\n"
	mounts, e := parseMountBoundaries(strings.NewReader(table), "/source")
	if e != nil || !mounts["/source/a b"] || len(mounts) != 1 {
		t.Fatalf("%v %v", mounts, e)
	}
}

func TestCleanupCannotBeRedirected(t *testing.T) {
	root := t.TempDir()
	spool := t.TempDir()
	protected := t.TempDir()
	os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600)
	os.WriteFile(filepath.Join(protected, "keep"), []byte("keep"), 0600)
	p, e := Create(context.Background(), []Root{{"s", root}}, "s", "", spool, nil, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	original := p.Directory
	p.Directory = protected
	if e = p.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(protected, "keep")); e != nil {
		t.Fatal("cleanup touched unrelated tree")
	}
	if _, e = os.Stat(original); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("plan not removed")
	}
	if e = (&Plan{Directory: protected}).Close(); e != nil {
		t.Fatal(e)
	}
}
func TestInvalidLinkAndCustomLimits(t *testing.T) {
	root := t.TempDir()
	os.Symlink(string([]byte{0xff}), filepath.Join(root, "bad"))
	if p, e := Create(context.Background(), []Root{{"s", root}}, "s", "", t.TempDir(), nil, DefaultLimits()); e == nil {
		p.Close()
		t.Fatal("invalid UTF-8 link accepted")
	}
	limits := DefaultLimits()
	limits.Entries = 1000001
	if p, e := Create(context.Background(), []Root{{"s", root}}, "s", "", t.TempDir(), nil, limits); e == nil {
		p.Close()
		t.Fatal("scanner bounds exceeded")
	}
}
