package linux

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestConfiguredSysfsIdentityUsesOnlyMatchingSyntheticNode(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "class", "scsi_tape", "nst0")
	device := filepath.Join(root, "devices", "synthetic", "1:0:0:0")
	for _, path := range []string{node, device} {
		if e := os.MkdirAll(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Symlink(device, filepath.Join(node, "device")); e != nil {
		t.Fatal(e)
	}
	for name, value := range map[string]string{"vendor": "TEST\n", "model": "SYNTHETIC\n", "rev": "1\n", "wwid": "synthetic-wwid\n"} {
		if e := os.WriteFile(filepath.Join(device, name), []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(node, "dev"), []byte("9:128\n"), 0600); e != nil {
		t.Fatal(e)
	}
	identity, e := identityFromSysfs(root, Identity{Major: 9, Minor: 128, NodeInode: 99}, "synthetic-kernel")
	if e != nil || identity.StableID != "synthetic-wwid" || identity.HCTL != "1:0:0:0" || identity.NodeInode != 99 {
		t.Fatal(identity, e)
	}
	if e = os.WriteFile(filepath.Join(node, "dev"), []byte("9:129\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = identityFromSysfs(root, Identity{Major: 9, Minor: 128}, "synthetic-kernel"); !errors.Is(e, ErrIdentity) {
		t.Fatal("mismatched mapping accepted", e)
	}
}
func TestUtilityVariantAndStatusFixtures(t *testing.T) {
	version, e := ParseMTVersion([]byte("mt-st v. 1.7\ndefault tape device: /dev/tape\n"))
	if e != nil || version != "1.7" {
		t.Fatal(version, e)
	}
	for _, raw := range [][]byte{[]byte("mt (GNU cpio) 2.13"), bytes.Repeat([]byte("x"), 4097), []byte("unknown variant")} {
		if _, e = ParseMTVersion(raw); !errors.Is(e, ErrUtility) {
			t.Fatal("unsupported utility accepted")
		}
	}
	value := mtget{GenericStatus: 0x01000000 | 0x04000000 | 0x80000000 | 0x08000000, FileNumber: 3, BlockNumber: 0, DeviceStatus: 0}
	status := decodeStatus(value)
	if !status.Online || !status.WriteProtected || !status.EOF || !status.EOD || status.FileNumber != 3 || status.BlockBytes != 0 {
		t.Fatal(status)
	}
	if unsafe.Sizeof(value) != 48 {
		t.Fatal("unsupported MTIOCGET ABI")
	}
	value.FileNumber = -1
	value.BlockNumber = -1
	status = decodeStatus(value)
	if status.FileNumber != -1 || status.BlockNumber != -1 {
		t.Fatal("unknown position converted into known value")
	}
}
func TestStrictProfileAndConfigAndNoDeviceOpen(t *testing.T) {
	c, p, _ := fixture(t)
	raw, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	var decoded Config
	if e = json.Unmarshal(raw, &decoded); e != nil {
		t.Fatal(e)
	}
	duplicate := bytes.Replace(raw, []byte(`"device":`), []byte(`"device":null,"device":`), 1)
	if json.Unmarshal(duplicate, &decoded) == nil {
		t.Fatal("duplicate field accepted")
	}
	bad := bytes.Replace(raw, []byte(`"device":`), []byte(`"Device":`), 1)
	if json.Unmarshal(bad, &decoded) == nil {
		t.Fatal("case alias accepted")
	}
	p.EODPolicy = "infer-from-eio"
	raw, e = json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(c.ProfileFile, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadProfile(c.ProfileFile); !errors.Is(e, ErrProfile) {
		t.Fatal("unqualified EOD accepted", e)
	}
	plain := filepath.Join(t.TempDir(), "regular-file")
	os.WriteFile(plain, []byte("synthetic"), 0600)
	if file, _, e := pinNode(plain); !errors.Is(e, ErrIdentity) {
		if file != nil {
			file.Close()
		}
		t.Fatal("regular file accepted as tape")
	}
}
