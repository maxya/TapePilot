package source

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"sort"
)

type Child struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

// Browse opens the selected directory with kernel-enforced root, link and mount
// boundaries. A rename racing the request cannot redirect it to another tree.
func Browse(root, selection string) ([]Child, error) {
	if selection == "" {
		selection = "."
	}
	if selection != "." && !safeName(selection) {
		return nil, ErrUnsafe
	}
	rootFD, e := unix.Open(root, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_PATH, 0)
	if e != nil {
		return nil, e
	}
	defer unix.Close(rootFD)
	fd, e := unix.Openat2(rootFD, selection, &unix.OpenHow{Flags: unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_RDONLY, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if e != nil {
		return nil, ErrUnsafe
	}
	f := os.NewFile(uintptr(fd), selection)
	defer f.Close()
	rows, e := f.ReadDir(10001)
	if e != nil && !errors.Is(e, io.EOF) {
		return nil, e
	}
	if len(rows) > 10000 {
		return nil, ErrLimit
	}
	out := make([]Child, 0, len(rows))
	for _, v := range rows {
		out = append(out, Child{v.Name(), v.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
