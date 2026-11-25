package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
)

var ErrOperations = errors.New("INVALID_OPERATION_SETTINGS")
var ErrProfileDisabled = errors.New("PROFILE_DISABLED")
var ErrOutstanding = errors.New("PROFILE_JOB_OUTSTANDING")
var operationID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var auditAction = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var auditObject = regexp.MustCompile(`^[A-Za-z0-9_-]{0,128}$`)

func plainLabel(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func relativeSelection(s string) bool {
	return s == "" || s != "." && len(s) <= 4096 && fs.ValidPath(s) && !strings.ContainsRune(s, 0)
}

type BackupProfile struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	SourceID          string   `json:"source_id"`
	RelativeSelection string   `json:"relative_selection"`
	Exclusions        []string `json:"exclusions"`
	TargetVolumeID    string   `json:"target_volume_id"`
	Enabled           bool     `json:"enabled"`
	Revision          int64    `json:"revision"`
	UpdatedAt         string   `json:"updated_at"`
}

func ValidBackupProfile(p BackupProfile) error {
	if !plainLabel(p.Name) || !operationID.MatchString(p.SourceID) || !relativeSelection(p.RelativeSelection) || len(p.Exclusions) > 256 {
		return ErrOperations
	}
	if _, e := uuid.Parse(p.TargetVolumeID); e != nil {
		return ErrOperations
	}
	for _, x := range p.Exclusions {
		if x == "" || !relativeSelection(x) {
			return ErrOperations
		}
	}
	if _, e := profileJobOptions(p, strings.Repeat("0", 64)); e != nil {
		return e
	}
	return nil
}

const profileColumns = "id,name,source_id,relative_selection,exclusions_json,target_volume_id,enabled,revision,updated_at"

func scanProfile(row scanner) (p BackupProfile, e error) {
	var excluded string
	e = row.Scan(&p.ID, &p.Name, &p.SourceID, &p.RelativeSelection, &excluded, &p.TargetVolumeID, &p.Enabled, &p.Revision, &p.UpdatedAt)
	if e == nil {
		e = json.Unmarshal([]byte(excluded), &p.Exclusions)
	}
	if p.Exclusions == nil {
		p.Exclusions = []string{}
	}
	return
}
func (s *Store) CreateBackupProfile(ctx context.Context, p BackupProfile, actor string) (BackupProfile, error) {
	if e := ValidBackupProfile(p); e != nil {
		return p, e
	}
	if p.ID != "" || p.Revision != 0 || p.UpdatedAt != "" {
		return p, ErrOperations
	}
	p.ID = uuid.NewString()
	p.Revision = 1
	p.UpdatedAt = stamp()
	if p.Exclusions == nil {
		p.Exclusions = []string{}
	}
	excluded, _ := json.Marshal(p.Exclusions)
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		var kind string
		if e := tx.QueryRowContext(ctx, "SELECT kind FROM volumes WHERE id=?", p.TargetVolumeID).Scan(&kind); e != nil {
			return e
		}
		if kind != "MANAGED" {
			return ErrOperations
		}
		var count int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM backup_profiles").Scan(&count); e != nil {
			return e
		}
		if count >= 1000 {
			return ErrOperations
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO backup_profiles VALUES(?,?,?,?,?,?,?,?,?,?)", p.ID, p.Name, p.SourceID, p.RelativeSelection, string(excluded), p.TargetVolumeID, p.Enabled, p.Revision, p.UpdatedAt, p.UpdatedAt); e != nil {
			return e
		}
		return insertAudit(ctx, tx, actor, "PROFILE_CREATED", p.ID)
	})
	return p, e
}
func (s *Store) BackupProfile(ctx context.Context, id string) (BackupProfile, error) {
	return scanProfile(s.db.QueryRowContext(ctx, "SELECT "+profileColumns+" FROM backup_profiles WHERE id=?", id))
}
func (s *Store) ListBackupProfiles(ctx context.Context) ([]BackupProfile, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT "+profileColumns+" FROM backup_profiles ORDER BY name,id LIMIT 1000")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []BackupProfile{}
	for rows.Next() {
		p, e := scanProfile(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) UpdateBackupProfile(ctx context.Context, p BackupProfile, actor string) (BackupProfile, error) {
	if e := ValidBackupProfile(p); e != nil {
		return p, e
	}
	if p.Revision < 1 || p.Revision == int64(^uint64(0)>>1) {
		return p, ErrOperations
	}
	excluded, _ := json.Marshal(p.Exclusions)
	p.UpdatedAt = stamp()
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		var kind string
		if e := tx.QueryRowContext(ctx, "SELECT kind FROM volumes WHERE id=?", p.TargetVolumeID).Scan(&kind); e != nil {
			return e
		}
		if kind != "MANAGED" {
			return ErrOperations
		}
		result, e := tx.ExecContext(ctx, "UPDATE backup_profiles SET name=?,source_id=?,relative_selection=?,exclusions_json=?,target_volume_id=?,enabled=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?", p.Name, p.SourceID, p.RelativeSelection, string(excluded), p.TargetVolumeID, p.Enabled, p.UpdatedAt, p.ID, p.Revision)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		return insertAudit(ctx, tx, actor, "PROFILE_UPDATED", p.ID)
	})
	if e == nil {
		p.Revision++
	}
	return p, e
}
func insertAudit(ctx context.Context, tx *sql.Tx, actor, action, object string) error {
	if actor != "admin" && actor != "scheduler" && actor != "system" || !auditAction.MatchString(action) || !auditObject.MatchString(object) {
		return ErrOperations
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO audit_events(actor,action,object,created_at) VALUES(?,?,?,?)", actor, action, object, stamp())
	return e
}
func (s *Store) Audit(ctx context.Context, action, object string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error { return insertAudit(ctx, tx, "admin", action, object) })
}

