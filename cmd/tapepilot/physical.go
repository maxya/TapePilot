package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/maxya/TapePilot/internal/config"
	linux "github.com/maxya/TapePilot/internal/tape/linux"
	"io"
	"strconv"
)

func physicalCommand(ctx context.Context, c config.Settings, args []string, w io.Writer) (err error) {
	// No device discovery, profile reads or device status queries before consent.
	if c.PhysicalRead == nil || len(args) < 3 || args[len(args)-2] != "--confirm-read-only" || args[len(args)-1] != "--confirm-write-protected" {
		return errors.New("requires configured physical_read profile and explicit read-only/write-protection confirmation")
	}
	approval := linux.OperatorApproval{ConfirmReadOnly: true, ConfirmWriteProtect: true}
	switch args[0] {
	case "physical-doctor":
		if len(args) != 3 {
			return errors.New(usage)
		}
		report, e := linux.Doctor(ctx, *c.PhysicalRead, approval)
		if e != nil {
			return e
		}
		return json.NewEncoder(w).Encode(report)
	case "physical-snapshot":
		if len(args) != 6 {
			return errors.New(usage)
		}
		target := c.LegacyRoots[args[1]]
		if target == "" {
			return errors.New("unknown legacy root ID")
		}
		files, e := strconv.Atoi(args[2])
		if e != nil || files < 1 || files > 100000 {
			return errors.New("invalid file limit")
		}
		size, e := strconv.ParseInt(args[3], 10, 64)
		if e != nil || size < 1 {
			return errors.New("invalid byte limit")
		}
		lease, e := linux.AcquireReadOnly(ctx, *c.PhysicalRead, approval)
		if e != nil {
			return e
		}
		report, copyErr := linux.Snapshot(ctx, lease, target, linux.SnapshotLimits{MaxFiles: files, MaxBytes: size, ReserveBytes: c.SpoolReserveBytes})
		closeErr := lease.Close()
		return errors.Join(copyErr, closeErr, json.NewEncoder(w).Encode(report))
	}
	return errors.New(usage)
}
