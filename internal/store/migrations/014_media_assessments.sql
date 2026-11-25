-- Durable read-only assessment stages. Baseline hashes and provenance never change.
CREATE TABLE media_assessments (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id),
 volume_id TEXT NOT NULL REFERENCES volumes(id),
 header_sha256 TEXT NOT NULL,
 failed_index INTEGER CHECK(failed_index>=1 AND failed_index%2=1),
 confirmed_corruption INTEGER NOT NULL DEFAULT 0 CHECK(confirmed_corruption IN(0,1)),
 invalidated INTEGER NOT NULL DEFAULT 0 CHECK(invalidated IN(0,1)),
 complete INTEGER NOT NULL DEFAULT 0 CHECK(complete IN(0,1)),
 finalized INTEGER NOT NULL DEFAULT 0 CHECK(finalized IN(0,1)),
 created_at TEXT NOT NULL
);
CREATE TABLE media_assessment_passes (
 job_id TEXT NOT NULL REFERENCES media_assessments(job_id),
 archive_id TEXT NOT NULL REFERENCES archives(id),
 data_file_index INTEGER NOT NULL,
 checked_at TEXT NOT NULL,
 PRIMARY KEY(job_id,archive_id),
 UNIQUE(job_id,data_file_index)
);

-- Pre-014 VERIFY failures could leave stale current VERIFIED rows. Withdraw
-- only the bounded historical scope: a readback-phase recovery failure after
-- this archive's last successful evidence. Preserve independently checked
-- prefix rows recorded by that attempt and any later readback/import/backup.
-- Canonicalize instants before comparison (RFC3339Nano text ordering is unsafe).
CREATE INDEX verifications_archive_created ON verifications(archive_id,created_at);
CREATE TEMP TABLE migration014_stale_archives AS
 SELECT a.id,a.volume_id FROM archives a JOIN volumes v ON v.id=a.volume_id
 WHERE a.integrity_state='VERIFIED' AND v.kind='MANAGED'
 AND EXISTS (
  SELECT 1 FROM jobs f WHERE f.volume_id=a.volume_id AND f.kind='VERIFY'
   AND f.state='RECOVERY_REQUIRED' AND f.phase='readback' AND f.completed_at IS NOT NULL
   AND tapepilot_utc_instant_v1(COALESCE(a.verified_at,a.created_at)) <= tapepilot_utc_instant_v1(f.completed_at)
   AND NOT EXISTS (
    SELECT 1 FROM verifications e WHERE e.archive_id=a.id
     AND e.expected_sha256=a.sha256 AND e.actual_sha256=a.sha256
     AND e.expected_bytes=a.stream_bytes AND e.actual_bytes=a.stream_bytes
     AND (e.job_id=f.id OR tapepilot_utc_instant_v1(e.created_at)>tapepilot_utc_instant_v1(f.completed_at))
   )
 );
UPDATE archives SET integrity_state='UNKNOWN',restore_eligible=0
 WHERE id IN(SELECT id FROM migration014_stale_archives);
UPDATE volumes SET state='QUARANTINED'
 WHERE id IN(SELECT volume_id FROM migration014_stale_archives) AND state!='SEALED';
DROP TABLE migration014_stale_archives;
