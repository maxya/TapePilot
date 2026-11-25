CREATE TABLE http_plans (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('RESTORE','BACKUP')),
 profile_id TEXT NOT NULL, revision INTEGER NOT NULL, volume_id TEXT NOT NULL,
 options_json TEXT NOT NULL CHECK(json_valid(options_json)),
 preview_json TEXT NOT NULL CHECK(json_valid(preview_json)),
 created_at TEXT NOT NULL, expires_at TEXT NOT NULL
);
CREATE INDEX http_plans_expiry ON http_plans(expires_at);
