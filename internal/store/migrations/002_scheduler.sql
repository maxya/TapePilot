ALTER TABLE jobs ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN(0,1));
ALTER TABLE jobs ADD COLUMN media_recheck INTEGER NOT NULL DEFAULT 0 CHECK(media_recheck IN(0,1));
CREATE TABLE volume_enrollments (
 volume_id TEXT PRIMARY KEY REFERENCES volumes(id), drive_id TEXT NOT NULL,
 profile TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN(0,1)),
 enrolled_at TEXT NOT NULL, revoked_at TEXT, operator_ref TEXT NOT NULL
);
CREATE TABLE enrollment_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, volume_id TEXT NOT NULL REFERENCES volumes(id),
 drive_id TEXT NOT NULL, profile TEXT NOT NULL, action TEXT NOT NULL, operator_ref TEXT NOT NULL, created_at TEXT NOT NULL
);
