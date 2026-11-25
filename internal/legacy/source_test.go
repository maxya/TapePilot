package legacy

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxya/TapePilot/internal/media"
)

func fixture(t *testing.T, format string) []byte {
	t.Helper()
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "file"), []byte("legacy payload"), 0600); e != nil {
		t.Fatal(e)
	}
	command := exec.Command("/usr/bin/tar", "--create", "--format="+format, "--file=-", "--directory="+root, ".")
	command.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	raw, e := command.Output()
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func TestOrdinaryLegacyArchivesPreserveNamesAndExactDigest(t *testing.T) {
	for _, format := range []string{"gnu", "pax", "ustar"} {
		t.Run(format, func(t *testing.T) {
			raw := fixture(t, format)
			names := []string{}
			sum, e := Scan(bytes.NewReader(raw), func(h *tar.Header) error { names = append(names, h.Name); return nil })
			if e != nil {
				t.Fatal(e)
			}
			expected := sha256.Sum256(raw)
			if sum.Bytes != int64(len(raw)) || sum.SHA256 != hex.EncodeToString(expected[:]) || len(names) != 2 || names[0] != "./" || names[1] != "./file" {
				t.Fatal("names or observed digest differ", names, sum)
			}
			if _, e = media.Scan(bytes.NewReader(raw), nil); e == nil {
				t.Fatal("legacy framing accepted as managed")
			}
		})
	}
}
func TestRejectLegacyUnsafeOrMalformedArchives(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/../b"} {
		var b bytes.Buffer
		writer := tar.NewWriter(&b)
		if e := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1}); e != nil {
			t.Fatal(e)
		}
		writer.Write([]byte("x"))
		writer.Close()
		if _, e := Scan(bytes.NewReader(b.Bytes()), nil); e == nil {
			t.Errorf("accepted %q", name)
		}
	}
	raw := fixture(t, "gnu")
	for _, bad := range [][]byte{raw[:1024], append(append([]byte(nil), raw...), 1), append(append([]byte(nil), raw...), make([]byte, 1024*1024+512)...), []byte("\x1f\x8b compressed")} {
		if _, e := Scan(bytes.NewReader(bad), nil); e == nil {
			t.Fatal("accepted malformed archive")
		}
	}
	var b bytes.Buffer
	writer := tar.NewWriter(&b)
	writer.WriteHeader(&tar.Header{Name: "file", Mode: 0600, Size: 0})
	writer.WriteHeader(&tar.Header{Name: "./file", Mode: 0600, Size: 0})
	writer.Close()
	if _, e := Scan(bytes.NewReader(b.Bytes()), nil); e == nil {
		t.Fatal("canonical duplicate accepted")
	}
}
func TestConfiguredReadOnlySourceBinding(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	raw := fixture(t, "gnu")
	if e := os.WriteFile(filepath.Join(root, "000000.tar"), raw, 0400); e != nil {
		t.Fatal(e)
	}
	roots := []Root{{"old", root}}
	if _, e := Identify(ctx, roots, "old", SimulatorProfile, false); !errors.Is(e, ErrPolicy) {
		t.Fatal("operator acknowledgment bypassed")
	}
	binding, e := Identify(ctx, roots, "old", SimulatorProfile, true)
	if e != nil {
		t.Fatal(e)
	}
	if count, e := Count(ctx, roots, binding); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	file, e := Open(ctx, roots, binding, 0)
	if e != nil {
		t.Fatal(e)
	}
	actual, e := io.ReadAll(file)
	if e != nil || !bytes.Equal(actual, raw) {
		t.Fatal(e)
	}
	if e = file.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(ctx, roots, binding, 1); e == nil {
		t.Fatal("missing file accepted")
	}
	if e = os.Symlink("/dev/nst999", filepath.Join(root, "000001.tar")); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	if _, e = Open(ctx, roots, binding, 1); e == nil {
		t.Fatal("symlink accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("special source blocked")
	}
	os.Remove(filepath.Join(root, "000001.tar"))
	replacement := root + "-old"
	if e = os.Rename(root, replacement); e != nil {
		t.Fatal(e)
	}
	defer os.Rename(replacement, root)
	if e = os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	defer os.Remove(root)
	if _, e = Open(ctx, roots, binding, 0); !errors.Is(e, ErrIdentity) {
		t.Fatal("source root replacement accepted", e)
	}
}
