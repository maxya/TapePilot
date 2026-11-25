package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/maxya/TapePilot/internal/qualification"
	"golang.org/x/sys/unix"
)

// Open only a pinned regular document. A mistaken device path must never cause
// even a read-only tape open when evaluating an offline evidence submission.
func checkQualification(filename string, out io.Writer) (err error) {
	fd, e := unix.Open(filename, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return e
	}
	pin := os.NewFile(uintptr(fd), "evidence")
	defer pin.Close()
	info, e := pin.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return errors.New("qualification evidence must be a regular file")
	}
	f, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		return e
	}
	report, e := qualification.Check(f)
	if e = errors.Join(e, f.Close()); e != nil {
		return e
	}
	if e = json.NewEncoder(out).Encode(report); e != nil {
		return e
	}
	if !report.EligibleForOperatorReview {
		return errors.New("mandatory physical evidence remains outstanding")
	}
	return nil
}
