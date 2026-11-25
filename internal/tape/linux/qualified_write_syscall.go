//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/maxya/TapePilot/internal/tape"
	"golang.org/x/sys/unix"
)

// hardwareWriteAuthority has no issuing path while the release allowlist is
// empty. Read-only profiles and local configuration cannot construct it.
type hardwareWriteAuthority struct{ profile string }
type qualifiedRecordFile struct{ file *os.File }

func openQualifiedRecordFile(ctx context.Context, c Config, captured Identity, authority *hardwareWriteAuthority) (qualificationRecordFD, error) {
	if authority == nil || !releasedWriteProfile(authority.profile) {
		return nil, ErrWritesDisabled
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	node, identity, e := pinNode(c.Device)
	if e != nil {
		return nil, e
	}
	defer node.Close()
	if identity.Major != captured.Major || identity.Minor != captured.Minor || identity.NodeInode != captured.NodeInode {
		return nil, ErrIdentity
	}
	observed, e := (realSystem{}).Inspect(ctx, c)
	if e != nil {
		return nil, e
	}
	if observed != captured {
		return nil, ErrIdentity
	}
	// This guarded code is unreachable in the current release. A future provider
	// must also reobserve identity and media after open before its first record.
	fd, e := unix.Open(fmt.Sprintf("/proc/self/fd/%d", node.Fd()), unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	file := os.NewFile(uintptr(fd), "qualified-tape-writer")
	observed, e = (realSystem{}).Inspect(ctx, c)
	if e != nil || observed != captured {
		return nil, errors.Join(ErrIdentity, e, file.Close())
	}
	return &qualifiedRecordFile{file}, nil
}
func (f *qualifiedRecordFile) WriteRecord(record []byte) (int, error) {
	if len(record) != tape.RecordBytes {
		return 0, ErrProfile
	}
	// One raw syscall only. No short-write loop, EINTR retry, padding or buffering.
	n, _, errno := unix.Syscall(unix.SYS_WRITE, f.file.Fd(), uintptr(unsafe.Pointer(&record[0])), uintptr(len(record)))
	if errno != 0 {
		return int(n), errno
	}
	return int(n), nil
}
func (f *qualifiedRecordFile) Close() error { return f.file.Close() }
