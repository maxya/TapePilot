CREATE TABLE volumes (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('MANAGED','LEGACY')),
 label TEXT NOT NULL, location TEXT NOT NULL DEFAULT '', generation TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('UNCLASSIFIED','BLANK_CANDIDATE','MANAGED','LEGACY_READ_ONLY','SEALED','QUARANTINED','UNKNOWN')),
 header_sha256 TEXT, created_at TEXT NOT NULL
);
CREATE TABLE jobs (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('INITIALIZE','BACKUP','VERIFY','IMPORT','RESTORE','INSPECT','UNLOAD','REWIND')),
 state TEXT NOT NULL CHECK(state IN ('QUEUED','PREFLIGHT','WAITING_MEDIA','RUNNING','VERIFYING','SUCCEEDED','FAILED','CANCEL_REQUESTED','CANCELLED','RECOVERY_REQUIRED')),
 phase TEXT NOT NULL DEFAULT '', volume_id TEXT REFERENCES volumes(id),
 options_json TEXT NOT NULL CHECK(json_valid(options_json)), dedupe_key TEXT UNIQUE,
 state_version INTEGER NOT NULL DEFAULT 0 CHECK(state_version>=0),
 active_slot INTEGER UNIQUE CHECK(active_slot=1), created_at TEXT NOT NULL, completed_at TEXT,
 error_code TEXT NOT NULL DEFAULT ''
);
CREATE TABLE job_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id),
 ts TEXT NOT NULL, code TEXT NOT NULL, message TEXT NOT NULL
);
CREATE INDEX job_events_job ON job_events(job_id,id);
CREATE TRIGGER immutable_job_options BEFORE UPDATE OF options_json,kind,volume_id ON jobs
 BEGIN SELECT RAISE(ABORT,'immutable job options'); END;
CREATE TABLE write_intents (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id), volume_id TEXT NOT NULL REFERENCES volumes(id),
 drive_id TEXT NOT NULL, expected_data_index INTEGER NOT NULL CHECK(expected_data_index>=0),
 expected_commit_index INTEGER NOT NULL CHECK(expected_commit_index>=0),
 previous_commit_sha256 TEXT, phase TEXT NOT NULL,
 write_may_have_started INTEGER NOT NULL DEFAULT 0 CHECK(write_may_have_started IN(0,1)),
 expected_data_sha256 TEXT, expected_data_bytes INTEGER CHECK(expected_data_bytes>=0),
 expected_commit_sha256 TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE archives (
 id TEXT PRIMARY KEY, volume_id TEXT NOT NULL REFERENCES volumes(id), job_id TEXT REFERENCES jobs(id),
 sequence INTEGER NOT NULL CHECK(sequence>0), data_file_index INTEGER NOT NULL CHECK(data_file_index>=0),
 commit_file_index INTEGER, source_label TEXT NOT NULL, created_at TEXT NOT NULL,
 stream_bytes INTEGER NOT NULL CHECK(stream_bytes>=0), sha256 TEXT NOT NULL,
 commit_sha256 TEXT, integrity_state TEXT NOT NULL CHECK(integrity_state IN('COMMITTED','VERIFIED','OBSERVED','FAILED','UNKNOWN')),
 integrity_basis TEXT NOT NULL, verified_at TEXT, restore_eligible INTEGER NOT NULL CHECK(restore_eligible IN(0,1)),
 UNIQUE(volume_id,data_file_index)
);
CREATE TABLE staged_members (
 attempt_id TEXT NOT NULL, path TEXT NOT NULL COLLATE BINARY, raw_path TEXT NOT NULL, parent_path TEXT NOT NULL,
 type TEXT NOT NULL, size INTEGER NOT NULL CHECK(size>=0), mtime TEXT NOT NULL, mode INTEGER NOT NULL,
 link_target TEXT NOT NULL, PRIMARY KEY(attempt_id,path)
);
CREATE TABLE members (
 id INTEGER PRIMARY KEY, archive_id TEXT NOT NULL REFERENCES archives(id),
 path TEXT NOT NULL COLLATE BINARY, raw_path TEXT NOT NULL, parent_path TEXT NOT NULL,
 type TEXT NOT NULL, size INTEGER NOT NULL CHECK(size>=0), mtime TEXT NOT NULL, mode INTEGER NOT NULL,
 link_target TEXT NOT NULL, UNIQUE(archive_id,path)
);
CREATE INDEX members_parent ON members(archive_id,parent_path,path);
CREATE INDEX members_path ON members(path,id);
CREATE TABLE verifications (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), archive_id TEXT REFERENCES archives(id),
 basis TEXT NOT NULL, expected_sha256 TEXT NOT NULL, actual_sha256 TEXT NOT NULL, actual_commit_sha256 TEXT,
 expected_bytes INTEGER NOT NULL CHECK(expected_bytes>=0), actual_bytes INTEGER NOT NULL CHECK(actual_bytes>=0),
 identity_valid INTEGER NOT NULL CHECK(identity_valid IN(0,1)), format_valid INTEGER NOT NULL CHECK(format_valid IN(0,1)),
 created_at TEXT NOT NULL,
 CHECK(expected_sha256=actual_sha256 AND expected_bytes=actual_bytes AND identity_valid=1 AND format_valid=1)
);
CREATE TABLE completion_evidence (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id), verified_spool INTEGER NOT NULL DEFAULT 0,
 published INTEGER NOT NULL DEFAULT 0
);
