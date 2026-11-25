package media

type Profile struct {
	BlockMode           string `json:"block_mode"`
	RecordBytes         int64  `json:"record_bytes"`
	TarBlockingFactor   int64  `json:"tar_blocking_factor"`
	DataPartitions      int64  `json:"data_partitions"`
	FilemarkPolicy      string `json:"filemark_policy"`
	HardwareCompression bool   `json:"hardware_compression"`
	SoftwareCompression string `json:"software_compression"`
	Encryption          string `json:"encryption"`
}
type Volume struct {
	Magic                 string  `json:"magic"`
	FormatMajor           int64   `json:"format_major"`
	FormatMinor           int64   `json:"format_minor"`
	VolumeID              string  `json:"volume_id"`
	LabelAtInitialization string  `json:"label_at_initialization"`
	CreatedAt             string  `json:"created_at"`
	CreatedByVersion      string  `json:"created_by_version"`
	MediaGeneration       string  `json:"media_generation"`
	NativeCapacityBytes   *int64  `json:"native_capacity_bytes"`
	TapeProfile           Profile `json:"tape_profile"`
	ArchiveFormat         string  `json:"archive_format"`
	HashAlgorithm         string  `json:"hash_algorithm"`
}
type Commit struct {
	Magic                string  `json:"magic"`
	FormatMajor          int64   `json:"format_major"`
	FormatMinor          int64   `json:"format_minor"`
	VolumeID             string  `json:"volume_id"`
	VolumeHeaderSHA256   string  `json:"volume_header_sha256"`
	Sequence             int64   `json:"sequence"`
	ArchiveID            string  `json:"archive_id"`
	JobID                string  `json:"job_id"`
	SourceLabel          string  `json:"source_label"`
	SourceSelection      string  `json:"source_selection"`
	CreatedAt            string  `json:"created_at"`
	DataFileIndex        int64   `json:"data_file_index"`
	CommitFileIndex      int64   `json:"commit_file_index"`
	DataStreamBytes      int64   `json:"data_stream_bytes"`
	DataRecordCount      int64   `json:"data_record_count"`
	DataSHA256           string  `json:"data_sha256"`
	PreviousCommitSHA256 *string `json:"previous_commit_sha256"`
	ArchiveFormat        string  `json:"archive_format"`
	RecordBytes          int64   `json:"record_bytes"`
	SourceConsistency    string  `json:"source_consistency"`
}
