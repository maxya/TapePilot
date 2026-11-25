package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"github.com/maxya/TapePilot/internal/doctor"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type console struct {
	r          *app.Runtime
	svc        app.Services
	auth       *auth.Auth
	expensive  chan struct{}
	streams    chan struct{}
	exportSlot chan struct{}
	exports    string
}
type endpoint func(http.ResponseWriter, *http.Request) (any, error)

func route(m *http.ServeMux, pattern string, status int, fn endpoint) {
	m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(w, r)
		if err != nil {
			fail(w, r, err)
			return
		}
		reply(w, status, v)
	})
}
func NewConsole(r *app.Runtime, svc app.Services) (http.Handler, error) {
	guard, err := auth.New(r.Store, r.Settings.PublicOrigin)
	if err != nil {
		return nil, err
	}
	c := &console{r: r, svc: svc, auth: guard, expensive: make(chan struct{}, 2), streams: make(chan struct{}, 8), exportSlot: make(chan struct{}, 1), exports: filepath.Join(r.Settings.StateDir, "exports")}
	if err = os.MkdirAll(c.exports, 0700); err != nil {
		return nil, err
	}
	m := http.NewServeMux()
	m.Handle("GET /api/v1/health", Handler())
	route(m, "GET /api/v1/system/info", 200, c.info)
	route(m, "GET /api/v1/system/doctor", 200, func(_ http.ResponseWriter, q *http.Request) (any, error) {
		return doctor.Inspect(q.Context(), r.Settings), nil
	})
	route(m, "GET /api/v1/drives", 200, c.drives)
	route(m, "GET /api/v1/drives/{id}", 200, c.drives)
	route(m, "POST /api/v1/drives/{id}/insert", 200, c.insert)
	for _, op := range []string{"inspect", "unload", "rewind"} {
		route(m, "POST /api/v1/drives/{id}/"+op, 202, c.motion)
	}
	route(m, "GET /api/v1/sources", 200, c.sources)
	route(m, "GET /api/v1/sources/{id}/children", 200, c.children)
	route(m, "GET /api/v1/volumes", 200, func(_ http.ResponseWriter, q *http.Request) (any, error) {
		return r.Store.ListVolumes(q.Context(), 500)
	})
	route(m, "GET /api/v1/volumes/{id}", 200, func(_ http.ResponseWriter, q *http.Request) (any, error) {
		return r.Store.GetVolume(q.Context(), q.PathValue("id"))
	})
	for _, op := range []string{"import", "verify", "seal"} {
		route(m, "POST /api/v1/volumes/{id}/"+op, 202, c.volumeAction)
	}
	route(m, "GET /api/v1/archives", 200, c.archives)
	route(m, "GET /api/v1/archives/{id}", 200, func(_ http.ResponseWriter, q *http.Request) (any, error) {
		return r.Store.GetArchive(q.Context(), q.PathValue("id"))
	})
	route(m, "GET /api/v1/archives/{id}/members", 200, c.members)
	route(m, "GET /api/v1/catalog/search", 200, c.members)
	m.HandleFunc("GET /api/v1/volumes/{id}/recovery-descriptor", c.descriptor)
	route(m, "POST /api/v1/exports", 201, c.export)
	m.HandleFunc("GET /api/v1/exports/{id}", c.download)
	c.jobRoutes(m)
	c.inventoryRoutes(m)
	RegisterOperations(m, r)
	return guard.Wrap(m), nil
}
func keys(m map[string]string) []string {
	a := make([]string, 0, len(m))
	for k := range m {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}
func (c *console) info(_ http.ResponseWriter, q *http.Request) (any, error) {
	return map[string]any{"mode": "simulator", "physical_writes_enabled": false, "qualified_profiles": 0, "restore_roots": keys(c.r.Settings.RestoreRoots), "legacy_roots": keys(c.r.Settings.LegacyRoots)}, nil
}
func (c *console) sources(_ http.ResponseWriter, q *http.Request) (any, error) {
	rows := []map[string]string{}
	for _, id := range keys(c.r.Settings.Sources) {
		rows = append(rows, map[string]string{"id": id})
	}
	return rows, nil
}
func (c *console) children(_ http.ResponseWriter, q *http.Request) (any, error) {
	rootPath, ok := c.r.Settings.Sources[q.PathValue("id")]
	if !ok {
		return nil, store.ErrNotFound
	}
	return source.Browse(rootPath, q.URL.Query().Get("path"))
}
func (c *console) drives(_ http.ResponseWriter, q *http.Request) (any, error) {
	id := q.PathValue("id")
	if id != "" && id != "simulator" {
		return nil, store.ErrNotFound
	}
	row := map[string]any{"id": "simulator", "mode": "simulator", "status": "empty"}
	l, err := c.r.Drive.Acquire(q.Context())
	switch {
	case err == nil:
		s, e := l.Status(q.Context())
		if e = errors.Join(e, l.Close()); e != nil {
			return nil, e
		}
		row["status"] = "loaded"
		row["cartridge_id"] = s.CartridgeID
		row["capacity_bytes"] = s.CapacityBytes
		row["used_bytes"] = s.UsedBytes
		row["write_protected"] = s.WriteProtected
		row["uncertain"] = s.Uncertain
	case errors.Is(err, sim.ErrBusy):
		row["status"] = "busy"
	case errors.Is(err, sim.ErrNoMedia):
	default:
		return nil, err
	}
	if id != "" {
		return row, nil
	}
	return []any{row}, nil
}
func (c *console) insert(w http.ResponseWriter, q *http.Request) (any, error) {
	var req struct {
		ID string `json:"cartridge_id"`
	}
	if q.PathValue("id") != "simulator" || body(w, q, &req) != nil || len(req.ID) > 64 || req.ID == "" {
		return nil, ErrInput
	}
	if err := c.r.Store.Audit(q.Context(), "SIM_INSERT", "simulator"); err != nil {
		return nil, err
	}
	if err := c.r.Insert(req.ID); err != nil {
		return nil, store.ErrConflict
	}
	return map[string]string{"status": "inserted"}, nil
}
func (c *console) motion(w http.ResponseWriter, q *http.Request) (any, error) {
	if q.PathValue("id") != "simulator" || body(w, q, &struct{}{}) != nil {
		return nil, ErrInput
	}
	return c.enqueue(q, domain.Kind(strings.ToUpper(filepath.Base(q.URL.Path))), "", struct{}{})
}
func (c *console) volumeAction(w http.ResponseWriter, q *http.Request) (any, error) {
	id := q.PathValue("id")
	if _, e := uuid.Parse(id); e != nil || body(w, q, &struct{}{}) != nil {
		return nil, ErrInput
	}
	switch filepath.Base(q.URL.Path) {
	case "seal":
		if err := c.r.Store.Audit(q.Context(), "VOLUME_SEAL", id); err != nil {
			return nil, err
		}
		return map[string]string{"state": "SEALED"}, c.r.Store.SetVolumeState(q.Context(), id, domain.Sealed)
	case "import":
		return c.enqueue(q, domain.Import, id, map[string]string{"mode": "managed", "expected_volume_id": id})
	default:
		return c.enqueue(q, domain.Verify, id, struct{}{})
	}
}
func (c *console) enqueue(q *http.Request, kind domain.Kind, volume string, options any) (any, error) {
	key := q.Header.Get("Idempotency-Key")
	if _, e := uuid.Parse(key); e != nil {
		return nil, ErrInput
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	identity := store.RequestIdentity{Actor: "admin", Route: q.Method + " " + q.URL.Path, Key: key, Request: raw}
	j, err := c.r.Store.EnqueueHTTPRequest(q.Context(), identity, store.Job{Kind: kind, VolumeID: volume, Options: raw})
	if err == nil {
		c.svc.Scheduler.Wake()
	}
	return j, err
}

type cursor struct {
	Value string
	ID    int64
	UUID  string
}

func getCursor(q *http.Request) (cursor, error) {
	var c cursor
	s := q.URL.Query().Get("cursor")
	if s == "" {
		return c, nil
	}
	if len(s) > 8192 {
		return c, ErrInput
	}
	raw, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil || json.Unmarshal(raw, &c) != nil || c.ID < 0 {
		return c, ErrInput
	}
	return c, nil
}
func nextCursor(w http.ResponseWriter, c cursor) {
	raw, _ := json.Marshal(c)
	w.Header().Set("X-Next-Cursor", base64.RawURLEncoding.EncodeToString(raw))
}
func (c *console) archives(w http.ResponseWriter, q *http.Request) (any, error) {
	cur, e := getCursor(q)
	if e != nil {
		return nil, e
	}
	v := q.URL.Query()
	limit, e := pageNumber(v.Get("limit"), 100, 500)
	if e != nil || limit == 0 {
		return nil, ErrInput
	}
	rows, e := c.r.Store.ListArchives(q.Context(), store.ArchiveQuery{VolumeID: v.Get("volume_id"), Limit: int(limit), AfterCreatedAt: cur.Value, AfterID: cur.UUID})
	if e == nil && len(rows) == int(limit) {
		last := rows[len(rows)-1]
		nextCursor(w, cursor{Value: last.CreatedAt, UUID: last.ID})
	}
	return rows, e
}
func (c *console) members(w http.ResponseWriter, q *http.Request) (any, error) {
	cur, e := getCursor(q)
	if e != nil {
		return nil, e
	}
	v := q.URL.Query()
	limit, e := pageNumber(v.Get("limit"), 100, 500)
	if e != nil || limit == 0 {
		return nil, ErrInput
	}
	a := q.PathValue("id")
	if a == "" {
		a = v.Get("archive_id")
	}
	sq := store.MemberQuery{Text: v.Get("q"), VolumeID: v.Get("volume_id"), VolumeLabel: v.Get("volume_label"), ArchiveID: a, SourceLabel: v.Get("source_label"), JobID: v.Get("job_id"), CreatedAfter: v.Get("created_after"), CreatedBefore: v.Get("created_before"), Limit: int(limit), AfterPath: cur.Value, AfterID: cur.ID}
	if v.Has("parent_path") {
		p := v.Get("parent_path")
		sq.ParentPath = &p
	}
	rows, e := c.r.Store.SearchMembers(q.Context(), sq)
	if e == nil && len(rows) == int(limit) {
		last := rows[len(rows)-1]
		nextCursor(w, cursor{Value: last.Path, ID: last.ID})
	}
	return rows, e
}
func (c *console) descriptor(w http.ResponseWriter, q *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if e := c.r.Store.WriteDescriptor(q.Context(), q.PathValue("id"), w); e != nil {
		fail(w, q, e)
	}
}
func (c *console) export(w http.ResponseWriter, q *http.Request) (any, error) {
	var req struct {
		Kind   string `json:"kind"`
		Volume string `json:"volume_id"`
	}
	if body(w, q, &req) != nil {
		return nil, ErrInput
	}
	select {
	case c.exportSlot <- struct{}{}:
		defer func() { <-c.exportSlot }()
	default:
		return nil, store.ErrConflict
	}
	if _, e := c.r.Store.PurgeExpiredExports(q.Context(), c.exports); e != nil {
		return nil, e
	}
	count, e := c.r.Store.ActiveExports(q.Context())
	if e != nil {
		return nil, e
	}
	if count >= 16 {
		return nil, store.ErrConflict
	}
	switch req.Kind {
	case "database":
		if req.Volume != "" {
			return nil, ErrInput
		}
	case "catalog", "descriptor", "recovery-text":
		if _, err := uuid.Parse(req.Volume); err != nil {
			return nil, ErrInput
		}
	default:
		return nil, ErrInput
	}
	if e := c.r.Store.Audit(q.Context(), "CREATE_EXPORT", req.Kind); e != nil {
		return nil, e
	}
	return c.r.Store.CreateExport(q.Context(), c.exports, req.Kind, req.Volume, 15*time.Minute)
}
func (c *console) download(w http.ResponseWriter, q *http.Request) {
	f, e, err := c.r.Store.OpenExport(q.Context(), c.exports, q.PathValue("id"))
	if err != nil {
		fail(w, q, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", `attachment; filename="`+e.Filename+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, q, e.Filename, time.Time{}, f)
}
