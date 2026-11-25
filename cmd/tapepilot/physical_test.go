package main

import (
	"context"
	"github.com/maxya/TapePilot/internal/config"
	linux "github.com/maxya/TapePilot/internal/tape/linux"
	"io"
	"testing"
)

// All requests in this test must return before any Linux device lookup.
func TestPhysicalCommandsRequireLocalConfigConfirmationAndLimits(t *testing.T) {
	c := config.Default()
	if physicalCommand(context.Background(), c, []string{"physical-doctor", "--confirm-read-only", "--confirm-write-protected"}, io.Discard) == nil {
		t.Fatal("unconfigured physical access")
	}
	c.PhysicalRead = &linux.Config{}
	for _, args := range [][]string{{"physical-doctor"}, {"physical-doctor", "--confirm-read-only"}, {"physical-snapshot", "missing", "1", "1024", "--confirm-read-only", "--confirm-write-protected"}} {
		if physicalCommand(context.Background(), c, args, io.Discard) == nil {
			t.Fatal(args)
		}
	}
}
