// Package linux contains opt-in, operator-supervised physical read-only access.
// No simulator or HTTP handler constructs a physical lease automatically.
package linux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"
)

var ErrApproval = errors.New("PHYSICAL_READ_APPROVAL_REQUIRED")
var ErrProfile = errors.New("UNQUALIFIED_PHYSICAL_READ_PROFILE")
var ErrIdentity = errors.New("PHYSICAL_IDENTITY_MISMATCH")
var ErrProtection = errors.New("WRITE_PROTECTION_REQUIRED")
var ErrPosition = errors.New("PHYSICAL_POSITION_UNCERTAIN")
var ErrBusy = errors.New("PHYSICAL_LEASE_BUSY")
var ErrClosed = errors.New("PHYSICAL_LEASE_CLOSED")
var ErrWritesDisabled = errors.New("PHYSICAL_WRITES_DISABLED")
var ErrLimit = errors.New("PHYSICAL_SNAPSHOT_LIMIT")
var ErrUtility = errors.New("MT_ST_REQUIRED")

type Config struct {
	Device           string `json:"device"`
	ExpectedIdentity string `json:"expected_identity"`
	ProfileFile      string `json:"profile_file"`
	LockDir          string `json:"lock_dir"`
}
type OperatorApproval struct{ ConfirmReadOnly, ConfirmWriteProtect bool }

func text(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func regularPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsRune(path, 0)
}
func (c Config) Validate() error {
	if !regularPath(c.Device) || !strings.HasPrefix(c.Device, "/dev/") || !text(c.ExpectedIdentity) || !regularPath(c.ProfileFile) || !regularPath(c.LockDir) {
		return ErrProfile
	}
	for _, path := range []string{c.ProfileFile, c.LockDir} {
		for _, system := range []string{"/dev", "/sys", "/proc"} {
			if path == system || strings.HasPrefix(path, system+"/") {
				return ErrProfile
			}
		}
	}
	return nil
}
func (c *Config) UnmarshalJSON(raw []byte) error {
	type plain Config
	var next plain
	if e := strict(raw, &next); e != nil {
		return e
	}
	value := Config(next)
	if e := value.Validate(); e != nil {
		return e
	}
	*c = value
	return nil
}

type ReadProfile struct {
	ID               string `json:"id"`
	Identity         string `json:"identity"`
	Vendor           string `json:"vendor"`
	Model            string `json:"model"`
	Firmware         string `json:"firmware"`
	KernelRelease    string `json:"kernel_release"`
	MTSTVersion      string `json:"mt_st_version"`
	BlockMode        string `json:"block_mode"`
	MaxRecordBytes   int    `json:"max_record_bytes"`
	ExactRecordBytes int    `json:"exact_record_bytes"`
	FilemarkPolicy   string `json:"filemark_policy"`
	EODPolicy        string `json:"eod_policy"`
	ReadOnlyApproved bool   `json:"read_only_approved"`
	QualificationRef string `json:"qualification_ref"`
}

func (p ReadProfile) Validate() error {
	for _, s := range []string{p.ID, p.Identity, p.Vendor, p.Model, p.Firmware, p.KernelRelease, p.MTSTVersion, p.QualificationRef} {
		if !text(s) {
			return ErrProfile
		}
	}
	if p.BlockMode != "variable" || p.MaxRecordBytes < 512 || p.MaxRecordBytes > 1<<20 || p.MaxRecordBytes%512 != 0 || p.ExactRecordBytes < 0 || p.ExactRecordBytes > p.MaxRecordBytes || p.ExactRecordBytes%512 != 0 || p.FilemarkPolicy != "zero-read-with-gmt-eof" || p.EODPolicy != "zero-read-with-gmt-eod" {
		return ErrProfile
	}
	return nil
}
func (p ReadProfile) Fingerprint() (string, error) {
	if e := p.Validate(); e != nil {
		return "", e
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func LoadProfile(path string) (ReadProfile, error) {
	var p ReadProfile
	if !regularPath(path) {
		return p, ErrProfile
	}
	fd, e := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return p, e
	}
	pinned := os.NewFile(uintptr(fd), "read-profile")
	defer pinned.Close()
	info, e := pinned.Stat()
	if e != nil {
		return p, e
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 || info.Mode().Perm()&0022 != 0 {
		return p, ErrProfile
	}
	file, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		return p, e
	}
	defer file.Close()
	raw, e := io.ReadAll(io.LimitReader(file, 16385))
	if e != nil {
		return p, e
	}
	if e = strict(raw, &p); e != nil {
		return p, e
	}
	return p, p.Validate()
}
func strict(raw []byte, dest any) error {
	if len(raw) > 16384 || !utf8.Valid(raw) {
		return ErrProfile
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return ErrProfile
	}
	fields := make(map[string]bool)
	t := reflect.TypeOf(dest).Elem()
	for i := 0; i < t.NumField(); i++ {
		fields[t.Field(i).Tag.Get("json")] = true
	}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return ErrProfile
		}
		name, ok := key.(string)
		if !ok || !fields[name] {
			return ErrProfile
		}
		delete(fields, name)
		var value json.RawMessage
		if d.Decode(&value) != nil || string(value) == "null" {
			return ErrProfile
		}
	}
	if token, e = d.Token(); e != nil || token != json.Delim('}') {
		return ErrProfile
	}
	if _, e = d.Token(); e != io.EOF {
		return ErrProfile
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dest) != nil {
		return ErrProfile
	}
	return nil
}
