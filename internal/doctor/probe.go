package doctor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var errOutputLimit = errors.New("VERSION_OUTPUT_LIMIT")

// version executes an observed regular executable through its pinned descriptor.
// Every production call must match the fixed package allowlist and version argv.
func version(ctx context.Context, name string, args []string) (string, error) {
	allowed := false
	for _, tool := range tools {
		if tool.path == name && len(args) == 1 && args[0] == "--version" {
			allowed = true
		}
	}
	if !allowed {
		return "", errors.New("UNSUPPORTED_VERSION_PROBE")
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	if protectedPath(resolved) || !(strings.HasPrefix(resolved, "/usr/bin/") || strings.HasPrefix(resolved, "/bin/") || strings.HasPrefix(resolved, "/usr/sbin/")) {
		return "", errors.New("UNSAFE_UTILITY")
	}
	file, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return "", errors.New("UNTRUSTED_UTILITY_PERMISSIONS")
	}
	var magic [4]byte
	if _, err = file.ReadAt(magic[:], 0); err != nil || !bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) {
		return "", errors.New("UNSUPPORTED_UTILITY_EXECUTABLE")
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "--version")
	cmd.Args[0] = name
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	output := &limitedOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if output.exceeded {
		return "", errOutputLimit
	}
	if err != nil {
		return "", err
	}
	return string(output.data), nil
}

type limitedOutput struct {
	mu       sync.Mutex
	data     []byte
	exceeded bool
}

func (w *limitedOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(data) > 4096-len(w.data) {
		w.exceeded = true
		return 0, io.ErrShortBuffer
	}
	w.data = append(w.data, data...)
	return len(data), nil
}
