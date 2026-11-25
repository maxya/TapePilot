package api

import (
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/catalog"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/store"
	"net/http"
)

func (c *console) inventoryRoutes(m *http.ServeMux) {
	route(m, "PATCH /api/v1/volumes/{id}", 200, func(w http.ResponseWriter, q *http.Request) (any, error) {
		var p struct {
			Barcode          *string `json:"barcode"`
			PreviousBarcode  *string `json:"previous_barcode"`
			Label            string  `json:"label"`
			Location         string  `json:"location"`
			PreviousLabel    string  `json:"previous_label"`
			PreviousLocation string  `json:"previous_location"`
		}
		if body(w, q, &p) != nil {
			return nil, ErrInput
		}
		id := q.PathValue("id")
		var editErr error
		if p.Barcode != nil {
			if p.PreviousBarcode == nil {
				return nil, ErrInput
			}
			editErr = c.r.Store.EditInventoryMetadata(q.Context(), id, p.PreviousLabel, p.PreviousLocation, *p.PreviousBarcode, p.Label, p.Location, *p.Barcode)
		} else {
			editErr = c.r.Store.EditInventory(q.Context(), id, p.PreviousLabel, p.PreviousLocation, p.Label, p.Location)
		}
		if editErr != nil {
			return nil, editErr
		}

		return c.r.Store.GetVolume(q.Context(), id)
	})
	route(m, "POST /api/v1/volumes", 201, func(w http.ResponseWriter, q *http.Request) (any, error) {
		var p struct {
			Barcode  string `json:"barcode"`
			Label    string `json:"label"`
			Location string `json:"location"`
		}
		if body(w, q, &p) != nil || len(p.Label) == 0 || len(p.Label) > 256 || len(p.Location) > 256 || !store.ValidBarcode(p.Barcode) {
			return nil, ErrInput
		}
		v := store.Volume{ID: uuid.NewString(), Kind: "MANAGED", State: domain.Unclassified, Label: p.Label, Location: p.Location, Barcode: p.Barcode}
		if e := c.r.Store.Audit(q.Context(), "INVENTORY_CREATED", v.ID); e != nil {
			return nil, e
		}
		if e := c.r.Store.CreateVolume(q.Context(), v); e != nil {
			return nil, e
		}
		return c.r.Store.GetVolume(q.Context(), v.ID)
	})
	route(m, "POST /api/v1/imports", 202, func(w http.ResponseWriter, q *http.Request) (any, error) {
		var p catalog.ImportOptions
		if body(w, q, &p) != nil {
			return nil, ErrInput
		}
		switch p.Mode {
		case "managed":
			if p.ExpectedVolumeID != "" {
				if _, e := uuid.Parse(p.ExpectedVolumeID); e != nil {
					return nil, ErrInput
				}
			}
			if p.SourceID != "" || p.Profile != "" || p.Label != "" || p.VolumeID != "" || p.Confirmed {
				return nil, ErrInput
			}
		case "legacy":
			if !p.Confirmed || p.Profile != legacy.SimulatorProfile || c.r.Settings.LegacyRoots[p.SourceID] == "" || len(p.Label) == 0 || len(p.Label) > 256 || p.ExpectedVolumeID != "" {
				return nil, ErrInput
			}
		default:
			return nil, ErrInput
		}
		return c.enqueue(q, domain.Import, "", p)
	})
}
