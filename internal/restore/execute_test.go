package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/catalog"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/engine"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape"
	"github.com/maxya/TapePilot/internal/tape/sim"
)

var ctx = context.Background()

type fixture struct {
	r                                 *Restore
	s                                 *store.Store
	drive                             *sim.Drive
	root, source, volumeID, archiveID string
	legacy                            bool
}

func fixtureRestore(t *testing.T, legacyMode bool) *fixture {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	src := filepath.Join(root, "source")
	out := filepath.Join(root, "output")
	spool := filepath.Join(root, "spool")
	for _, dir := range []string{filepath.Join(src, "data"), out, spool} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"normal.txt", "line\nbreak.txt", "-leading.txt", "猫.txt"} {
		if err = os.WriteFile(filepath.Join(src, "data", name), []byte("payload:"+name), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Link(filepath.Join(src, "data", "normal.txt"), filepath.Join(src, "data", "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("normal.txt", filepath.Join(src, "data", "symlink")); err != nil {
		t.Fatal(err)
	}
	d, err := sim.Open(filepath.Join(root, "tape"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: s, drive: d, root: root, source: src, volumeID: uuid.NewString(), legacy: legacyMode}
	f.r = &Restore{Store: s, Drive: d, Roots: map[string]string{"recovered": out}, Spool: spool, ReserveBytes: 1024}
	enqueueRun := func(kind domain.Kind, volumeID string, opts any, executor jobs.Executor) {
		raw, err := json.Marshal(opts)
		if err != nil {
			t.Fatal(err)
		}
		j, err := s.Enqueue(ctx, store.Job{Kind: kind, VolumeID: volumeID, Options: raw})
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := executor.(*engine.Engine); ok && kind == domain.Initialize {
			e.InitializeArmed = true
			e.InitializeJobID = j.ID
		}
		if ran, err := jobs.New(s, executor).RunOne(ctx); err != nil || !ran {
			t.Fatal(ran, err)
		}
		got, err := s.GetJob(ctx, j.ID)
		if err != nil || got.State != domain.Succeeded {
			t.Fatal(got, err)
		}
	}
	if legacyMode {
		legacyDir := filepath.Join(root, "legacy")
		if err = os.Mkdir(legacyDir, 0700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("/usr/bin/tar", "--create", "--format=pax", "--file="+filepath.Join(legacyDir, "000000.tar"), "--directory="+src, ".")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal(err, string(output))
		}
		f.r.LegacyRoots = []legacy.Root{{ID: "old", Path: legacyDir}}
		importer := &catalog.Importer{Store: s, LegacyRoots: f.r.LegacyRoots}
		enqueueRun(domain.Import, "", catalog.ImportOptions{Mode: "legacy", SourceID: "old", Profile: legacy.SimulatorProfile, Label: "Synthetic legacy", VolumeID: f.volumeID, Confirmed: true}, importer)
	} else {
		if err = d.Create("cartridge", 64*tape.RecordBytes, false); err != nil {
			t.Fatal(err)
		}
		if err = d.Insert("cartridge"); err != nil {
			t.Fatal(err)
		}
		l, err := d.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		status, err := l.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		obs, err := l.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		if err = s.CreateVolume(ctx, store.Volume{ID: f.volumeID, Kind: "MANAGED", Label: "Synthetic", Generation: "SIM", State: domain.BlankCandidate}); err != nil {
			t.Fatal(err)
		}
		e := &engine.Engine{Store: s, Drive: d, Sources: []source.Root{{ID: "source", Path: src}}, Spool: spool}
		enqueueRun(domain.Initialize, f.volumeID, engine.InitOptions{CartridgeID: "cartridge", Generation: status.Generation, Confirmed: true}, e)
		if err = s.EnrollVolume(ctx, store.Enrollment{VolumeID: f.volumeID, DriveID: obs.Drive, Profile: obs.Profile, OperatorRef: "test operator"}); err != nil {
			t.Fatal(err)
		}
		enqueueRun(domain.Backup, f.volumeID, engine.BackupOptions{SourceID: "source", Selection: "data", SourceLabel: "Synthetic"}, e)
	}
	archives, err := s.ListArchives(ctx, store.ArchiveQuery{VolumeID: f.volumeID, Limit: 5})
	if err != nil || len(archives) != 1 {
		t.Fatal(archives, err)
	}
	f.archiveID = archives[0].ID
	return f
}
func (f *fixture) options(slug string) Options {
	opts := Options{ArchiveID: f.archiveID, All: true, RestoreRootID: "recovered", DestinationSlug: slug}
	if f.legacy {
		opts.LegacySourceID = "old"
		opts.LegacyProfile = legacy.SimulatorProfile
		opts.LegacyConfirmation = true
	}
	return opts
}
func (f *fixture) run(t *testing.T, options Options) store.Job {
	t.Helper()
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Enqueue(ctx, store.Job{Kind: domain.Restore, VolumeID: f.volumeID, Options: raw})
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := jobs.New(f.s, f.r).RunOne(ctx); err != nil || !ran {
		t.Fatal(ran, err)
	}
	got, err := f.s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestManagedFullAndSelectiveRestore(t *testing.T) {
	f := fixtureRestore(t, false)
	all := f.run(t, f.options("full"))
	if all.State != domain.Succeeded {
		t.Fatal(all)
	}
	for _, name := range []string{"normal.txt", "line\nbreak.txt", "-leading.txt", "猫.txt"} {
		raw, err := os.ReadFile(filepath.Join(f.root, "output", "full", "data", name))
		if err != nil || string(raw) != "payload:"+name {
			t.Fatal(name, err)
		}
	}
	original, err := os.Stat(filepath.Join(f.root, "output", "full", "data", "normal.txt"))
	if err != nil {
		t.Fatal(err)
	}
	link, err := os.Stat(filepath.Join(f.root, "output", "full", "data", "hardlink"))
	if err != nil || !os.SameFile(original, link) {
		t.Fatal("hardlink mismatch", err)
	}
	target, err := os.Readlink(filepath.Join(f.root, "output", "full", "data", "symlink"))
	if err != nil || target != "normal.txt" {
		t.Fatal(target, err)
	}
	if original.Mode().Perm() != 0640 {
		t.Fatal("unprivileged mode changed", original.Mode())
	}
	members, err := f.s.ListMembers(ctx, f.archiveID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, m := range members {
		if m.Type == "1" {
			id = m.ID
		}
	}
	if id == 0 {
		t.Fatal("missing hardlink")
	}
	options := f.options("selected")
	options.All = false
	options.MemberIDs = []int64{id}
	preview, err := f.r.Preview(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SelectedMembers < 2 {
		t.Fatal("hardlink/parent dependencies missing", preview)
	}
	selected := f.run(t, options)
	if selected.State != domain.Succeeded {
		t.Fatal(selected)
	}
	if _, err = os.Stat(filepath.Join(f.root, "output", "selected", "data", "line\nbreak.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unselected member extracted")
	}
	result, err := f.s.RestorePublication(ctx, selected.ID)
	if err != nil || result.CompletedAt == "" || result.DestinationSlug != "selected" {
		t.Fatal(result, err)
	}
}
func TestLegacyObservedRestoreAndConfirmation(t *testing.T) {
	f := fixtureRestore(t, true)
	options := f.options("unconfirmed")
	options.LegacyConfirmation = false
	if got := f.run(t, options); got.State != domain.Failed {
		t.Fatal(got)
	}
	got := f.run(t, f.options("legacy"))
	if got.State != domain.Succeeded {
		t.Fatal(got)
	}
	data, err := os.ReadFile(filepath.Join(f.root, "output", "legacy", "data", "normal.txt"))
	if err != nil || string(data) != "payload:normal.txt" {
		t.Fatal(err)
	}
	archive, err := f.s.GetArchive(ctx, f.archiveID)
	if err != nil || archive.Integrity != domain.Observed {
		t.Fatal("legacy evidence changed", archive, err)
	}
}
func TestCorruptSpoolAndFailedExtractionLeaveNoPublishedTree(t *testing.T) {
	for _, phase := range []string{"after_spool", "before_extract", "after_extract", "before_rename"} {
		t.Run(phase, func(t *testing.T) {
			f := fixtureRestore(t, false)
			f.r.testHook = func(name string) error {
				if name != phase {
					return nil
				}
				if phase == "after_spool" {
					files, err := filepath.Glob(filepath.Join(f.r.Spool, "tapepilot-spool-*", "archive.tar"))
					if err != nil || len(files) != 1 {
						return errors.New("missing private spool")
					}
					file, err := os.OpenFile(files[0], os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, writeErr := file.WriteAt([]byte{255}, 512)
					return errors.Join(writeErr, file.Close())
				}
				return errors.New("injected restore failure")
			}
			job := f.run(t, f.options("failed"))
			if job.State == domain.Succeeded {
				t.Fatal("fault reported success")
			}
			if _, err := os.Stat(filepath.Join(f.root, "output", "failed")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed restore published", err)
			}
			spool, err := os.ReadDir(f.r.Spool)
			if err != nil || len(spool) != 0 {
				t.Fatal("private spool retained", err)
			}
			entries, err := os.ReadDir(filepath.Join(f.root, "output"))
			if err != nil || len(entries) != 0 {
				t.Fatal("private stage retained", err)
			}
		})
	}
}
func TestDestinationRaceNeverOverwritesAndPublicationFailureIsRecoverable(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		f := fixtureRestore(t, false)
		f.r.testHook = func(phase string) error {
			if !afterRename && phase == "before_rename" {
				return os.Mkdir(filepath.Join(f.root, "output", "raced"), 0700)
			}
			if afterRename && phase == "after_rename" {
				return errors.New("publication database unavailable")
			}
			return nil
		}
		got := f.run(t, f.options("raced"))
		if got.State != domain.RecoveryRequired {
			t.Fatal(got)
		}
		entries, err := os.ReadDir(filepath.Join(f.root, "output", "raced"))
		if err != nil {
			t.Fatal(err)
		}
		if !afterRename && len(entries) != 0 {
			t.Fatal("existing destination overwritten")
		}
		if afterRename && len(entries) == 0 {
			t.Fatal("published tree lost after DB failure")
		}
	}
}
func TestPreflightRejectsExistingTargetLowDiskAndHostPaths(t *testing.T) {
	f := fixtureRestore(t, false)
	if err := os.Mkdir(filepath.Join(f.root, "output", "exists"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Preview(ctx, f.options("exists")); !errors.Is(err, ErrDestination) {
		t.Fatal(err)
	}
	options := f.options("../outside")
	if _, err := f.r.Preview(ctx, options); err == nil {
		t.Fatal("destination traversal")
	}
	f.r.ReserveBytes = math.MaxInt64 - 1
	if got := f.run(t, f.options("space")); got.State != domain.Failed {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(f.root, "output", "space")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("low disk published")
	}
}

func TestManagedSourceMismatchUpdatesIntegrityButSpoolFailureDoesNot(t *testing.T) {
	for _, damageMedia := range []bool{false, true} {
		t.Run(fmt.Sprintf("media=%t", damageMedia), func(t *testing.T) {
			f := fixtureRestore(t, false)
			before, err := f.s.GetArchive(ctx, f.archiveID)
			if err != nil {
				t.Fatal(err)
			}
			if damageMedia {
				path := filepath.Join(f.root, "tape", "cartridge", "000001.tar")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				f.r.testHook = func(phase string) error {
					if phase == "after_spool" {
						return errors.New("private spool unavailable")
					}
					return nil
				}
			}
			if job := f.run(t, f.options("mismatch")); job.State == domain.Succeeded {
				t.Fatal(job)
			}
			current, err := f.s.GetArchive(ctx, f.archiveID)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.Verified
			if damageMedia {
				want = domain.IntegrityFailed
			}
			if current.Integrity != want || current.VerifiedAt != before.VerifiedAt || (damageMedia && current.RestoreEligible) {
				t.Fatal(current)
			}
		})
	}
}
