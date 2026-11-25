// Package sim implements a disk-backed record tape. Its numbered TAR files are
// independently readable with standard utilities. It never accesses devices.
package sim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maxya/TapePilot/internal/tape"
)

var (
	ErrBusy       = errors.New("DRIVE_BUSY")
	ErrNoMedia    = errors.New("NO_MEDIA")
	ErrPosition   = errors.New("POSITION_UNCERTAIN")
	ErrProtected  = errors.New("WRITE_PROTECTED")
	ErrUncertain  = errors.New("RECOVERY_REQUIRED")
	ErrEndOfMedia = errors.New("END_OF_MEDIA")
	ErrInjected   = errors.New("SIMULATED_FAILURE")
	ErrClosed     = errors.New("LEASE_CLOSED")
	validID       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// Faults are local test controls. They must never be populated from HTTP input.
// Record numbers are one-based within the next writer or reader.
type Faults struct {
	FailWriteRecord   int64
	ShortWriteRecord  int64
	FailReadRecord    int64
	CorruptReadRecord int64
	FailClose         bool
	FailCloseAtIndex  *uint64
	EndOfMediaRecord  int64
	FailMotion        bool
}
type media struct {
	Version        int     `json:"version"`
	ID             string  `json:"id"`
	Generation     string  `json:"generation"`
	Capacity       int64   `json:"capacity_bytes"`
	WriteProtected bool    `json:"write_protected"`
	Files          []int64 `json:"records_per_file"`
	Pending        *uint64 `json:"pending_file,omitempty"`
	Uncertain      bool    `json:"uncertain"`
}

// Drive owns one process-local selection. A shared flock prevents other Drive
// instances from acquiring its root until the lease is released.
type Drive struct {
	mu       sync.Mutex
	root     string
	inserted string
	active   *Lease
	faults   Faults
}
type Lease struct {
	drive        *Drive
	lock         *os.File
	data         media
	dir          string
	observation  tape.Observation
	file         uint64
	busy         bool
	closed       bool
	appendProof  *tape.AppendProof
	intent       string
	purpose      string
	commitReady  uint64
	commitIntent string
}

func randomID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func validRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || root == "/dev" || strings.HasPrefix(root, "/dev/") {
		return errors.New("INVALID_SIMULATOR_ROOT")
	}
	// Require pre-existing trusted ancestors and reject aliases before mutation.
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		return err
	}
	if parent != filepath.Dir(root) {
		return errors.New("INVALID_SIMULATOR_ROOT")
	}
	if info, err := os.Lstat(root); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("INVALID_SIMULATOR_ROOT")
	}
	return nil
}
func Open(root string) (*Drive, error) {
	if err := validRoot(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("SIMULATOR_ROOT_NOT_PRIVATE")
	}
	return &Drive{root: root}, nil
}
func (d *Drive) Create(id string, capacity int64, protected bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		return ErrBusy
	}
	if !validID.MatchString(id) || capacity < tape.RecordBytes {
		return errors.New("INVALID_CARTRIDGE")
	}
	generation, err := randomID()
	if err != nil {
		return err
	}
	dir := filepath.Join(d.root, id)
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	return save(dir, media{Version: 1, ID: id, Generation: generation, Capacity: capacity, WriteProtected: protected, Files: []int64{}})
}
func (d *Drive) Insert(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		return ErrBusy
	}
	if !validID.MatchString(id) {
		return errors.New("INVALID_CARTRIDGE")
	}
	_, err := load(filepath.Join(d.root, id))
	if err != nil {
		return err
	}
	d.inserted = id
	return nil
}
func (d *Drive) SetFaults(f Faults) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		return ErrBusy
	}
	if f.FailCloseAtIndex != nil {
		index := *f.FailCloseAtIndex
		f.FailCloseAtIndex = &index
	}
	d.faults = f
	return nil
}
func (d *Drive) Acquire(ctx context.Context) (*Lease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.active != nil {
		return nil, ErrBusy
	}
	if d.inserted == "" {
		return nil, ErrNoMedia
	}
	lock, err := os.OpenFile(filepath.Join(d.root, "drive.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrBusy
	}
	dir := filepath.Join(d.root, d.inserted)
	m, err := load(dir)
	if err != nil {
		lock.Close()
		return nil, err
	}
	leaseID, err := randomID()
	if err != nil {
		lock.Close()
		return nil, err
	}
	nonce, err := randomID()
	if err != nil {
		lock.Close()
		return nil, err
	}
	l := &Lease{drive: d, lock: lock, data: m, dir: dir, observation: tape.Observation{Drive: "simulator:" + d.root, Profile: "simulator-v1", Lease: leaseID, MediaGeneration: m.Generation, Nonce: nonce, ObservedAt: time.Now().UTC()}}
	d.active = l
	return l, nil
}
func (l *Lease) check(ctx context.Context) error {
	if l.closed || l.drive.active != l || l.drive.inserted != l.data.ID || l.observation.Nonce == "" {
		return ErrClosed
	}
	return ctx.Err()
}
func (l *Lease) Close() error {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if l.busy {
		return ErrBusy
	}
	l.closed = true
	l.drive.active = nil
	return l.lock.Close()
}
func (l *Lease) Identity(ctx context.Context) (string, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	return l.observation.Drive, l.check(ctx)
}
func (l *Lease) Observe(ctx context.Context) (tape.Observation, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return tape.Observation{}, err
	}
	if l.busy {
		return tape.Observation{}, ErrBusy
	}
	return l.observation, nil
}
func (l *Lease) motion(ctx context.Context, index uint64) error {
	if err := l.check(ctx); err != nil {
		return err
	}
	if l.busy {
		return ErrBusy
	}
	if l.drive.faults.FailMotion {
		return ErrInjected
	}
	if index > uint64(len(l.data.Files)) {
		return ErrPosition
	}
	l.file = index
	l.commitReady = 0
	l.commitIntent = ""
	l.intent = ""
	l.purpose = ""
	l.appendProof = nil
	return nil
}
func (l *Lease) Rewind(ctx context.Context) error {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	return l.motion(ctx, 0)
}
func (l *Lease) PositionFile(ctx context.Context, index uint64) error {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	return l.motion(ctx, index)
}
func (l *Lease) Unload(ctx context.Context) error {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return err
	}
	if l.busy {
		return ErrBusy
	}
	if l.drive.faults.FailMotion {
		return ErrInjected
	}
	l.drive.inserted = ""
	l.observation.Nonce = ""
	return nil
}
func (l *Lease) PositionAppend(ctx context.Context, proof tape.AppendProof) error {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return err
	}
	if l.busy {
		return ErrBusy
	}
	if l.data.Uncertain || l.data.Pending != nil {
		return ErrUncertain
	}
	if proof.Observation != l.observation || proof.NextIndex != uint64(len(l.data.Files)) || proof.NextIndex%2 != 1 || proof.VolumeID == "" || proof.HeaderDigest == ([32]byte{}) {
		return ErrPosition
	}
	if err := l.motion(ctx, proof.NextIndex); err != nil {
		return err
	}
	l.appendProof = &proof
	return nil
}

