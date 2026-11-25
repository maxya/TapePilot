package main

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/restore"
	"github.com/maxya/TapePilot/internal/store"
	"io"
	"strconv"
)

func catalogCommand(ctx context.Context, r *app.Runtime, s app.Services, args []string, out io.Writer, encode func(any) error) error {
	switch args[0] {
	case "import", "legacy-import":
		opts := map[string]any{"mode": "managed"}
		if args[0] == "import" {
			if len(args) > 2 {
				return errors.New(usage)
			}
			if len(args) == 2 {
				opts["expected_volume_id"] = args[1]
			}
		} else {
			if len(args) != 4 || args[3] != "--confirm" {
				return errors.New(usage)
			}
			opts = map[string]any{"mode": "legacy", "source_id": args[1], "profile": legacy.SimulatorProfile, "label": args[2], "confirmed": true}
		}
		return executeJob(ctx, r, s.Scheduler, store.Job{ID: uuid.NewString(), Kind: domain.Import}, opts, encode)
	case "restore", "restore-preview":
		if len(args) < 5 {
			return errors.New(usage)
		}
		opt := restore.Options{ArchiveID: args[1], RestoreRootID: args[2], DestinationSlug: args[3]}
		selections := args[4:]
		if len(selections) >= 3 && selections[len(selections)-2] == "--legacy" {
			opt.LegacySourceID = selections[len(selections)-1]
			opt.LegacyProfile = legacy.SimulatorProfile
			opt.LegacyConfirmation = true
			selections = selections[:len(selections)-2]
		}
		if len(selections) == 1 && selections[0] == "all" {
			opt.All = true
		} else {
			for _, id := range selections {
				n, err := strconv.ParseInt(id, 10, 64)
				if err != nil || n <= 0 {
					return errors.New("expected positive member ID")
				}
				opt.MemberIDs = append(opt.MemberIDs, n)
			}
		}
		plan, err := s.Restore.Preview(ctx, opt)
		if err != nil {
			return err
		}
		if args[0] == "restore-preview" {
			return encode(plan)
		}
		return executeJob(ctx, r, s.Scheduler, store.Job{ID: uuid.NewString(), Kind: domain.Restore, VolumeID: plan.VolumeID}, opt, encode)
	case "catalog-ndjson", "recovery-text", "descriptor":
		if len(args) != 2 {
			return errors.New(usage)
		}
		if args[0] == "descriptor" {
			return r.Store.WriteDescriptor(ctx, args[1], out)
		}
		if args[0] == "recovery-text" {
			return r.Store.WriteRecoveryText(ctx, args[1], out)
		}
		return r.Store.WriteCatalogNDJSON(ctx, args[1], out)
	case "members":
		if len(args) != 2 {
			return errors.New(usage)
		}
		m, err := r.Store.ListMembers(ctx, args[1], 0, 100)
		if err != nil {
			return err
		}
		return encode(m)
	}
	return catalogQuery(ctx, r, args, encode)
}

func catalogQuery(ctx context.Context, r *app.Runtime, args []string, encode func(any) error) error {
	if len(args) != 2 {
		return errors.New(usage)
	}
	switch args[0] {
	case "archives":
		rows, err := r.Store.ListArchives(ctx, store.ArchiveQuery{VolumeID: args[1], Limit: 100})
		if err != nil {
			return err
		}
		return encode(rows)
	case "search":
		rows, err := r.Store.SearchMembers(ctx, store.MemberQuery{Text: args[1], Limit: 100})
		if err != nil {
			return err
		}
		return encode(rows)
	}
	return errors.New(usage)
}
