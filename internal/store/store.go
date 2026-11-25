// Package store owns durable state and the single scheduler claim.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS
var ErrConflict = errors.New("STATE_CONFLICT")
var ErrOwned = errors.New("STORE_ALREADY_OWNED")
var ErrEvidence = errors.New("COMPLETION_EVIDENCE_REQUIRED")
var ErrNotFound = sql.ErrNoRows

type Store struct {
	db        *sql.DB
	owner     *os.File
	mu        sync.Mutex
	readSlots chan struct{}
	readDSN   string
}

func stamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

// Open requires a private local directory and takes an advisory lifetime lock.
func Open(ctx context.Context, dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("state directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	if err := checkLocalFilesystem(dir); err != nil {
		return nil, err
	}
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		owner.Close()
		return nil, ErrOwned
	}
	path := filepath.Join(dir, "catalog.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		owner.Close()
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		owner.Close()
		return nil, errors.New("catalog must be a private regular file")
	}
	if err = f.Close(); err != nil {
		owner.Close()
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		owner.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, owner: owner, readSlots: make(chan struct{}, 4), readDSN: (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"}
	if err = s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if err = s.Reconcile(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return errors.Join(s.db.Close(), s.owner.Close()) }
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL,checksum TEXT NOT NULL)"); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	var version, count int
	if err = s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0),COUNT(*) FROM schema_migrations").Scan(&version, &count); err != nil {
		return err
	}
	if version > len(entries) || version != count {
		return errors.New("unsupported or noncontiguous database schema version")
	}
	for i := 0; i < version; i++ {
		script, err := migrations.ReadFile("migrations/" + entries[i].Name())
		if err != nil {
			return err
		}
		var actual string
		if err = s.db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version=?", i+1).Scan(&actual); err != nil {
			return err
		}
		if actual != fmt.Sprintf("%x", sha256.Sum256(script)) {
			return errors.New("migration checksum mismatch")
		}
	}
	if version > 0 && version < len(entries) {
		dest := filepath.Join(filepath.Dir(s.owner.Name()), fmt.Sprintf("pre-migration-v%d-%s.db", version, uuid.NewString()))
		if err = s.Backup(ctx, dest); err != nil {
			return fmt.Errorf("pre-migration backup: %w", err)
		}
	}
	for i := version; i < len(entries); i++ {
		script, err := migrations.ReadFile("migrations/" + entries[i].Name())
		if err != nil {
			return err
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(script))
		if err = s.transaction(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?)", i+1, stamp(), checksum)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func privateDirectory(dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if resolved != filepath.Clean(dir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("directory must be private and have no symlink ancestors")
	}
	return nil
}
