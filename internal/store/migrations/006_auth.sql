CREATE TABLE admins (
 id INTEGER PRIMARY KEY CHECK(id=1), password_hash TEXT NOT NULL,
 version INTEGER NOT NULL CHECK(version>0), created_at TEXT NOT NULL, changed_at TEXT NOT NULL
);
CREATE TABLE sessions (
 token_hash TEXT PRIMARY KEY CHECK(length(token_hash)=64),
 admin_version INTEGER, created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL,
 idle_expires_at TEXT NOT NULL, absolute_expires_at TEXT NOT NULL
);
CREATE INDEX sessions_expiry ON sessions(absolute_expires_at,idle_expires_at);
CREATE TABLE auth_audit_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL,
 action TEXT NOT NULL, outcome TEXT NOT NULL, request_id TEXT NOT NULL
);
