package app

import (
	"github.com/maxya/TapePilot/internal/catalog"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/engine"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/restore"
)

type Services struct {
	Engine    *engine.Engine
	Restore   *restore.Restore
	Scheduler *jobs.Scheduler
}

func (r *Runtime) Services() Services {
	e := &engine.Engine{Store: r.Store, Drive: r.Drive, Spool: r.Spool, Sources: r.Sources}
	roots := make([]legacy.Root, 0, len(r.Settings.LegacyRoots))
	for id, p := range r.Settings.LegacyRoots {
		roots = append(roots, legacy.Root{ID: id, Path: p})
	}
	restores := &restore.Restore{Store: r.Store, Drive: r.Drive, Spool: r.Spool, Roots: r.Settings.RestoreRoots, LegacyRoots: roots, ReserveBytes: r.Settings.SpoolReserveBytes}
	imports := &catalog.Importer{Store: r.Store, Drive: r.Drive, LegacyRoots: roots}
	handlers := Handlers{domain.Initialize: e.Execute, domain.Backup: e.Execute, domain.Restore: restores.Execute, domain.Import: imports.Execute, domain.Verify: r.DriveJob, domain.Inspect: r.DriveJob, domain.Rewind: r.DriveJob, domain.Unload: r.DriveJob}
	return Services{e, restores, jobs.New(r.Store, handlers)}
}