// AuthorizeInitialization requires an explicit local decision for a synthetic
// cartridge and a durable engine intent ID. It never accepts populated media.
func (l *Lease) AuthorizeInitialization(ctx context.Context, intentID string, confirmed bool) (tape.WritePermit, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	if !confirmed || l.busy || l.file != 0 || len(l.data.Files) != 0 || l.data.Uncertain || l.data.Pending != nil || l.data.WriteProtected {
		return nil, ErrUncertain
	}
	l.purpose = "initialize"
	return l.authorize(intentID)
}

// AuthorizeAppend follows a freshly checked PositionAppend proof. The engine
// owns format validation and durable enrollment, including seal/revocation.
func (l *Lease) AuthorizeAppend(ctx context.Context, intentID string) (tape.WritePermit, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	if l.appendProof == nil || l.file != l.appendProof.NextIndex || l.busy {
		return nil, ErrPosition
	}
	l.purpose = "data"
	return l.authorize(intentID)
}

// AuthorizeCommit is available only immediately after this lease closed a
// complete data file. Restarted or read-only leases cannot synthesize a commit.
func (l *Lease) AuthorizeCommit(ctx context.Context, intentID string) (tape.WritePermit, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	if l.busy || l.commitReady == 0 || l.commitReady != l.file || l.file != uint64(len(l.data.Files)) || l.file%2 != 0 || intentID != l.commitIntent {
		return nil, ErrPosition
	}
	l.purpose = "commit"
	return l.authorize(intentID)
}
func (l *Lease) authorize(intentID string) (tape.WritePermit, error) {
	if intentID == "" {
		return nil, errors.New("WRITE_NOT_ARMED")
	}
	l.intent = intentID
	return tape.NewSimulatorPermit(tape.SimulatorScope{Observation: l.observation, FileIndex: l.file, IntentID: intentID}, time.Now().Add(time.Minute)), nil
}
func (l *Lease) OpenWriter(ctx context.Context, permit tape.WritePermit) (tape.RecordWriter, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	if l.busy {
		return nil, ErrBusy
	}
	if l.data.WriteProtected {
		return nil, ErrProtected
	}
	if l.data.Uncertain || l.data.Pending != nil {
		return nil, ErrUncertain
	}
	if l.file != uint64(len(l.data.Files)) {
		return nil, ErrPosition
	}
	fresh, err := load(l.dir)
	if err != nil {
		return nil, err
	}
	if fresh.Generation != l.data.Generation || fresh.ID != l.data.ID || len(fresh.Files) != len(l.data.Files) || fresh.Uncertain || fresh.Pending != nil {
		return nil, ErrUncertain
	}
	if fresh.WriteProtected {
		return nil, ErrProtected
	}
	scope := tape.SimulatorScope{Observation: l.observation, FileIndex: l.file, IntentID: l.intent}
	if err := tape.ConsumeSimulatorPermit(permit, scope, time.Now()); err != nil {
		return nil, err
	}
	index := l.file
	l.data.Pending = &index
	l.data.Uncertain = true
	if err := save(l.dir, l.data); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filePath(l.dir, index), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	l.busy = true
	l.appendProof = nil
	intent := l.intent
	l.intent = ""
	purpose := l.purpose
	l.purpose = ""
	return &writer{lease: l, file: f, purpose: purpose, intent: intent}, nil
}
func filePath(dir string, index uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%06d.tar", index))
}
func (l *Lease) OpenReader(ctx context.Context) (tape.RecordReader, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	if l.busy {
		return nil, ErrBusy
	}
	l.busy = true
	l.intent = ""
	l.purpose = ""
	l.commitReady = 0
	l.commitIntent = ""
	l.appendProof = nil
	return &reader{lease: l}, nil
}

