CREATE TABLE job_progress (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id),
 phase TEXT NOT NULL CHECK(phase IN ('planning','identity_check','positioning','streaming','closing_data','writing_commit','readback','indexing','spooling','extracting','publishing')),
 accepted_bytes INTEGER NOT NULL CHECK(accepted_bytes>=0),
 readback_bytes INTEGER NOT NULL CHECK(readback_bytes>=0),
 phase_total_bytes INTEGER CHECK(phase_total_bytes>0),
 sampled_at TEXT NOT NULL,
 started_at TEXT NOT NULL,
 elapsed_seconds REAL NOT NULL CHECK(elapsed_seconds>=0),
 speed_mib_per_second REAL NOT NULL CHECK(speed_mib_per_second>=0),
 estimated_percent REAL CHECK(estimated_percent>=0 AND estimated_percent<=100),
 eta_seconds REAL CHECK(eta_seconds>=0)
);
