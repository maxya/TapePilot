package integration_test

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/stream"
	"github.com/maxya/TapePilot/internal/tape"
	"github.com/maxya/TapePilot/internal/tape/sim"
)

// Exercise the P1 layers together using only a fresh synthetic cartridge.
// This low-level fixture deliberately ends after data. It must remain readable
// yet ineligible for append on the next lease.
func TestGNUStreamThroughSimulator(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "-example\nfile"), []byte("TapePilot"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := source.Create(ctx, []source.Root{{ID: "test", Path: root}}, "test", "", t.TempDir(), nil, source.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	d, err := sim.Open(filepath.Join(t.TempDir(), "drive"))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Create("synthetic", 16*tape.RecordBytes, false); err != nil {
		t.Fatal(err)
	}
	if err = d.Insert("synthetic"); err != nil {
		t.Fatal(err)
	}
	lease, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := media.Volume{Magic: tape.VolumeMagic, FormatMajor: 1, VolumeID: uuid.NewString(), LabelAtInitialization: "synthetic", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), CreatedByVersion: "development", MediaGeneration: "simulator", TapeProfile: media.StandardProfile(), ArchiveFormat: "pax-tar", HashAlgorithm: "sha256"}
	label, err := media.EncodeVolume(v)
	if err != nil {
		t.Fatal(err)
	}
	p, err := lease.AuthorizeInitialization(ctx, uuid.NewString(), true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := lease.OpenWriter(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(w.WriteRecord(ctx, label), w.CloseAndFlush()); err != nil {
		t.Fatal(err)
	}
	if err = lease.Rewind(ctx); err != nil {
		t.Fatal(err)
	}
	rr, err := lease.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observed, header, err := media.ReadVolume(media.NewFileReader(ctx, rr))
	if err != nil {
		t.Fatal(err)
	}
	if observed.VolumeID != v.VolumeID {
		t.Fatal("wrong volume")
	}
	sum := sha256.Sum256(label)
	if hex.EncodeToString(sum[:]) != header.SHA256 {
		t.Fatal("header digest mismatch")
	}
	if _, err = io.Copy(io.Discard, media.NewFileReader(ctx, rr)); !errors.Is(err, media.ErrEOD) {
		t.Fatal("expected EOD", err)
	}
	if err = rr.Close(); err != nil {
		t.Fatal(err)
	}
	obs, err := lease.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proof := tape.AppendProof{Observation: obs, VolumeID: v.VolumeID, HeaderDigest: sum, NextIndex: 1}
	if err = lease.PositionAppend(ctx, proof); err != nil {
		t.Fatal(err)
	}
	p, err = lease.AuthorizeAppend(ctx, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	w, err = lease.OpenWriter(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := stream.Produce(ctx, plan, w)
	if err = errors.Join(err, w.CloseAndFlush()); err != nil {
		t.Fatal(err)
	}
	if err = lease.PositionFile(ctx, 1); err != nil {
		t.Fatal(err)
	}
	rr, err = lease.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members := 0
	readback, err := media.Scan(media.NewFileReader(ctx, rr), func(h *tar.Header) error {
		members++
		if h.Name != "-example\nfile" {
			t.Fatal(h.Name)
		}
		return nil
	})
	if err = errors.Join(err, rr.Close()); err != nil {
		t.Fatal(err)
	}
	if members != 1 || readback.Bytes != expected.Bytes || readback.SHA256 != hex.EncodeToString(expected.SHA256[:]) {
		t.Fatal(readback)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if p, err = next.AuthorizeCommit(ctx, uuid.NewString()); err == nil || p != nil {
		t.Fatal("reissued commit after lease loss")
	}
}