func load(dir string) (media, error) {
	var m media
	info, err := os.Lstat(dir)
	if err != nil {
		return m, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return m, ErrUncertain
	}
	f, err := os.OpenFile(filepath.Join(dir, "media.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1024*1024))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return m, ErrUncertain
	}
	if m.Version != 1 || !validID.MatchString(m.ID) || filepath.Base(dir) != m.ID || m.Generation == "" || m.Capacity < tape.RecordBytes {
		return m, ErrUncertain
	}
	var used int64
	expectedFiles := make(map[string]bool, len(m.Files))
	for i, count := range m.Files {
		expectedFiles[filepath.Base(filePath(dir, uint64(i)))] = true
		if count <= 0 || count > m.Capacity/tape.RecordBytes || used > m.Capacity-count*tape.RecordBytes {
			return m, ErrUncertain
		}
		used += count * tape.RecordBytes
		info, err := os.Lstat(filePath(dir, uint64(i)))
		if err != nil || !info.Mode().IsRegular() || info.Size() != count*tape.RecordBytes {
			return m, ErrUncertain
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return m, err
	}
	expected := len(m.Files)
	if m.Pending != nil {
		if *m.Pending != uint64(expected) || !m.Uncertain {
			return m, ErrUncertain
		}
	}
	for _, entry := range entries {
		if entry.Name() == "media.json" {
			continue
		}
		if m.Pending != nil && entry.Name() == filepath.Base(filePath(dir, *m.Pending)) {
			continue
		}
		if !expectedFiles[entry.Name()] {
			return m, ErrUncertain
		}
	}
	return m, nil
}
func save(dir string, m media) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".media-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, "media.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Status reports the current leased synthetic cartridge without moving it.
// CompletedFiles counts physical files, independently of archive validity.
type Status struct {
	CartridgeID    string
	Generation     string
	CapacityBytes  int64
	UsedBytes      int64
	CompletedFiles uint64
	WriteProtected bool
	Uncertain      bool
}

func (l *Lease) Status(ctx context.Context) (Status, error) {
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if err := l.check(ctx); err != nil {
		return Status{}, err
	}
	if l.busy {
		return Status{}, ErrBusy
	}
	result := Status{CartridgeID: l.data.ID, Generation: l.data.Generation, CapacityBytes: l.data.Capacity, CompletedFiles: uint64(len(l.data.Files)), WriteProtected: l.data.WriteProtected, Uncertain: l.data.Uncertain}
	for _, count := range l.data.Files {
		result.UsedBytes += count * tape.RecordBytes
	}
	return result, nil
}

var _ tape.Adapter = (*Lease)(nil)
