//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type realSystem struct{}

func pinNode(path string) (*os.File, Identity, error) {
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil {
		return nil, Identity{}, e
	}
	fd, e := unix.Open(resolved, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, Identity{}, e
	}
	file := os.NewFile(uintptr(fd), "configured-nonrewind-node")
	info, e := file.Stat()
	if e != nil {
		file.Close()
		return nil, Identity{}, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeCharDevice == 0 {
		file.Close()
		return nil, Identity{}, ErrIdentity
	}
	identity := Identity{Major: unix.Major(uint64(stat.Rdev)), Minor: unix.Minor(uint64(stat.Rdev)), NodeInode: stat.Ino}
	if identity.Major != 9 || identity.Minor&128 == 0 || identity.Minor&96 != 0 {
		file.Close()
		return nil, Identity{}, ErrIdentity
	}
	return file, identity, nil
}
func (realSystem) Inspect(ctx context.Context, c Config) (Identity, error) {
	if e := ctx.Err(); e != nil {
		return Identity{}, e
	}
	node, identity, e := pinNode(c.Device)
	if e != nil {
		return Identity{}, e
	}
	defer node.Close()
	var name unix.Utsname
	if e = unix.Uname(&name); e != nil {
		return Identity{}, e
	}
	return identityFromSysfs("/sys", identity, unix.ByteSliceToString(name.Release[:]))
}

var hctl = regexp.MustCompile(`^[0-9]+:[0-9]+:[0-9]+:[0-9]+$`)

func smallFile(path string, limit int64) ([]byte, error) {
	file, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer file.Close()
	raw, e := io.ReadAll(io.LimitReader(file, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(raw)) > limit {
		return nil, ErrProfile
	}
	return raw, nil
}
func identityFromSysfs(root string, identity Identity, kernel string) (Identity, error) {
	index := (identity.Minor & 31) | ((identity.Minor >> 8) << 5)
	base := filepath.Join(root, "class", "scsi_tape", "nst"+strconv.FormatUint(uint64(index), 10))
	raw, e := smallFile(filepath.Join(base, "dev"), 64)
	if e != nil {
		return Identity{}, e
	}
	if strings.TrimSpace(string(raw)) != fmt.Sprintf("%d:%d", identity.Major, identity.Minor) {
		return Identity{}, ErrIdentity
	}
	device, e := filepath.EvalSymlinks(filepath.Join(base, "device"))
	if e != nil {
		return Identity{}, e
	}
	relative, e := filepath.Rel(root, device)
	if e != nil || relative == ".." || strings.HasPrefix(relative, "../") {
		return Identity{}, ErrIdentity
	}
	identity.HCTL = filepath.Base(device)
	if !hctl.MatchString(identity.HCTL) {
		return Identity{}, ErrIdentity
	}
	for name, target := range map[string]*string{"vendor": &identity.Vendor, "model": &identity.Model, "rev": &identity.Firmware, "wwid": &identity.StableID} {
		raw, e = smallFile(filepath.Join(device, name), 256)
		if e != nil {
			return Identity{}, e
		}
		*target = strings.TrimSpace(string(raw))
		if !text(*target) {
			return Identity{}, ErrIdentity
		}
	}
	identity.KernelRelease = kernel
	return identity, nil
}
func mtBinary() (string, error) {
	for _, path := range []string{"/usr/bin/mt-st", "/bin/mt-st", "/usr/bin/mt"} {
		info, e := os.Stat(path)
		if e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
	}
	return "", ErrUtility
}

var mtVersion = regexp.MustCompile(`^mt-st v\. ([0-9]+(?:\.[0-9]+)+)$`)

func ParseMTVersion(raw []byte) (string, error) {
	if len(raw) > 4096 {
		return "", ErrUtility
	}
	line := strings.Split(strings.TrimSpace(string(raw)), "\n")[0]
	match := mtVersion.FindStringSubmatch(line)
	if len(match) != 2 {
		return "", ErrUtility
	}
	return match[1], nil
}
func (realSystem) MTVersion(ctx context.Context) (string, error) {
	binary, e := mtBinary()
	if e != nil {
		return "", e
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, e := runMT(ctx, binary, nil, "--version")
	if e != nil {
		return "", e
	}
	return ParseMTVersion(raw)
}
func (realSystem) OpenRead(ctx context.Context, c Config, captured Identity) (readDevice, error) {
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
	file, e := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", node.Fd()), os.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	observed, e = (realSystem{}).Inspect(ctx, c)
	if e != nil || observed != captured {
		file.Close()
		return nil, errors.Join(ErrIdentity, e)
	}
	return &physicalReader{file}, nil
}
func (realSystem) Motion(ctx context.Context, c Config, captured Identity, operation string, index uint64) error {
	switch operation {
	case "rewind", "offline":
		if index != 0 {
			return ErrPosition
		}
	case "fsf":
		if index < 1 || index > 100000 {
			return ErrPosition
		}
	default:
		return ErrWritesDisabled
	}
	node, identity, e := pinNode(c.Device)
	if e != nil {
		return e
	}
	defer node.Close()
	if identity.Major != captured.Major || identity.Minor != captured.Minor || identity.NodeInode != captured.NodeInode {
		return ErrIdentity
	}
	observed, e := (realSystem{}).Inspect(ctx, c)
	if e != nil {
		return e
	}
	if observed != captured {
		return ErrIdentity
	}
	binary, e := mtBinary()
	if e != nil {
		return e
	}
	arguments := []string{"-f", "/proc/self/fd/3", operation}
	if operation == "fsf" {
		arguments = append(arguments, strconv.FormatUint(index, 10))
	}
	output, e := runMT(ctx, binary, []*os.File{node}, arguments...)
	if e != nil {
		return e
	}
	if len(strings.TrimSpace(string(output))) != 0 {
		return ErrUtility
	}
	return nil
}

type commandOutput struct {
	mu       sync.Mutex
	raw      []byte
	overflow bool
}

func (b *commandOutput) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := 4096 - len(b.raw)
	if remaining < len(raw) {
		b.overflow = true
	} else {
		remaining = len(raw)
	}
	b.raw = append(b.raw, raw[:remaining]...)
	return len(raw), nil
}
func runMT(ctx context.Context, binary string, files []*os.File, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.ExtraFiles = files
	command.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	var output commandOutput
	command.Stdout = &output
	command.Stderr = &output
	if e := command.Run(); e != nil {
		return nil, errors.Join(ErrUtility, ctx.Err())
	}
	if output.overflow {
		return nil, ErrUtility
	}
	return output.raw, nil
}

type physicalReader struct{ file *os.File }

func (r *physicalReader) Read(ctx context.Context, buffer []byte) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	return syscall.Read(int(r.file.Fd()), buffer)
}
func (r *physicalReader) Close() error { return r.file.Close() }

// Linux's MTIOCGET is a fixed read-only driver-status request. This structure
// and request layout are supported here only on Linux amd64/arm64 LP64 ABIs.
// Source contract: include/uapi/linux/mtio.h, struct mtget and GMT_* masks.
type mtget struct {
	Type, Residual, DeviceStatus, GenericStatus, ErrorStatus int64
	FileNumber, BlockNumber                                  int32
}

func (r *physicalReader) Status(ctx context.Context) (Status, error) {
	if e := ctx.Err(); e != nil {
		return Status{}, e
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return Status{}, ErrProfile
	}
	var value mtget
	request := uintptr(2<<30) | uintptr(unsafe.Sizeof(value))<<16 | uintptr('m')<<8 | 2
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, r.file.Fd(), request, uintptr(unsafe.Pointer(&value)))
	if errno != 0 {
		return Status{}, errno
	}
	if value.Type != 0x71 && value.Type != 0x72 {
		return Status{}, ErrProfile
	}
	return decodeStatus(value), nil
}
func decodeStatus(value mtget) Status {
	flags := uint64(value.GenericStatus)
	return Status{Online: flags&0x01000000 != 0, WriteProtected: flags&0x04000000 != 0, BOT: flags&0x40000000 != 0, EOF: flags&0x80000000 != 0, EOD: flags&0x08000000 != 0, BlockBytes: value.DeviceStatus & 0xffffff, FileNumber: int64(value.FileNumber), BlockNumber: int64(value.BlockNumber)}
}