type AuditEntry struct {
	ID        int64  `json:"id"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Object    string `json:"object"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) ListAudit(ctx context.Context, after int64, limit int) ([]AuditEntry, error) {
	if after < 0 || limit < 1 || limit > 200 {
		return nil, ErrOperations
	}
	rows, e := s.db.QueryContext(ctx, "SELECT id,actor,action,object,created_at FROM audit_events WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		if e = rows.Scan(&a.ID, &a.Actor, &a.Action, &a.Object, &a.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type Notification struct {
	ID        int64   `json:"id"`
	JobID     string  `json:"job_id"`
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	CreatedAt string  `json:"created_at"`
	ReadAt    *string `json:"read_at"`
}

func (s *Store) ListNotifications(ctx context.Context, after int64, limit int) ([]Notification, error) {
	if after < 0 || limit < 1 || limit > 200 {
		return nil, ErrOperations
	}
	rows, e := s.db.QueryContext(ctx, "SELECT id,COALESCE(job_id,''),code,message,created_at,read_at FROM notifications WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var read sql.NullString
		if e = rows.Scan(&n.ID, &n.JobID, &n.Code, &n.Message, &n.CreatedAt, &read); e != nil {
			return nil, e
		}
		if read.Valid {
			n.ReadAt = &read.String
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func (s *Store) ReadNotification(ctx context.Context, id int64) error {
	if id < 1 {
		return ErrOperations
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, e := tx.ExecContext(ctx, "UPDATE notifications SET read_at=COALESCE(read_at,?) WHERE id=?", stamp(), id)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrNotFound
		}
		return insertAudit(ctx, tx, "admin", "NOTIFICATION_READ", notificationObject(id))
	})
}
func notificationObject(id int64) string { return "notification_" + strconv.FormatInt(id, 10) }
func (s *Store) EnqueueProfile(ctx context.Context, id string, revision int64, actor string) (Job, error) {
	return s.EnqueueManualProfile(ctx, id, revision, uuid.NewString(), "", actor)
}
func (s *Store) EnqueueManualProfile(ctx context.Context, id string, revision int64, key, planDigest, actor string) (job Job, err error) {
	if key == "" || len(key) > 128 || planDigest != "" && !digest(planDigest) {
		return job, ErrOperations
	}
	hash := sha256.Sum256([]byte(actor + "\x00" + key))
	dedupe := "manual-profile:" + id + ":" + hex.EncodeToString(hash[:])
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		p, e := scanProfile(tx.QueryRowContext(ctx, "SELECT "+profileColumns+" FROM backup_profiles WHERE id=?", id))
		if e != nil {
			return e
		}
		if p.Revision != revision {
			return ErrConflict
		}
		var e2 error
		job, _, e2 = enqueueProfileTx(ctx, tx, p, dedupe, planDigest, actor)
		return e2
	})
	return
}
func profileJobOptions(p BackupProfile, planDigest string) ([]byte, error) {
	options := struct {
		SourceID    string   `json:"source_id"`
		Selection   string   `json:"selection"`
		Exclusions  []string `json:"exclusions"`
		SourceLabel string   `json:"source_label"`
		PlanDigest  string   `json:"plan_digest,omitempty"`
	}{p.SourceID, p.RelativeSelection, p.Exclusions, p.Name, planDigest}
	raw, e := json.Marshal(options)
	if e != nil {
		return nil, e
	}
	if len(raw) > 16384 {
		return nil, ErrOperations
	}
	return raw, nil
}
func enqueueProfileTx(ctx context.Context, tx *sql.Tx, p BackupProfile, dedupe, planDigest, actor string) (Job, bool, error) {
	return enqueueProfileOperationTx(ctx, tx, p, domain.Backup, dedupe, planDigest, actor)
}
func enqueueProfileOperationTx(ctx context.Context, tx *sql.Tx, p BackupProfile, operation domain.Kind, dedupe, planDigest, actor string) (Job, bool, error) {
	var job Job
	var raw []byte
	var e error
	if operation == domain.Verify {
		raw = []byte(`{}`)
	} else if operation == domain.Backup {
		raw, e = profileJobOptions(p, planDigest)
	} else {
		return job, false, ErrOperations
	}
	if e != nil {
		return job, false, e
	}
	previous, e := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE dedupe_key=?", dedupe))
	if e == nil {
		var previousRevision int64
		if e = tx.QueryRowContext(ctx, "SELECT profile_revision FROM job_profiles WHERE job_id=?", previous.ID).Scan(&previousRevision); e != nil {
			return job, false, e
		}
		if previousRevision != p.Revision {
			return job, false, ErrConflict
		}
		if previous.Kind != operation || previous.VolumeID != p.TargetVolumeID || string(previous.Options) != string(raw) {
			return job, false, ErrConflict
		}
		return previous, false, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return job, false, e
	}
	if !p.Enabled {
		return job, false, ErrProfileDisabled
	}
	var count int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_profiles p JOIN jobs j ON j.id=p.job_id WHERE p.profile_id=? AND j.state NOT IN('SUCCEEDED','FAILED','CANCELLED','RECOVERY_REQUIRED')", p.ID).Scan(&count); e != nil {
		return job, false, e
	}
	if count != 0 {
		return job, false, ErrOutstanding
	}
	job = Job{ID: uuid.NewString(), Kind: operation, State: domain.Queued, VolumeID: p.TargetVolumeID, Options: raw, DedupeKey: dedupe, CreatedAt: stamp()}
	if _, e = tx.ExecContext(ctx, "INSERT INTO jobs(id,kind,state,volume_id,options_json,dedupe_key,created_at) VALUES(?,?,?,?,?,?,?)", job.ID, job.Kind, job.State, job.VolumeID, string(raw), dedupe, job.CreatedAt); e != nil {
		return job, false, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO job_profiles VALUES(?,?,?)", job.ID, p.ID, p.Revision); e != nil {
		return job, false, e
	}
	if e = event(ctx, tx, job.ID, "QUEUED", "Profile job queued"); e != nil {
		return job, false, e
	}
	if e = insertAudit(ctx, tx, actor, "PROFILE_ENQUEUED", p.ID); e != nil {
		return job, false, e
	}
	return job, true, nil
}
