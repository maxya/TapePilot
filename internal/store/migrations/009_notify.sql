CREATE TABLE smtp_preferences (
 id INTEGER PRIMARY KEY CHECK(id=1), enabled INTEGER NOT NULL CHECK(enabled IN(0,1))
);
INSERT INTO smtp_preferences VALUES(1,0);
CREATE TABLE notification_outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL REFERENCES jobs(id),
 code TEXT NOT NULL, created_at TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
 next_attempt_at TEXT NOT NULL, delivered_at TEXT, last_error_code TEXT NOT NULL DEFAULT '',
 permanent_failure INTEGER NOT NULL DEFAULT 0 CHECK(permanent_failure IN(0,1)),
 UNIQUE(job_id,code)
);
CREATE INDEX notification_outbox_ready ON notification_outbox(permanent_failure,delivered_at,next_attempt_at,id);
CREATE TRIGGER job_event_email AFTER INSERT ON job_events
 WHEN NEW.code IN('WAITING_MEDIA','SUCCEEDED','FAILED','RECOVERY_REQUIRED','RESTORE_PUBLISHED')
 AND (SELECT enabled FROM smtp_preferences WHERE id=1)=1
 BEGIN
 INSERT INTO notification_outbox(job_id,code,created_at,next_attempt_at)
 SELECT NEW.job_id,NEW.code,NEW.ts,NEW.ts
 WHERE (SELECT COUNT(*) FROM notification_outbox WHERE delivered_at IS NULL AND permanent_failure=0 AND attempts<5)<10000
 ON CONFLICT(job_id,code) DO NOTHING;
 INSERT INTO notifications(code,message,created_at,dedupe_key)
 SELECT 'SMTP_QUEUE_FULL','Email queue is full. Review notification delivery settings.',NEW.ts,'smtp-queue-full'
 WHERE NOT EXISTS(SELECT 1 FROM notification_outbox WHERE job_id=NEW.job_id AND code=NEW.code)
 ON CONFLICT(dedupe_key) DO NOTHING;
END;
