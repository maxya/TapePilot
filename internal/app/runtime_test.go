package app

import (
	"context"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/tape"
	"path/filepath"
	"testing"
)

func TestPersistedSimulatorSelection(t *testing.T) {
	c := config.Settings{StateDir: filepath.Join(t.TempDir(), "state")}
	ctx := context.Background()
	r, err := Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Drive.Create("first", 16*tape.RecordBytes, false); err != nil {
		t.Fatal(err)
	}
	if err = r.Insert("first"); err != nil {
		t.Fatal(err)
	}
	if err = r.Store.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Store.Close()
	l, err := r.Drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s, err := l.Status(ctx)
	if err != nil || s.CartridgeID != "first" {
		t.Fatal(s, err)
	}
}
