package store

import (
	"database/sql/driver"
	"strings"
	"time"

	"modernc.org/sqlite"
)

const catalogInstantLayout = "2006-01-02T15:04:05.000000000Z"

// catalogInstant preserves nanoseconds and gives every UTC instant the same
// width, making SQLite BINARY ordering match chronological ordering exactly.
func catalogInstant(value string) (string, error) {
	if !strings.HasSuffix(value, "Z") {
		return "", ErrEvidence
	}
	if fraction := strings.IndexAny(value, ".,"); fraction >= 0 && len(value)-fraction-2 > 9 {
		return "", ErrEvidence
	}
	instant, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", ErrEvidence
	}
	return instant.UTC().Format(catalogInstantLayout), nil
}

// Migration 005 uses a versioned deterministic function instead of SQLite's
// millisecond-limited strftime, preserving every nanosecond in existing rows.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("tapepilot_utc_instant_v1", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, ok := args[0].(string)
		if !ok {
			return nil, ErrEvidence
		}
		return catalogInstant(value)
	})
}
