package media

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string, value any) []byte {
	t.Helper()
	b, err := os.ReadFile("../../schemas/fixtures/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(b, value); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVolumeRecovery(t *testing.T) {
	var v Volume
	raw := fixture(t, "volume", &v)
	encoded, err := EncodeVolume(v)
	if err != nil {
		t.Fatal(err)
	}
	read, sum, err := ReadVolume(bytes.NewReader(encoded))
	if err != nil || read.VolumeID != v.VolumeID {
		t.Fatal(read, err)
	}
	hash := sha256.Sum256(encoded)
	if sum.SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal(sum)
	}
	cmd := exec.Command("tar", "-xOf", "-", "tapepilot/volume.json")
	cmd.Stdin = bytes.NewReader(encoded)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(out, &read); err != nil {
		t.Fatal(err)
	}
	descriptor, err := SimulatorDescriptor(v, sum.SHA256, nil, time.Now())
	if err != nil || descriptor.VolumeID != v.VolumeID {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"format_minor": 0`, `"format_minor":0,"format_minor":0`, 1), strings.Replace(string(raw), "volume_id", "VOLUME_ID", 1), strings.Replace(string(raw), `"hardware_compression": false`, `"hardware_compression":null`, 1), string(raw) + "{}"} {
		if decode([]byte(bad), &read) == nil {
			t.Fatal("accepted invalid JSON")
		}
	}
}
func TestCommitChain(t *testing.T) {
	var c Commit
	fixture(t, "commit", &c)
	raw, err := EncodeCommit(c)
	if err != nil {
		t.Fatal(err)
	}
	read, _, err := ReadCommit(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	data := Digest{c.DataStreamBytes, c.DataSHA256}
	if err = read.Match(c.VolumeID, c.VolumeHeaderSHA256, 1, "", data); err != nil {
		t.Fatal(err)
	}
	for _, modify := range []func(*Commit){func(x *Commit) { x.DataStreamBytes++ }, func(x *Commit) { x.CommitFileIndex++ }, func(x *Commit) { x.Sequence = 1 << 62 }, func(x *Commit) { x.VolumeHeaderSHA256 = strings.Repeat("1", 64) }, func(x *Commit) { x.SourceSelection = "../other" }} {
		bad := c
		modify(&bad)
		if bad.Match(c.VolumeID, c.VolumeHeaderSHA256, 1, "", data) == nil {
			t.Fatal(bad)
		}
	}
}
func TestGNUArchiveFraming(t *testing.T) {
	root := t.TempDir()
	file := strings.Repeat("é", 65)
	if err := os.WriteFile(filepath.Join(root, file), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "-C", root, "--format=pax", "--blocking-factor=512", "-cf", "-", "--", file)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	sum, err := Scan(bytes.NewReader(b), func(h *tar.Header) error {
		n++
		if h.Name != file || h.Size != 5 {
			t.Fatal(h)
		}
		return nil
	})
	if err != nil || n != 1 || sum.Bytes != 262144 {
		t.Fatal(n, sum, err)
	}
	for _, bad := range [][]byte{b[:len(b)-512], append(bytes.Clone(b), make([]byte, 262144)...), bytes.Clone(b), b[:2048]} {
		if len(bad) == len(b) {
			bad[len(bad)-1] = 1
		}
		if _, err := Scan(bytes.NewReader(bad), nil); err == nil {
			t.Fatal("accepted broken framing")
		}
	}
	corrupt := bytes.Clone(b)
	corrupt[0] ^= 1
	if _, err := Scan(bytes.NewReader(corrupt), nil); err == nil {
		t.Fatal("accepted checksum")
	}
}
func FuzzVolume(f *testing.F) {
	var v Volume
	b, err := os.ReadFile("../../schemas/fixtures/volume.json")
	if err != nil {
		f.Fatal(err)
	}
	if err = json.Unmarshal(b, &v); err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		var v Volume
		if decode(b, &v) == nil && v.Validate() == nil {
			if _, err := EncodeVolume(v); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestRejectDuplicatesAndNonzeroAfterTerminator(t *testing.T) {
	var v Volume
	fixture(t, "volume", &v)
	valid, err := EncodeVolume(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		append(bytes.Clone(valid[:1024]), valid[:len(valid)-1024]...),
		append(bytes.Clone(valid[:1536]), make([]byte, 512)...),
	} {
		if _, _, err := ReadVolume(bytes.NewReader(bad)); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for i := 0; i < 2; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0600, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	archive.Write(make([]byte, 262144-archive.Len()))
	if _, err := Scan(bytes.NewReader(archive.Bytes()), nil); err == nil {
		t.Fatal("accepted duplicate member")
	}
	garbage := bytes.Clone(valid)
	garbage[len(garbage)-1] = 1
	if _, _, err := ReadVolume(bytes.NewReader(garbage)); err == nil {
		t.Fatal("accepted trailing data")
	}
}
