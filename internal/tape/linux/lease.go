package linux

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/maxya/TapePilot/internal/tape"
)

type Identity struct {
	StableID      string `json:"stable_id"`
	Vendor        string `json:"vendor"`
	Model         string `json:"model"`
	Firmware      string `json:"firmware"`
	HCTL          string `json:"hctl"`
	KernelRelease string `json:"kernel_release"`
	Major         uint32 `json:"major"`
	Minor         uint32 `json:"minor"`
	NodeInode     uint64 `json:"node_inode"`
}
type Status struct {
	Online         bool  `json:"online"`
	WriteProtected bool  `json:"write_protected"`
	BOT            bool  `json:"bot"`
	EOF            bool  `json:"eof"`
	EOD            bool  `json:"eod"`
	BlockBytes     int64 `json:"block_bytes"`
	FileNumber     int64 `json:"file_number"`
	BlockNumber    int64 `json:"block_number"`
}
type Report struct {
	Identity              Identity `json:"identity"`
	Status                Status   `json:"status"`
	ProfileFingerprint    string   `json:"profile_fingerprint"`
	MTSTVersion           string   `json:"mt_st_version"`
	PhysicalWritesEnabled bool     `json:"physical_writes_enabled"`
}
type readDevice interface {
	Read(context.Context, []byte) (int, error)
	Status(context.Context) (Status, error)
	Close() error
}
type system interface {
	Inspect(context.Context, Config) (Identity, error)
	MTVersion(context.Context) (string, error)
	OpenRead(context.Context, Config, Identity) (readDevice, error)
	Motion(context.Context, Config, Identity, string, uint64) error
}
type Lease struct {
	mu           sync.Mutex
	config       Config
	profile      ReadProfile
	report       Report
	observation  tape.Observation
	host         system
	lock         *os.File
	busy, closed bool
}

