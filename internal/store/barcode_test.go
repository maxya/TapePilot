package store

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"os"
	"strings"
	"testing"
)

func TestOptionalBarcodePersistsOfflineAndUsesCAS(t *testing.T) {
	s, dir := openTest(t)
	id := uuid.NewString()
	if err := s.CreateVolume(ctx, Volume{ID: id, Kind: "MANAGED", Label: "Unobserved label", State: domain.Unclassified, Barcode: "LTO123L5"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetVolume(ctx, id)
	if err != nil || loaded.Barcode != "LTO123L5" {
		t.Fatal(loaded, err)
	}
	if err = s.EditInventoryMetadata(ctx, id, loaded.Label, loaded.Location, loaded.Barcode, "New label", "Shelf 1", "LTO456L5"); err != nil {
		t.Fatal(err)
	}
	if err = s.EditInventoryMetadata(ctx, id, loaded.Label, loaded.Location, loaded.Barcode, "Stale update", "", "bad"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale barcode edit accepted", err)
	}
	if err = s.EditInventory(ctx, id, "New label", "Shelf 1", "Renamed", "Shelf 2"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err = reopened.GetVolume(ctx, id)
	if err != nil || loaded.Barcode != "LTO456L5" || loaded.State != domain.Unclassified || loaded.HeaderSHA256 != "" {
		t.Fatal("barcode affected identity or protection", loaded, err)
	}
	if err = reopened.EnrollVolume(ctx, Enrollment{VolumeID: id, DriveID: "synthetic", Profile: "simulator", OperatorRef: "operator"}); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal("barcode authorized an unobserved volume", err)
	}
	for _, bad := range []string{strings.Repeat("x", 129), "ABC\nDEF", string([]byte{0xff}), "ABC\x00DEF"} {
		if err = reopened.EditInventoryMetadata(ctx, id, loaded.Label, loaded.Location, loaded.Barcode, loaded.Label, loaded.Location, bad); !errors.Is(err, ErrOperations) {
			t.Fatal("bad barcode accepted", err)
		}
	}
}
func TestBarcodeSurvivesManagedReimport(t *testing.T) {
	s, _ := openTest(t)
	id := volume(t, s)
	loaded, _ := s.GetVolume(ctx, id)
	if err := s.EditInventoryMetadata(ctx, id, loaded.Label, loaded.Location, "", loaded.Label, loaded.Location, "ABC123L5"); err != nil {
		t.Fatal(err)
	}
	j := job(t, s, domain.Import, "")
	j, _ = s.Claim(ctx)
	j = advance(t, s, j, domain.Running)
	raw, err := os.ReadFile("../../schemas/fixtures/volume.json")
	if err != nil {
		t.Fatal(err)
	}
	var label media.Volume
	if err = json.Unmarshal(raw, &label); err != nil {
		t.Fatal(err)
	}
	label.VolumeID = id
	raw, err = json.Marshal(label)
	if err != nil {
		t.Fatal(err)
	}
	imported := loaded
	imported.Barcode = "SHOULD-NOT-REPLACE"
	if err = s.RegisterImportedVolume(ctx, j.ID, imported, raw); err != nil {
		t.Fatal(err)
	}

	actual, err := s.GetVolume(ctx, id)
	if err != nil || actual.Barcode != "ABC123L5" {
		t.Fatal(actual, err)
	}
}
