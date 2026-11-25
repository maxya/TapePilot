CREATE TABLE backup_profiles (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, source_id TEXT NOT NULL,
 relative_selection TEXT NOT NULL, exclusions_json TEXT NOT NULL CHECK(json_valid(exclusions_json)),
 target_volume_id TEXT NOT NULL REFERENCES volumes(id), enabled INTEGER NOT NULL CHECK(enabled IN(0,1)),
 revision INTEGER NOT NULL CHECK(revision>0), created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE job_profiles (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id), profile_id TEXT NOT NULL REFERENCES backup_profiles(id),
 profile_revision INTEGER NOT NULL CHECK(profile_revision>0)
);
CREATE INDEX job_profiles_profile ON job_profiles(profile_id,job_id);
CREATE TABLE schedules (
 id TEXT PRIMARY KEY, profile_id TEXT NOT NULL REFERENCES backup_profiles(id),
 cadence TEXT NOT NULL CHECK(cadence IN('daily','weekly')), timezone TEXT NOT NULL,
 local_time TEXT NOT NULL, weekday INTEGER NOT NULL CHECK(weekday BETWEEN 0 AND 6),
 enabled INTEGER NOT NULL CHECK(enabled IN(0,1)), revision INTEGER NOT NULL CHECK(revision>0),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_slot TEXT NOT NULL DEFAULT ''
);
CREATE TABLE audit_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, action TEXT NOT NULL,
 object TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TRIGGER audit_append_only_update BEFORE UPDATE ON audit_events BEGIN SELECT RAISE(ABORT,'append-only audit'); END;
CREATE TRIGGER audit_append_only_delete BEFORE DELETE ON audit_events BEGIN SELECT RAISE(ABORT,'append-only audit'); END;
CREATE TABLE notifications (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT REFERENCES jobs(id),
 code TEXT NOT NULL, message TEXT NOT NULL, created_at TEXT NOT NULL, read_at TEXT,
 dedupe_key TEXT NOT NULL UNIQUE
);
CREATE TRIGGER job_event_audit AFTER INSERT ON job_events BEGIN
 INSERT INTO audit_events(actor,action,object,created_at) VALUES('system','JOB_'||NEW.code,NEW.job_id,NEW.ts);
END;
CREATE TRIGGER job_event_notification AFTER INSERT ON job_events
 WHEN NEW.code IN('WAITING_MEDIA','SUCCEEDED','FAILED','RECOVERY_REQUIRED','CANCELLED','RESTORE_PUBLISHED')
 BEGIN
 INSERT INTO notifications(job_id,code,message,created_at,dedupe_key)
 VALUES(NEW.job_id,NEW.code,NEW.message,NEW.ts,'job:'||NEW.job_id||':'||NEW.code)
 ON CONFLICT(dedupe_key) DO NOTHING;
END;
CREATE TRIGGER auth_event_audit AFTER INSERT ON auth_audit_events BEGIN
 INSERT INTO audit_events(actor,action,object,created_at)
 VALUES('admin','AUTH_'||NEW.action||'_'||NEW.outcome,'',NEW.ts);
END;