func AcquireReadOnly(ctx context.Context, c Config, a OperatorApproval) (*Lease, error) {
	return acquire(ctx, c, a, realSystem{})
}
func acquire(ctx context.Context, c Config, a OperatorApproval, host system) (lease *Lease, err error) {
	if !a.ConfirmReadOnly || !a.ConfirmWriteProtect {
		return nil, ErrApproval
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := c.Validate(); e != nil {
		return nil, e
	}
	profile, e := LoadProfile(c.ProfileFile)
	if e != nil {
		return nil, e
	}
	if !profile.ReadOnlyApproved || profile.Identity != c.ExpectedIdentity {
		return nil, ErrProfile
	}
	// The administrator provisions the same private lock directory for native and
	// container access. The process never creates or repairs host permissions.
	resolved, e := filepath.EvalSymlinks(c.LockDir)
	if e != nil {
		return nil, e
	}
	if resolved != c.LockDir {
		return nil, ErrProfile
	}
	info, e := os.Stat(c.LockDir)
	if e != nil {
		return nil, e
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || int(owner.Uid) != os.Geteuid() {
		return nil, ErrProfile
	}
	sum := sha256.Sum256([]byte(c.ExpectedIdentity))
	lock, e := os.OpenFile(filepath.Join(c.LockDir, hex.EncodeToString(sum[:])+".lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	lockInfo, e := lock.Stat()
	if e != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, ErrProfile
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, ErrBusy
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	identity, e := host.Inspect(ctx, c)
	if e != nil {
		return nil, e
	}
	if identity.StableID != profile.Identity || identity.Vendor != profile.Vendor || identity.Model != profile.Model || identity.Firmware != profile.Firmware || identity.KernelRelease != profile.KernelRelease || identity.Major != 9 || identity.Minor&128 == 0 {
		return nil, ErrIdentity
	}
	version, e := host.MTVersion(ctx)
	if e != nil {
		return nil, e
	}
	if version != profile.MTSTVersion {
		return nil, ErrUtility
	}
	device, e := host.OpenRead(ctx, c, identity)
	if e != nil {
		return nil, e
	}
	status, statusErr := device.Status(ctx)
	closeErr := device.Close()
	if e = errors.Join(statusErr, closeErr); e != nil {
		return nil, e
	}
	if e = validStatus(status); e != nil {
		return nil, e
	}
	fingerprint, e := profile.Fingerprint()
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return &Lease{config: c, profile: profile, report: Report{Identity: identity, Status: status, ProfileFingerprint: fingerprint, MTSTVersion: version}, observation: tape.Observation{Drive: identity.StableID, Profile: fingerprint, Lease: hex.EncodeToString(nonce), Nonce: hex.EncodeToString(nonce), ObservedAt: time.Now().UTC()}, host: host, lock: lock}, nil
}
func validStatus(status Status) error {
	if !status.WriteProtected {
		return ErrProtection
	}
	if !status.Online || status.BlockBytes != 0 {
		return ErrProfile
	}
	return nil
}
func Doctor(ctx context.Context, c Config, a OperatorApproval) (Report, error) {
	lease, e := AcquireReadOnly(ctx, c, a)
	if e != nil {
		return Report{}, e
	}
	report := lease.report
	return report, lease.Close()
}
func (l *Lease) check(ctx context.Context) error {
	if l.closed {
		return ErrClosed
	}
	return ctx.Err()
}
func (l *Lease) fresh(ctx context.Context) (Status, error) {
	identity, e := l.host.Inspect(ctx, l.config)
	if e != nil {
		return Status{}, e
	}
	if identity != l.report.Identity {
		return Status{}, ErrIdentity
	}
	version, e := l.host.MTVersion(ctx)
	if e != nil {
		return Status{}, e
	}
	if version != l.report.MTSTVersion {
		return Status{}, ErrUtility
	}
	device, e := l.host.OpenRead(ctx, l.config, identity)
	if e != nil {
		return Status{}, e
	}
	status, statusErr := device.Status(ctx)
	closeErr := device.Close()
	if e = errors.Join(statusErr, closeErr); e != nil {
		return Status{}, e
	}
	return status, validStatus(status)
}
func (l *Lease) Identity(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.check(ctx); e != nil {
		return "", e
	}
	if l.busy {
		return "", ErrBusy
	}
	if _, e := l.fresh(ctx); e != nil {
		return "", e
	}
	return l.report.Identity.StableID, nil
}
func (l *Lease) Observe(ctx context.Context) (tape.Observation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.check(ctx); e != nil {
		return tape.Observation{}, e
	}
	if l.busy {
		return tape.Observation{}, ErrBusy
	}
	status, e := l.fresh(ctx)
	if e != nil {
		return tape.Observation{}, e
	}
	l.report.Status = status
	return l.observation, nil
}
func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.busy {
		return ErrBusy
	}
	l.closed = true
	return l.lock.Close()
}
func (l *Lease) OpenWriter(context.Context, tape.WritePermit) (tape.RecordWriter, error) {
	return nil, ErrWritesDisabled
}
func (l *Lease) PositionAppend(context.Context, tape.AppendProof) error { return ErrWritesDisabled }
func (l *Lease) motion(ctx context.Context, op string, index uint64) error {
	if e := l.check(ctx); e != nil {
		return e
	}
	if l.busy {
		return ErrBusy
	}
	if _, e := l.fresh(ctx); e != nil {
		return e
	}
	if e := l.host.Motion(ctx, l.config, l.report.Identity, op, index); e != nil {
		return e
	}
	identity, e := l.host.Inspect(ctx, l.config)
	if e != nil {
		return e
	}
	if identity != l.report.Identity {
		return ErrIdentity
	}
	if op == "offline" {
		return nil
	}
	status, e := l.fresh(ctx)
	if e != nil {
		return e
	}
	if status.FileNumber != int64(index) || status.BlockNumber != 0 || op == "rewind" && !status.BOT {
		return ErrPosition
	}
	l.report.Status = status
	return nil
}
func (l *Lease) Rewind(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.motion(ctx, "rewind", 0)
}
func (l *Lease) PositionFile(ctx context.Context, index uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if index > 100000 {
		return ErrLimit
	}
	if e := l.motion(ctx, "rewind", 0); e != nil {
		return e
	}
	if index == 0 {
		return nil
	}
	return l.motion(ctx, "fsf", index)
}
func (l *Lease) Unload(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.motion(ctx, "offline", 0)
}
func (l *Lease) OpenReader(ctx context.Context) (tape.RecordReader, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.check(ctx); e != nil {
		return nil, e
	}
	if l.busy {
		return nil, ErrBusy
	}
	if _, e := l.fresh(ctx); e != nil {
		return nil, e
	}
	device, e := l.host.OpenRead(ctx, l.config, l.report.Identity)
	if e != nil {
		return nil, e
	}
	status, e := device.Status(ctx)
	if e != nil {
		device.Close()
		return nil, e
	}
	if e = validStatus(status); e != nil {
		device.Close()
		return nil, e
	}
	l.busy = true
	return &reader{lease: l, device: device}, nil
}

type reader struct {
	lease                             *Lease
	device                            readDevice
	inFile, afterMark, closed, failed bool
}

func (r *reader) ReadRecord(ctx context.Context, buffer []byte) (int, tape.RecordKind, error) {
	l := r.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.closed {
		return 0, 0, ErrClosed
	}
	if r.failed {
		return 0, 0, ErrPosition
	}
	if e := l.check(ctx); e != nil {
		return 0, 0, e
	}
	if len(buffer) < l.profile.MaxRecordBytes {
		return 0, 0, ErrLimit
	}
	n, e := r.device.Read(ctx, buffer[:l.profile.MaxRecordBytes])
	if e != nil {
		r.failed = true
		return n, 0, e
	}
	if n > 0 {
		if n > l.profile.MaxRecordBytes || n%512 != 0 || l.profile.ExactRecordBytes != 0 && n != l.profile.ExactRecordBytes {
			r.failed = true
			return n, 0, ErrProfile
		}
		r.inFile = true
		r.afterMark = false
		return n, tape.Data, nil
	}
	status, e := r.device.Status(ctx)
	if e != nil {
		r.failed = true
		return 0, 0, e
	}
	if e = validStatus(status); e != nil {
		r.failed = true
		return 0, 0, e
	}
	if r.inFile && status.EOF {
		r.inFile = false
		r.afterMark = true
		return 0, tape.Filemark, nil
	}
	if !r.inFile && status.EOD && (r.afterMark || status.BOT) {
		return 0, tape.EOD, nil
	}
	if !r.inFile && status.EOF {
		r.afterMark = true
		return 0, tape.Filemark, nil
	}
	r.failed = true
	return 0, 0, ErrPosition
}
func (r *reader) Close() error {
	l := r.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	e := r.device.Close()
	l.busy = false
	return e
}
