package media

import "time"

type ArchiveEntry struct {
	ArchiveID       string  `json:"archive_id"`
	Sequence        int64   `json:"sequence"`
	DataFileIndex   int64   `json:"data_file_index"`
	CommitFileIndex int64   `json:"commit_file_index"`
	DataStreamBytes int64   `json:"data_stream_bytes"`
	DataSHA256      string  `json:"data_sha256"`
	CommitSHA256    string  `json:"commit_sha256"`
	SourceLabel     string  `json:"source_label"`
	CreatedAt       string  `json:"created_at"`
	VerifiedAt      *string `json:"verified_at"`
	IntegrityBasis  string  `json:"integrity_basis"`
}
type Descriptor struct {
	FormatID               string         `json:"format_id"`
	FormatMajor            int64          `json:"format_major"`
	FormatMinor            int64          `json:"format_minor"`
	VolumeID               string         `json:"volume_id"`
	Label                  string         `json:"label_at_initialization"`
	Profile                Profile        `json:"tape_profile"`
	HeaderSHA256           string         `json:"volume_header_sha256"`
	Archives               []ArchiveEntry `json:"archives"`
	ExportedAt             string         `json:"exported_at"`
	Instructions           string         `json:"recovery_instructions"`
	HardwareProfile        string         `json:"hardware_profile"`
	HardwareProfileVersion string         `json:"hardware_profile_version"`
	Caveats                []string       `json:"caveats"`
}

// SimulatorDescriptor exports catalog evidence supplied by a verified scan.
func SimulatorDescriptor(v Volume, header string, entries []ArchiveEntry, now time.Time) (Descriptor, error) {
	if err := v.Validate(); err != nil {
		return Descriptor{}, err
	}
	if !digest.MatchString(header) {
		return Descriptor{}, ErrFormat
	}
	ids := map[string]bool{}
	for i, e := range entries {
		n := int64(i) + 1
		if e.Sequence != n || e.CommitFileIndex != 2*n || e.DataFileIndex != 2*n-1 || !uuid.MatchString(e.ArchiveID) || ids[e.ArchiveID] || !digest.MatchString(e.CommitSHA256) || !digest.MatchString(e.DataSHA256) || e.DataStreamBytes <= 0 || e.DataStreamBytes%v.TapeProfile.RecordBytes != 0 || !instant(e.CreatedAt) || !text(e.SourceLabel) || e.VerifiedAt == nil || !instant(*e.VerifiedAt) || (e.IntegrityBasis != "WRITE_READBACK" && e.IntegrityBasis != "ON_MEDIA_MANIFEST") {
			return Descriptor{}, ErrFormat
		}
		ids[e.ArchiveID] = true
	}
	return Descriptor{v.Magic, v.FormatMajor, v.FormatMinor, v.VolumeID, v.LabelAtInitialization, v.TapeProfile, header, append([]ArchiveEntry{}, entries...), now.UTC().Format(time.RFC3339Nano), recoveryInstructions, "simulator", "1", []string{"Physical profile unqualified."}}, nil
}
