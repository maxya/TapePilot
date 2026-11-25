package media

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maxya/TapePilot/internal/tape"
)

var uuid = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
var digest = regexp.MustCompile("^[0-9a-f]{64}$")

func text(s string) bool {
	return s != "" && utf8.ValidString(s) && len(s) <= 4096 && !strings.ContainsRune(s, 0)
}
func instant(s string) bool {
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && strings.HasSuffix(s, "Z")
}
func StandardProfile() Profile {
	return Profile{"variable", tape.RecordBytes, tape.BlockingFactor, 1, "single-on-close", false, "none", "none"}
}
func (v Volume) Validate() error {
	p := v.TapeProfile
	p.HardwareCompression = false // compression is descriptive; eligibility is profile-specific.
	if v.Magic != tape.VolumeMagic || v.FormatMajor != 1 || v.FormatMinor != 0 || !uuid.MatchString(v.VolumeID) || !text(v.LabelAtInitialization) || !text(v.CreatedByVersion) || !text(v.MediaGeneration) || !instant(v.CreatedAt) || p != StandardProfile() || v.ArchiveFormat != "pax-tar" || v.HashAlgorithm != "sha256" || v.NativeCapacityBytes != nil && *v.NativeCapacityBytes < 0 {
		return ErrFormat
	}
	return nil
}
func (c Commit) Validate() error {
	if c.Magic != tape.CommitMagic || c.FormatMajor != 1 || c.FormatMinor != 0 || c.RecordBytes != tape.RecordBytes || c.ArchiveFormat != "pax-tar" || !uuid.MatchString(c.VolumeID) || !uuid.MatchString(c.JobID) || !uuid.MatchString(c.ArchiveID) || !digest.MatchString(c.VolumeHeaderSHA256) || !digest.MatchString(c.DataSHA256) || !instant(c.CreatedAt) || !text(c.SourceLabel) || !text(c.SourceSelection) || !fs.ValidPath(c.SourceSelection) {
		return ErrFormat
	}
	if c.Sequence <= 0 || c.Sequence > math.MaxInt64/2 || c.CommitFileIndex != 2*c.Sequence || c.DataFileIndex != c.CommitFileIndex-1 || c.DataStreamBytes <= 0 || c.DataStreamBytes%tape.RecordBytes != 0 || c.DataRecordCount != c.DataStreamBytes/tape.RecordBytes {
		return ErrFormat
	}
	if (c.Sequence == 1) != (c.PreviousCommitSHA256 == nil) || c.PreviousCommitSHA256 != nil && !digest.MatchString(*c.PreviousCommitSHA256) {
		return ErrFormat
	}
	if c.SourceConsistency != "live-best-effort" && c.SourceConsistency != "operator-supplied-snapshot" {
		return ErrFormat
	}
	return nil
}

func (c Commit) Match(volumeID, header string, sequence int64, previous string, data Digest) error {
	if err := c.Validate(); err != nil {
		return err
	}
	prev := ""
	if c.PreviousCommitSHA256 != nil {
		prev = *c.PreviousCommitSHA256
	}
	if c.VolumeID != volumeID || c.VolumeHeaderSHA256 != header || c.Sequence != sequence || prev != previous || c.DataStreamBytes != data.Bytes || c.DataSHA256 != data.SHA256 {
		return fmt.Errorf("%w: archive chain mismatch", ErrFormat)
	}
	return nil
}

// checkShape enforces all tagged fields exactly once at each supported nesting
// level. encoding/json alone accepts repeated and case-insensitive keys.
func checkShape(d *json.Decoder, t reflect.Type) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if t.Kind() != reflect.Struct {
		if _, nested := token.(json.Delim); nested || token == nil && t.Kind() != reflect.Pointer {
			return ErrFormat
		}
		return nil
	}
	if token != json.Delim('{') {
		return ErrFormat
	}
	fields := make(map[string]reflect.Type, t.NumField())
	for f := range t.Fields() {
		fields[f.Tag.Get("json")] = f.Type
	}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := k.(string)
		ft, known := fields[name]
		if !ok || !known {
			return ErrFormat
		}
		delete(fields, name)
		if err := checkShape(d, ft); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 0 {
		return ErrFormat
	}
	return nil
}
func decode(b []byte, value any) error {
	if len(b) > 64<<10 || !utf8.Valid(b) {
		return ErrFormat
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := checkShape(d, reflect.TypeOf(value).Elem()); err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

const recoveryInstructions = "TapePilot v1. Read each tape file independently on a qualified read-only profile. Volume label is file 0. Sequence n has data file 2n-1 and commit file 2n. Compare the full data file SHA-256 and byte count with the commit before using GNU tar. Preserve earlier pairs if the tail is incomplete.\n"

func EncodeVolume(v Volume) ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return encode(tape.VolumeMember, v, fmt.Sprintf("Volume %s, record bytes %d.\n%s", v.VolumeID, tape.RecordBytes, recoveryInstructions))
}
func EncodeCommit(c Commit) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return encode(tape.CommitMember, c, "")
}
func encode(name string, v any, recovery string) ([]byte, error) {
	value, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(value) > 64<<10 {
		return nil, ErrFormat
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	write := func(n string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0444, Size: int64(len(data)), Format: tar.FormatUSTAR, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := write(name, value); err != nil {
		return nil, err
	}
	if recovery != "" {
		if err := write(tape.RecoveryMember, []byte(recovery)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	b.Write(make([]byte, (tape.RecordBytes-b.Len()%tape.RecordBytes)%tape.RecordBytes))
	return b.Bytes(), nil
}
func readMetadata(r io.Reader, name string, v any) (Digest, error) {
	gotJSON, gotGuide := false, false
	sum, err := scan(io.LimitReader(r, (1<<20)+1), func(h *tar.Header, r io.Reader) error {
		switch h.Name {
		case name:
			if h.Size > 64<<10 {
				return ErrFormat
			}
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			gotJSON = true
			return decode(b, v)
		case tape.RecoveryMember:
			if name != tape.VolumeMember || h.Size <= 0 || h.Size > 128<<10 {
				return ErrFormat
			}
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			gotGuide = utf8.Valid(b) && !bytes.ContainsRune(b, 0)
			return nil
		default:
			return ErrFormat
		}
	}, true)
	if err != nil {
		return Digest{}, err
	}
	if !gotJSON || name == tape.VolumeMember && !gotGuide {
		return Digest{}, ErrFormat
	}
	return sum, nil
}
func ReadVolume(r io.Reader) (Volume, Digest, error) {
	var v Volume
	sum, err := readMetadata(r, tape.VolumeMember, &v)
	if err == nil {
		err = v.Validate()
	}
	return v, sum, err
}
func ReadCommit(r io.Reader) (Commit, Digest, error) {
	var c Commit
	sum, err := readMetadata(r, tape.CommitMember, &c)
	if err == nil {
		err = c.Validate()
	}
	return c, sum, err
}
