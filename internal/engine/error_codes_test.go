package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/store"
)

func TestSchedulerBackupChangedSourceCodeAndSafetyState(t *testing.T) {
	for _, afterOpen := range []bool{false, true} {
		name := "preview mismatch"
		if afterOpen {
			name = "changed after writer open"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.initialize(t)
			options := BackupOptions{SourceID: "source", Selection: "data", SourceLabel: "Synthetic"}
			expected := domain.Failed
			if afterOpen {
				expected = domain.RecoveryRequired
				f.engine.testHook = func(phase string) error {
					if phase == "after_data_open" {
						return os.WriteFile(filepath.Join(f.sourceRoot, "data", "normal.txt"), []byte("changed synthetic contents"), 0600)
					}
					return nil
				}
			} else {
				options.PlanDigest = strings.Repeat("0", 64)
			}
			raw, e := json.Marshal(options)
			if e != nil {
				t.Fatal(e)
			}
			queued, e := f.store.Enqueue(ctx, store.Job{Kind: domain.Backup, VolumeID: f.volumeID, Options: raw})
			if e != nil {
				t.Fatal(e)
			}
			scheduler := jobs.New(f.store, f.engine)
			if worked, e := scheduler.RunOne(ctx); e != nil || !worked {
				t.Fatal(worked, e)
			}
			actual, e := f.store.GetJob(ctx, queued.ID)
			if e != nil || actual.State != expected || actual.ErrorCode != "SOURCE_CHANGED" {
				t.Fatalf("state=%s code=%s error=%v", actual.State, actual.ErrorCode, e)
			}
			if _, e = os.Stat(filepath.Join(f.root, "tape", "cartridge", "000002.tar")); !os.IsNotExist(e) {
				t.Fatal("commit written for changed source")
			}
			if count, e := f.store.ArchiveCount(ctx, f.volumeID); e != nil || count != 0 {
				t.Fatal("changed source cataloged", count, e)
			}
		})
	}
}
