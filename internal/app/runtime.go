// Package app owns one simulator installation and its durable scheduler.
package app

import (
	"context"
	"errors"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type Runtime struct {
	selectionMu sync.Mutex
	Settings    config.Settings
	Store       *store.Store
	Drive       *sim.Drive
	Sources     []source.Root
	Spool       string
}

func Open(ctx context.Context, c config.Settings) (*Runtime, error) {
	if err := c.CheckHost(); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, c.StateDir)
	if err != nil {
		return nil, err
	}
	r := &Runtime{Settings: c, Store: db, Spool: filepath.Join(c.StateDir, "spool")}
	fail := func(err error) (*Runtime, error) { return nil, errors.Join(err, db.Close()) }
	if err = db.ConfigureSMTP(ctx, c.SMTP != nil); err != nil {
		return fail(err)
	}
	if err = os.MkdirAll(r.Spool, 0700); err != nil {
		return fail(err)
	}
	info, err := os.Lstat(r.Spool)
	if err != nil {
		return fail(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fail(errors.New("spool must be a private directory"))
	}
	r.Drive, err = sim.Open(filepath.Join(c.StateDir, "simulator"))
	if err != nil {
		return fail(err)
	}
	for id, path := range c.Sources {
		r.Sources = append(r.Sources, source.Root{ID: id, Path: path})
	}
	// The simulator selection is cosmetic until Insert and a fresh lease validate
	// the cartridge inventory. No physical pathname or position is persisted here.
	name, err := readSelection(filepath.Join(c.StateDir, "simulator-selection"))
	if err == nil {
		if err = r.Drive.Insert(strings.TrimSuffix(string(name), "\n")); err != nil {
			return fail(err)
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	return r, nil
}
func (r *Runtime) Insert(id string) error {
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	if err := r.Drive.Insert(id); err != nil {
		return err
	}
	temp, err := os.CreateTemp(r.Settings.StateDir, ".selection-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.WriteString(id + "\n")
	closeErr := errors.Join(temp.Sync(), temp.Close())
	if err = errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), filepath.Join(r.Settings.StateDir, "simulator-selection")); err != nil {
		return err
	}
	d, err := os.Open(r.Settings.StateDir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func readSelection(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65 {
		return nil, errors.Join(errors.New("invalid simulator selection file"), err, f.Close())
	}
	b, err := io.ReadAll(io.LimitReader(f, 66))
	return b, errors.Join(err, f.Close())
}

func (r *Runtime) clearSelection() error {
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	if err := os.Remove(filepath.Join(r.Settings.StateDir, "simulator-selection")); err != nil && !os.IsNotExist(err) {
		return err
	}
	d, err := os.Open(r.Settings.StateDir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
