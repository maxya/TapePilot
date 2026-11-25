package store

import (
	"context"
	"database/sql"
	"errors"
	"modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
)

// Backup uses SQLite's online backup API, including committed WAL contents.
// The caller supplies a fresh path inside an administrator-owned export directory.
func (s *Store) Backup(ctx context.Context, dest string) (err error) {
	if !filepath.IsAbs(dest) {
		return errors.New("backup destination must be absolute")
	}
	if err = privateDirectory(filepath.Dir(dest)); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			os.Remove(dest)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(raw any) error {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite online backup unsupported")
		}
		b, err := source.NewBackup((&url.URL{Scheme: "file", Path: dest}).String())
		if err != nil {
			return err
		}
		for {
			if err = ctx.Err(); err != nil {
				return errors.Join(err, b.Finish())
			}
			more, stepErr := b.Step(128)
			if stepErr != nil {
				return errors.Join(stepErr, b.Finish())
			}
			if !more {
				break
			}
		}
		return b.Finish()
	})
	if err != nil {
		return err
	}
	check, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dest}).String()+"?mode=ro")
	if err != nil {
		return err
	}
	var result string
	err = check.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result)
	closeErr := check.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if result != "ok" {
		return errors.New("backup integrity check failed")
	}
	f, err = os.OpenFile(dest, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(dest))
	if err != nil {
		return err
	}
	if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
		return err
	}
	completed = true
	return nil
}
