package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimulatorOnly(t *testing.T) {
	const good = `{"mode":"simulator","physical_writes_enabled":false,"verify_after_write":true}`
	if Validate(strings.NewReader(good)) != nil {
		t.Fatal("valid settings rejected")
	}
	for _, bad := range []string{"{}", good + "{}", good + strings.Repeat(" ", 4097), strings.Replace(good, "simulator", "physical", 1), strings.Replace(good, "false", "true", 1), strings.Replace(good, "true", "false", 1), strings.Replace(good, "{", `{"mode":"physical",`, 1), strings.Replace(good, "mode", "device", 1)} {
		if Validate(strings.NewReader(bad)) == nil {
			t.Errorf("accepted invalid config")
		}
	}
}

func TestHostAllowlists(t *testing.T) {
	const base = `{"mode":"simulator","physical_writes_enabled":false,"verify_after_write":true,`
	for _, bad := range []string{`"sources":{"x":"/dev/stub"}}`, `"sources":{"x":"/test","x":"/other"}}`, `"state_dir":"/state","restore_roots":{"x":"/state/out"}}`, `"state_dir":"/state","sources":{"x":"/state"}}`, `"sources":{"x":"../test"}}`} {
		if Validate(strings.NewReader(base+bad)) == nil {
			t.Fatal(bad)
		}
	}
}

func TestCheckHostRejectsSymlinkAncestor(t *testing.T) {
	base := t.TempDir()
	if err := os.Symlink(base, filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	c := Settings{StateDir: filepath.Join(base, "alias", "state")}
	if c.CheckHost() == nil {
		t.Fatal("accepted symlink ancestor")
	}
}

func TestPublicOriginRequiresExactLocalHTTPOrTLS(t *testing.T) {
	for _, s := range []string{"http://127.0.0.1:8787", "http://[::1]:8787", "https://archive.example"} {
		if !ValidOrigin(s) {
			t.Error(s)
		}
	}
	for _, s := range []string{"http://archive.example", "https://user@archive.example", "https://archive.example:99999", "https://archive.example/path", "http://localhost:0"} {
		if ValidOrigin(s) {
			t.Error(s)
		}
	}
}
