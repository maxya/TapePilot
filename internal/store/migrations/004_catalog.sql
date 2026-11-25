CREATE TABLE volume_metadata (
 volume_id TEXT PRIMARY KEY REFERENCES volumes(id), metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json))
);
CREATE TABLE catalog_attempts (
 attempt_id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id)
);
CREATE TABLE restore_publications (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id), archive_id TEXT NOT NULL REFERENCES archives(id),
 spool_sha256 TEXT NOT NULL, spool_bytes INTEGER NOT NULL CHECK(spool_bytes>0),
 root_id TEXT, destination_slug TEXT, intent_at TEXT, completed_at TEXT
);
CREATE TABLE exports (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN('database','descriptor','recovery-text','catalog')),
 volume_id TEXT REFERENCES volumes(id), filename TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL, expires_at TEXT NOT NULL, bytes INTEGER NOT NULL CHECK(bytes>=0), sha256 TEXT NOT NULL
);
CREATE INDEX archives_cursor ON archives(created_at,id);
CREATE INDEX archives_volume_sequence ON archives(volume_id,sequence);
