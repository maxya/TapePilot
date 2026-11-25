// Package legacy opens only explicitly configured read-only simulator fixtures.
package legacy

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/maxya/TapePilot/internal/media"
	"golang.org/x/sys/unix"
)

const SimulatorProfile = "simulator-plain-tar-v1"

var ErrPolicy = errors.New("LEGACY_PROFILE_REQUIRED")
var ErrIdentity = errors.New("LEGACY_SOURCE_CHANGED")
var sourceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Root struct{ ID, Path string }
type Binding struct {
	SourceID     string `json:"source_id"`
	Profile      string `json:"profile"`
	RootIdentity string `json:"root_identity"`
}

// Identify requires explicit profile and operator acknowledgment. The resulting
// immutable binding can subsequently open only this configured fixture root.
func Identify(ctx context.Context, roots []Root, id, profile string, confirmed bool) (Binding, error) {
	if e := ctx.Err(); e != nil {
		return Binding{}, e
	}
	if !confirmed || profile != SimulatorProfile || !sourceID.MatchString(id) {
		return Binding{}, ErrPolicy
	}
	root, e := openRoot(roots, id)
	if e != nil {
		return Binding{}, e
	}
	defer root.Close()
	info, e := root.Stat()
	if e != nil {
		return Binding{}, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Binding{}, ErrIdentity
	}
	return Binding{id, profile, fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)}, nil
}
func openRoot(roots []Root, id string) (*os.File, error) {
	var selected string
	for _, root := range roots {
		if root.ID == id {
			if selected != "" {
				return nil, ErrPolicy
			}
			selected = root.Path
		}
	}
	if !filepath.IsAbs(selected) || filepath.Clean(selected) != selected {
		return nil, ErrPolicy
	}
	resolved, e := filepath.EvalSymlinks(selected)
	if e != nil {
		return nil, e
	}
	if resolved != selected {
		return nil, ErrPolicy
	}
	file, e := os.OpenFile(selected, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	return file, nil
}

// Open returns one numbered ordinary file through a pinned directory handle.
// An absent file is EOD only if the caller establishes a contiguous layout.
func Open(ctx context.Context, roots []Root, binding Binding, index int64) (io.ReadCloser, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if index < 0 || index >= 100000 || binding.Profile != SimulatorProfile || !sourceID.MatchString(binding.SourceID) {
		return nil, ErrPolicy
	}
	root, e := openRoot(roots, binding.SourceID)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	info, e := root.Stat()
	if e != nil {
		return nil, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || binding.RootIdentity != fmt.Sprintf("%d:%d", stat.Dev, stat.Ino) {
		return nil, ErrIdentity
	}
	name := fmt.Sprintf("%06d.tar", index)
	fd, e := unix.Openat(int(root.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	pinned := os.NewFile(uintptr(fd), name)
	defer pinned.Close()
	info, e = pinned.Stat()
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() < 1024 || info.Size()%512 != 0 {
		return nil, media.ErrFormat
	}
	file, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		return nil, e
	}
	return &contextReader{ctx: ctx, file: file, initial: info}, nil
}

type contextReader struct {
	ctx     context.Context
	file    *os.File
	initial os.FileInfo
}

func (r *contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.file.Read(b)
}
func (r *contextReader) Close() error {
	info, e := r.file.Stat()
	if e == nil && (!os.SameFile(info, r.initial) || info.Size() != r.initial.Size() || !info.ModTime().Equal(r.initial.ModTime())) {
		e = ErrIdentity
	}
	if e == nil {
		before, beforeOK := r.initial.Sys().(*syscall.Stat_t)
		after, afterOK := info.Sys().(*syscall.Stat_t)
		if !beforeOK || !afterOK || before.Ctim != after.Ctim || before.Mode != after.Mode {
			e = ErrIdentity
		}
	}
	return errors.Join(e, r.file.Close())
}
func Scan(r io.Reader, visit func(*tar.Header) error) (media.Digest, error) {
	return media.ScanLegacy(r, visit)
}

// Count establishes a contiguous explicitly numbered layout and rejects unknown
// directory contents. Root replacement is detected against the import binding.
func Count(ctx context.Context, roots []Root, binding Binding) (int64, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	actual, e := Identify(ctx, roots, binding.SourceID, binding.Profile, true)
	if e != nil {
		return 0, e
	}
	if actual != binding {
		return 0, ErrIdentity
	}
	root, e := openRoot(roots, binding.SourceID)
	if e != nil {
		return 0, e
	}
	defer root.Close()
	names, e := root.Readdirnames(100001)
	if e != nil && e != io.EOF {
		return 0, e
	}
	if len(names) == 0 || len(names) > 100000 {
		return 0, ErrPolicy
	}
	found := make(map[string]bool, len(names))
	for _, name := range names {
		found[name] = true
	}
	for i := 0; i < len(names); i++ {
		if !found[fmt.Sprintf("%06d.tar", i)] {
			return 0, ErrPolicy
		}
	}
	return int64(len(names)), nil
}
