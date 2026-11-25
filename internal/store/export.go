package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Descriptor returns an internally consistent catalog snapshot, bounded to
// 100,000 completed archives per volume. Inventory streams separately as NDJSON.
func (s *Store) Descriptor(ctx context.Context, volume string) (media.Descriptor, error) {
	var result media.Descriptor
	err := s.readSnapshot(ctx, func(tx *sql.Tx) error {
		var metadata, header, kind string
		if err := tx.QueryRowContext(ctx, "SELECT m.metadata_json,v.header_sha256,v.kind FROM volumes v JOIN volume_metadata m ON m.volume_id=v.id WHERE v.id=?", volume).Scan(&metadata, &header, &kind); err != nil {
			return err
		}
		if kind != "MANAGED" {
			return ErrEvidence
		}
		var label media.Volume
		if err := json.Unmarshal([]byte(metadata), &label); err != nil {
			return err
		}
		if label.VolumeID != volume {
			return ErrEvidence
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+archiveColumns+" FROM archives WHERE volume_id=? ORDER BY sequence LIMIT 100001", volume)
		if err != nil {
			return err
		}
		defer rows.Close()
		entries := []media.ArchiveEntry{}
		var suffix string
		for rows.Next() {
			a, err := scanArchive(rows)
			if err != nil {
				return err
			}
			if len(entries) == 100000 {
				return ErrEvidence
			}
			if a.Sequence != int64(len(entries))+1 {
				return ErrEvidence
			}
			if a.Integrity == domain.IntegrityFailed || a.Integrity == domain.IntegrityUnknown {
				suffix = fmt.Sprintf("Archive sequence %d has current integrity %s. This descriptor includes only the preceding verified prefix. Inspect the remaining files read-only.", a.Sequence, a.Integrity)
				break
			}
			if a.Integrity != domain.Verified || a.CommitIndex == nil {
				return ErrEvidence
			}
			verified := a.VerifiedAt
			entries = append(entries, media.ArchiveEntry{ArchiveID: a.ID, Sequence: a.Sequence, DataFileIndex: a.DataIndex, CommitFileIndex: *a.CommitIndex, DataStreamBytes: a.StreamBytes, DataSHA256: a.SHA256, CommitSHA256: a.CommitSHA256, SourceLabel: a.SourceLabel, CreatedAt: a.CreatedAt, VerifiedAt: &verified, IntegrityBasis: a.IntegrityBasis})
		}
		if err = rows.Err(); err != nil {
			return err
		}
		result, err = media.SimulatorDescriptor(label, header, entries, time.Now())
		if err != nil {
			return err
		}
		if suffix != "" {
			result.Caveats = append(result.Caveats, suffix)
		}
		result.HardwareProfile = "unqualified"
		result.HardwareProfileVersion = "unqualified"
		var profile string
		profileErr := tx.QueryRowContext(ctx, "SELECT profile FROM volume_enrollments WHERE volume_id=?", volume).Scan(&profile)
		if profileErr == nil {
			result.HardwareProfile = profile
		} else if !errors.Is(profileErr, sql.ErrNoRows) {
			return profileErr
		}
		return nil
	})
	return result, err
}
func (s *Store) WriteDescriptor(ctx context.Context, volume string, w io.Writer) error {
	v, err := s.GetVolume(ctx, volume)
	if err != nil {
		return err
	}
	if v.Kind == "LEGACY" {
		d, err := s.LegacyDescriptor(ctx, volume)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(d)
	}
	d, err := s.Descriptor(ctx, volume)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(d)
}
func (s *Store) WriteRecoveryText(ctx context.Context, volume string, w io.Writer) error {
	v, err := s.GetVolume(ctx, volume)
	if err != nil {
		return err
	}
	if v.Kind == "LEGACY" {
		d, err := s.LegacyDescriptor(ctx, volume)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "Legacy TAR recovery descriptor\nVolume ID %s\nLabel %s\nRead profile %s\n\n%s\n\n", d.VolumeID, strconv.Quote(d.Label), strconv.Quote(d.ReadProfile), d.Instructions); err != nil {
			return err
		}
		for _, a := range d.Archives {
			if _, err = fmt.Fprintf(w, "Archive %s, logical file %d, bytes %d\nObserved SHA-256 %s\nIntegrity basis %s\n\n", a.ArchiveID, a.DataFileIndex, a.Bytes, a.SHA256, a.IntegrityBasis); err != nil {
				return err
			}
		}
		return nil
	}
	d, err := s.Descriptor(ctx, volume)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "TapePilot recovery descriptor\nVolume UUID %s\nImmutable label %s\nFormat %d.%d, record bytes %d, TAR blocking factor %d\nHeader SHA-256 %s\n\n%s\nStore this descriptor separately from cartridges. Physical profile is unqualified.\n\n", d.VolumeID, strconv.Quote(d.Label), d.FormatMajor, d.FormatMinor, d.Profile.RecordBytes, d.Profile.TarBlockingFactor, d.HeaderSHA256, d.Instructions); err != nil {
		return err
	}
	for _, caveat := range d.Caveats {
		if _, err = fmt.Fprintln(w, caveat); err != nil {
			return err
		}
	}
	for _, a := range d.Archives {
		if _, err = fmt.Fprintf(w, "Archive %s, data file %d, commit file %d, bytes %d\nData SHA-256 %s\nCommit SHA-256 %s\nIntegrity basis %s\n\n", a.ArchiveID, a.DataFileIndex, a.CommitFileIndex, a.DataStreamBytes, a.DataSHA256, a.CommitSHA256, a.IntegrityBasis); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) WriteCatalogNDJSON(ctx context.Context, volume string, w io.Writer) error {
	return s.readSnapshot(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT m.id,m.path,m.raw_path,m.parent_path,m.type,m.size,m.mtime,m.mode,m.link_target,a.id,a.volume_id,v.label,a.data_file_index,a.integrity_state,a.integrity_basis FROM members m JOIN archives a ON a.id=m.archive_id JOIN volumes v ON v.id=a.volume_id WHERE (?='' OR a.volume_id=?) ORDER BY a.volume_id,a.sequence,m.id", volume, volume)
		if err != nil {
			return err
		}
		defer rows.Close()
		encoder := json.NewEncoder(w)
		for rows.Next() {
			if err = ctx.Err(); err != nil {
				return err
			}
			var m CatalogMember
			if err = rows.Scan(&m.ID, &m.Path, &m.RawPath, &m.ParentPath, &m.Type, &m.Size, &m.Mtime, &m.Mode, &m.LinkTarget, &m.ArchiveID, &m.VolumeID, &m.VolumeLabel, &m.DataFileIndex, &m.Integrity, &m.IntegrityBasis); err != nil {
				return err
			}
			if err = encoder.Encode(m); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

type Export struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	VolumeID  string `json:"volume_id"`
	Filename  string `json:"filename"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
}

// CreateExport writes only generated filenames under a configured private root.
// TTL is restricted to one minute through one day. Exports contain private data.
func (s *Store) CreateExport(ctx context.Context, dir, kind, volume string, ttl time.Duration) (Export, error) {
	var out Export
	if ttl < time.Minute || ttl > 24*time.Hour {
		return out, ErrEvidence
	}
	if !filepath.IsAbs(dir) {
		return out, ErrEvidence
	}
	if err := privateDirectory(dir); err != nil {
		return out, err
	}
	ext := map[string]string{"database": ".db", "descriptor": ".json", "recovery-text": ".txt", "catalog": ".ndjson"}[kind]
	if ext == "" {
		return out, ErrEvidence
	}
	out = Export{ID: uuid.NewString(), Kind: kind, VolumeID: volume, CreatedAt: stamp(), ExpiresAt: time.Now().UTC().Add(ttl).Format("2006-01-02T15:04:05.000000000Z")}
	out.Filename = out.ID + ext
	path := filepath.Join(dir, out.Filename)
	complete := false
	defer func() {
		if !complete {
			os.Remove(path)
		}
	}()
	if kind == "database" {
		if err := s.Backup(ctx, path); err != nil {
			return Export{}, err
		}
	} else {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return Export{}, err
		}
		switch kind {
		case "descriptor":
			err = s.WriteDescriptor(ctx, volume, f)
		case "recovery-text":
			err = s.WriteRecoveryText(ctx, volume, f)
		case "catalog":
			err = s.WriteCatalogNDJSON(ctx, volume, f)
		}
		err = errors.Join(err, f.Sync(), f.Close())
		if err != nil {
			return Export{}, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return Export{}, err
	}
	h := sha256.New()
	out.Bytes, err = io.Copy(h, f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return Export{}, err
	}
	out.SHA256 = fmt.Sprintf("%x", h.Sum(nil))
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO exports VALUES(?,?,?,?,?,?,?,?)", out.ID, out.Kind, nullable(out.VolumeID), out.Filename, out.CreatedAt, out.ExpiresAt, out.Bytes, out.SHA256)
		return err
	})
	if err != nil {
		return Export{}, err
	}
	complete = true
	return out, nil
}
func (s *Store) OpenExport(ctx context.Context, dir, id string) (*os.File, Export, error) {
	var e Export
	if _, err := uuid.Parse(id); err != nil {
		return nil, e, ErrNotFound
	}
	if err := privateDirectory(dir); err != nil {
		return nil, e, err
	}
	err := s.db.QueryRowContext(ctx, "SELECT id,kind,COALESCE(volume_id,''),filename,created_at,expires_at,bytes,sha256 FROM exports WHERE id=?", id).Scan(&e.ID, &e.Kind, &e.VolumeID, &e.Filename, &e.CreatedAt, &e.ExpiresAt, &e.Bytes, &e.SHA256)
	if err != nil {
		return nil, e, err
	}
	if e.ExpiresAt <= stamp() {
		return nil, e, ErrExportExpired
	}
	if filepath.Base(e.Filename) != e.Filename || len(e.Filename) < 37 || len(e.Filename) > 64 || e.Filename[:36] != id {
		return nil, e, ErrEvidence
	}
	f, err := os.OpenFile(filepath.Join(dir, e.Filename), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, e, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != e.Bytes {
		f.Close()
		return nil, e, ErrEvidence
	}
	return f, e, nil
}

// PurgeExpiredExports removes at most 1,000 registered disposable exports.
// Archive catalog and tape data are never removed by this operation.
func (s *Store) PurgeExpiredExports(ctx context.Context, dir string) (int, error) {
	if err := privateDirectory(dir); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,filename FROM exports WHERE expires_at<=? ORDER BY expires_at,id LIMIT 1000", stamp())
	if err != nil {
		return 0, err
	}
	type expired struct{ id, name string }
	var pending []expired
	for rows.Next() {
		var e expired
		if err = rows.Scan(&e.id, &e.name); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, e := range pending {
		if _, err = uuid.Parse(e.id); err != nil {
			return count, ErrEvidence
		}
		if filepath.Base(e.name) != e.name || len(e.name) < 37 || e.name[:36] != e.id {
			return count, ErrEvidence
		}
		if err = os.Remove(filepath.Join(dir, e.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return count, err
		}
		if err = s.transaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "DELETE FROM exports WHERE id=? AND expires_at<=?", e.id, stamp())
			return err
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type LegacyArchiveEntry struct {
	ArchiveID      string `json:"archive_id"`
	DataFileIndex  int64  `json:"data_file_index"`
	Bytes          int64  `json:"data_stream_bytes"`
	SHA256         string `json:"data_sha256"`
	IntegrityBasis string `json:"integrity_basis"`
	ObservedAt     string `json:"observed_at"`
}
type LegacyDescriptor struct {
	FormatID     string               `json:"format_id"`
	VolumeID     string               `json:"volume_id"`
	Label        string               `json:"label"`
	ReadProfile  string               `json:"read_profile"`
	Archives     []LegacyArchiveEntry `json:"archives"`
	ExportedAt   string               `json:"exported_at"`
	Instructions string               `json:"recovery_instructions"`
	Caveats      []string             `json:"caveats"`
}

func (s *Store) LegacyDescriptor(ctx context.Context, volume string) (LegacyDescriptor, error) {
	var result LegacyDescriptor
	err := s.readSnapshot(ctx, func(tx *sql.Tx) error {
		var kind, metadata string
		if err := tx.QueryRowContext(ctx, "SELECT v.kind,v.label,m.metadata_json FROM volumes v JOIN volume_metadata m ON m.volume_id=v.id WHERE v.id=?", volume).Scan(&kind, &result.Label, &metadata); err != nil {
			return err
		}
		if kind != "LEGACY" {
			return ErrEvidence
		}
		var binding struct {
			Profile string `json:"profile"`
		}
		if err := json.Unmarshal([]byte(metadata), &binding); err != nil {
			return err
		}
		if binding.Profile == "" {
			return ErrEvidence
		}
		result = LegacyDescriptor{FormatID: "tapepilot-legacy-observed", VolumeID: volume, Label: result.Label, ReadProfile: binding.Profile, Archives: []LegacyArchiveEntry{}, ExportedAt: stamp(), Instructions: "Read each recorded logical file using its explicitly qualified read profile. Compare every archive byte and byte count to the recorded observed SHA-256 before extracting with ordinary GNU tar. Plain uncompressed TAR extraction needs no TapePilot application or database.", Caveats: []string{"The baseline digest was observed at import and has no pre-existing independent expected digest.", "Physical hardware read behavior remains unqualified."}}
		rows, err := tx.QueryContext(ctx, "SELECT id,data_file_index,stream_bytes,sha256,integrity_basis,created_at FROM archives WHERE volume_id=? AND integrity_state='OBSERVED' ORDER BY data_file_index LIMIT 100001", volume)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(result.Archives) == 100000 {
				return ErrEvidence
			}
			var a LegacyArchiveEntry
			if err = rows.Scan(&a.ArchiveID, &a.DataFileIndex, &a.Bytes, &a.SHA256, &a.IntegrityBasis, &a.ObservedAt); err != nil {
				return err
			}
			result.Archives = append(result.Archives, a)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) ActiveExports(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM exports WHERE expires_at>?", stamp()).Scan(&count)
	return count, err
}
