// Package source creates bounded, private plans from administrator-owned roots.
package source

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

var ErrUnsafe = errors.New("UNSAFE_SOURCE")
var ErrChanged = errors.New("SOURCE_CHANGED")
var ErrLimit = errors.New("PLAN_LIMIT")

type Root struct{ ID, Path string }
type Limits struct {
	Entries, Depth, PathBytes, DirectoryEntries int
	TTL                                         time.Duration
}

func DefaultLimits() Limits { return Limits{1000000, 128, 4096, 10000, 30 * time.Minute} }

type Entry struct {
	Name          string
	Size, Mtime   int64
	Mode          uint32
	Device, Inode uint64
	Link          string
	Sparse        bool
}
type Plan struct {
	SourceID, Selection, Directory      string
	Count, LogicalBytes, EstimatedBytes int64
	SparseFiles                         int64
	ExpiresAt                           time.Time
	root                                string
	rootEntry                           Entry
	limits                              Limits
	privateDirectory                    string
	cleanup                             *os.Root
	cleanupName                         string
	closeOnce                           sync.Once
	closeError                          error
}

// Create only accepts a configured root ID and a relative subtree. The root and
// its owners must be trusted and stable throughout planning and streaming.
func Create(ctx context.Context, roots []Root, id, selection, spool string, exclusions []string, limits Limits) (p *Plan, err error) {
	if limits.Entries > 1000000 || limits.Entries <= 0 || limits.Depth > 128 || limits.Depth <= 0 || limits.PathBytes > 4096 || limits.PathBytes <= 0 || limits.DirectoryEntries > 10000 || limits.DirectoryEntries <= 0 || limits.TTL <= 0 {
		return nil, ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var root string
	for _, r := range roots {
		if r.ID == id {
			if root != "" {
				return nil, ErrUnsafe
			}
			root = r.Path
		}
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrUnsafe
	}
	resolved, e := filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	if resolved != root {
		return nil, ErrUnsafe
	}
	if selection != "" && !safeName(selection) {
		return nil, ErrUnsafe
	}
	for _, x := range exclusions {
		if !safeName(x) {
			return nil, ErrUnsafe
		}
	}
	if !filepath.IsAbs(spool) {
		return nil, ErrUnsafe
	}
	resolvedSpool, e := filepath.EvalSymlinks(spool)
	if e != nil {
		return nil, e
	}
	cleanup, e := os.OpenRoot(resolvedSpool)
	if e != nil {
		return nil, e
	}
	dir, e := os.MkdirTemp(resolvedSpool, "plan-")
	if e != nil {
		cleanup.Close()
		return nil, e
	}
	p = &Plan{privateDirectory: dir, cleanup: cleanup, cleanupName: filepath.Base(dir), SourceID: id, Selection: selection, Directory: dir, root: root, limits: limits, ExpiresAt: time.Now().UTC().Add(limits.TTL)}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	mounts, e := mountBoundaries(root)
	if e != nil {
		return nil, e
	}
	handle, e := os.OpenRoot(root)
	if e != nil {
		return nil, e
	}
	defer handle.Close()
	p.rootEntry, e = observe(handle, ".")
	if e != nil {
		return nil, e
	}
	list, e := os.OpenFile(filepath.Join(dir, "paths.nul"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	defer list.Close()
	rows, e := os.OpenFile(filepath.Join(dir, "entries.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for _, x := range exclusions {
		if target := selection; target != "" && (target == x || strings.HasPrefix(target, x+"/")) {
			return nil, ErrUnsafe
		}
	}
	enc := json.NewEncoder(rows)
	var nameBytes int64
	emit := func(name string, entry Entry) error {
		if len(entry.Link) > limits.PathBytes {
			return ErrLimit
		}
		nameBytes += int64(len(entry.Name) + len(entry.Link))
		if nameBytes > 128*1024*1024 {
			return ErrLimit
		}
		p.Count++
		if p.Count > int64(limits.Entries) {
			return ErrLimit
		}
		size := int64(0)
		if os.FileMode(entry.Mode).IsRegular() {
			size = entry.Size
		}
		if size < 0 || size > math.MaxInt64-262144 || p.EstimatedBytes > math.MaxInt64-size-262144 || p.LogicalBytes > math.MaxInt64-size {
			return ErrLimit
		}
		if entry.Sparse {
			p.SparseFiles++
		}
		p.LogicalBytes += size
		// Each member reserves one whole record for bounded PAX/name overhead.
		p.EstimatedBytes += size + 262144
		if _, e := list.WriteString(name + "\x00"); e != nil {
			return e
		}
		return enc.Encode(entry)
	}
	var walk func(string, int) error
	walk = func(name string, depth int) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if depth > limits.Depth || len(name) > limits.PathBytes {
			return ErrLimit
		}
		for _, x := range exclusions {
			if name == x || strings.HasPrefix(name, x+"/") {
				return nil
			}
		}
		if mounts[filepath.Join(root, name)] {
			return ErrUnsafe
		}
		entry, e := observe(handle, name)
		if e != nil {
			return e
		}
		if entry.Device != p.rootEntry.Device {
			return ErrUnsafe
		}
		mode := os.FileMode(entry.Mode)
		if !mode.IsRegular() && !mode.IsDir() && mode&os.ModeSymlink == 0 {
			return ErrUnsafe
		}
		if mode.IsRegular() {
			file, e := handle.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if e != nil {
				return e
			}
			if e = file.Close(); e != nil {
				return e
			}
		}
		if name != "." {
			if e = emit(name, entry); e != nil {
				return e
			}
		}
		if !mode.IsDir() {
			return nil
		}
		f, e := handle.Open(name)
		if e != nil {
			return e
		}
		names, e := f.Readdirnames(limits.DirectoryEntries + 1)
		closeErr := f.Close()
		if e != nil && e != io.EOF {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if len(names) > limits.DirectoryEntries {
			return ErrLimit
		}
		sort.Strings(names)
		for _, child := range names {
			if !utf8.ValidString(child) || strings.ContainsRune(child, 0) {
				return ErrUnsafe
			}
			next := child
			if name != "." {
				next = name + "/" + child
			}
			if e = walk(next, depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	target := selection
	if target == "" {
		target = "."
	}
	// Parents must be real directories and are included in the explicit TAR list.
	parts := strings.Split(selection, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		if mounts[filepath.Join(root, parent)] {
			return nil, ErrUnsafe
		}
		entry, e := observe(handle, parent)
		if e != nil {
			return nil, e
		}
		if !os.FileMode(entry.Mode).IsDir() || entry.Device != p.rootEntry.Device {
			return nil, ErrUnsafe
		}
		if e = emit(parent, entry); e != nil {
			return nil, e
		}
	}
	if e = walk(target, 0); e != nil {
		return nil, e
	}
	if p.Count == 0 {
		return nil, ErrUnsafe
	}
	if p.EstimatedBytes > math.MaxInt64-3*262144 {
		return nil, ErrLimit
	}
	p.EstimatedBytes += 3 * 262144
	if e = list.Sync(); e != nil {
		return nil, e
	}
	if e = rows.Sync(); e != nil {
		return nil, e
	}
	if e = list.Close(); e != nil {
		return nil, e
	}
	if e = rows.Close(); e != nil {
		return nil, e
	}
	if e = p.Validate(ctx); e != nil {
		return nil, e
	}
	return p, nil
}
func safeName(s string) bool {
	return s != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0) && !filepath.IsAbs(s) && filepath.ToSlash(filepath.Clean(s)) == s && s != "." && s != ".." && !strings.HasPrefix(s, "../")
}
func observe(root *os.Root, name string) (Entry, error) {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		info, e := root.Lstat(strings.Join(parts[:i], "/"))
		if e != nil {
			return Entry{}, e
		}
		if !info.IsDir() {
			return Entry{}, ErrUnsafe
		}
	}
	info, e := root.Lstat(name)
	if e != nil {
		return Entry{}, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Entry{}, ErrUnsafe
	}
	entry := Entry{Name: name, Size: info.Size(), Mtime: info.ModTime().UnixNano(), Mode: uint32(info.Mode()), Device: uint64(stat.Dev), Inode: stat.Ino}
	entry.Sparse = info.Mode().IsRegular() && stat.Blocks >= 0 && stat.Blocks <= math.MaxInt64/512 && stat.Blocks*512 < info.Size()
	if info.Mode()&os.ModeSymlink != 0 {
		entry.Link, e = root.Readlink(name)
		if !utf8.ValidString(entry.Link) {
			return Entry{}, ErrUnsafe
		}
	}
	return entry, e
}
func (p *Plan) Validate(ctx context.Context) error {
	if p == nil || p.privateDirectory == "" || !time.Now().Before(p.ExpiresAt) {
		return ErrChanged
	}
	mounts, e := mountBoundaries(p.root)
	if e != nil {
		return e
	}
	root, e := os.OpenRoot(p.root)
	if e != nil {
		return e
	}
	defer root.Close()
	current, e := observe(root, ".")
	if e != nil {
		return e
	}
	if current != p.rootEntry {
		return ErrChanged
	}
	f, e := os.Open(filepath.Join(p.privateDirectory, "entries.jsonl"))
	if e != nil {
		return e
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), p.limits.PathBytes*12+4096)
	var count int64
	for scanner.Scan() {
		if e = ctx.Err(); e != nil {
			return e
		}
		var expected Entry
		if e = json.Unmarshal(scanner.Bytes(), &expected); e != nil {
			return e
		}
		if mounts[filepath.Join(p.root, expected.Name)] {
			return ErrUnsafe
		}
		actual, e := observe(root, expected.Name)
		if e != nil {
			return fmt.Errorf("%w: %v", ErrChanged, e)
		}
		if actual != expected {
			return ErrChanged
		}
		count++
	}
	if e = scanner.Err(); e != nil {
		return e
	}
	if count != p.Count {
		return ErrChanged
	}
	return nil
}

// OpenFiles revalidates a plan and returns pinned directory and literal list
// descriptors. The caller owns both descriptors.
func (p *Plan) OpenFiles(ctx context.Context) (*os.File, *os.File, error) {
	if e := p.Validate(ctx); e != nil {
		return nil, nil, e
	}
	root, e := os.Open(p.root)
	if e != nil {
		return nil, nil, e
	}
	info, e := root.Stat()
	if e != nil {
		root.Close()
		return nil, nil, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != p.rootEntry.Device || stat.Ino != p.rootEntry.Inode {
		root.Close()
		return nil, nil, ErrChanged
	}
	list, e := os.Open(filepath.Join(p.privateDirectory, "paths.nul"))
	if e != nil {
		root.Close()
		return nil, nil, e
	}
	return root, list, nil
}
func (p *Plan) Close() error {
	if p == nil || p.cleanup == nil {
		return nil
	}
	p.closeOnce.Do(func() { p.closeError = errors.Join(p.cleanup.RemoveAll(p.cleanupName), p.cleanup.Close()) })
	return p.closeError
}

// Linux bind mounts can share a device ID. Check the kernel's mount table as
// well as st_dev so a configured source cannot silently cross either kind.
func mountBoundaries(root string) (map[string]bool, error) {
	f, e := os.Open("/proc/self/mountinfo")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return parseMountBoundaries(f, root)
}
func parseMountBoundaries(r io.Reader, root string) (map[string]bool, error) {
	mounts := make(map[string]bool)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, "\\")
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			return nil, ErrUnsafe
		}
		mount := decode.Replace(fields[4])
		if strings.HasPrefix(mount, strings.TrimSuffix(root, "/")+"/") {
			mounts[mount] = true
		}
	}
	return mounts, scanner.Err()
}
