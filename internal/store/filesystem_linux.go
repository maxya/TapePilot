package store

import (
	"errors"
	"syscall"
)

// SQLite WAL relies on local locking and shared memory semantics. Enable only
// local Linux filesystems supported by the current deployment profile.
func checkLocalFilesystem(dir string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return err
	}
	switch uint32(stat.Type) {
	case 0xef53, 0x9123683e, 0x58465342, 0x2fc12fc1, 0x794c7630, 0x01021994:
		// ext family, Btrfs, XFS, ZFS, overlayfs, tmpfs (disposable tests).
		return nil
	default:
		return errors.New("unsupported state filesystem: local WAL-capable storage required")
	}
}
