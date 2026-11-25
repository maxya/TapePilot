package restore

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/catalog"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/store"
)

func paxRecord(key, value string) string {
	length := len(key) + len(value) + 4
	for {
		line := fmt.Sprintf("%d %s=%s\n", length, key, value)
		if len(line) == length {
			return line
		}
		length = len(line)
	}
}
func rawHeader(t *testing.T, name string, kind byte, size int64) []byte {
	t.Helper()
	var b bytes.Buffer
	writer := tar.NewWriter(&b)
	if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0640, Format: tar.FormatUSTAR, Size: 0}); err != nil {
		t.Fatal(err)
	}
	header := append([]byte{}, b.Bytes()[:512]...)
	header[156] = kind
	copy(header[124:136], fmt.Sprintf("%011o\x00", size))
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	sum := 0
	for _, value := range header {
		sum += int(value)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return header
}
func paxArchive(t *testing.T, records, payload string, headerSize int64) []byte {
	t.Helper()
	var b bytes.Buffer
	b.Write(rawHeader(t, "PaxHeaders/file", tar.TypeXHeader, int64(len(records))))
	b.WriteString(records)
	b.Write(make([]byte, (512-len(records)%512)%512))
	b.Write(rawHeader(t, "placeholder", tar.TypeReg, headerSize))
	b.WriteString(payload)
	b.Write(make([]byte, (512-len(payload)%512)%512))
	b.Write(make([]byte, 1024))
	return b.Bytes()
}
func importPAXFixture(t *testing.T, raw []byte) (*fixture, store.Job) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	legacyDir := filepath.Join(root, "legacy")
	output := filepath.Join(root, "output")
	spool := filepath.Join(root, "spool")
	for _, dir := range []string{legacyDir, output, spool} {
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(legacyDir, "000000.tar"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	roots := []legacy.Root{{ID: "old", Path: legacyDir}}
	volumeID := uuid.NewString()
	f := &fixture{s: s, root: root, volumeID: volumeID, legacy: true, r: &Restore{Store: s, Roots: map[string]string{"recovered": output}, LegacyRoots: roots, Spool: spool}}
	if err = os.WriteFile(filepath.Join(root, "outside"), []byte("preserve sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	options := catalog.ImportOptions{Mode: "legacy", SourceID: "old", Profile: legacy.SimulatorProfile, Label: "PAX fixture", VolumeID: volumeID, Confirmed: true}
	j, err := s.Enqueue(ctx, store.Job{Kind: domain.Import, Options: mustOptions(t, options)})
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := jobs.New(s, &catalog.Importer{Store: s, LegacyRoots: roots}).RunOne(ctx); err != nil || !ran {
		t.Fatal(ran, err)
	}
	j, err = s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State == domain.Succeeded {
		archives, err := s.ListArchives(ctx, store.ArchiveQuery{VolumeID: volumeID, Limit: 2})
		if err != nil || len(archives) != 1 {
			t.Fatal(archives, err)
		}
		f.archiveID = archives[0].ID
	}
	return f, j
}
func TestPAXOverridesAndOversizedHeadersThroughRestore(t *testing.T) {
	for _, tc := range []struct {
		name, records, payload, output string
		headerSize                     int64
		accepted                       bool
	}{
		{"unsafe path override", paxRecord("path", "../outside"), "changed", "", 7, false},
		{"absolute path override", paxRecord("path", "/outside"), "changed", "", 7, false},
		{"safe effective path", paxRecord("path", "dir/-effective\nname"), "payload", "dir/-effective\nname", 7, true},
		{"effective size", paxRecord("size", "7"), "payload", "placeholder", 0, true},
		{"negative effective size", paxRecord("size", "-1"), "payload", "", 7, false},
		{"oversized extended header", paxRecord("path", strings.Repeat("x", (1<<20)+4096)), "payload", "", 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, imported := importPAXFixture(t, paxArchive(t, tc.records, tc.payload, tc.headerSize))
			if tc.accepted {
				if imported.State != domain.Succeeded {
					t.Fatal("supported PAX interpretation rejected", imported)
				}
				restored := f.run(t, f.options("restored"))
				if restored.State != domain.Succeeded {
					t.Fatal(restored)
				}
				got, err := os.ReadFile(filepath.Join(f.root, "output", "restored", tc.output))
				if err != nil || string(got) != tc.payload {
					t.Fatal("effective PAX fields restored incorrectly", string(got), err)
				}
			} else {
				if imported.State == domain.Succeeded {
					t.Fatal("hostile PAX archive accepted")
				}
				entries, err := os.ReadDir(filepath.Join(f.root, "output"))
				if err != nil || len(entries) != 0 {
					t.Fatal("unsafe tree published", err)
				}
			}
			outside, err := os.ReadFile(filepath.Join(f.root, "outside"))
			if err != nil || string(outside) != "preserve sentinel" {
				t.Fatal("write escaped private tree", err)
			}
		})
	}
}

func mustOptions(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
