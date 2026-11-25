-- Canonicalize existing UTC timestamps without reducing their nanosecond precision.
-- tapepilot_utc_instant_v1 is a versioned deterministic Go migration function.
UPDATE archives SET created_at=tapepilot_utc_instant_v1(created_at);
