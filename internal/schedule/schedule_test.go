package schedule

import (
	"github.com/maxya/TapePilot/internal/store"
	"testing"
	"time"
)

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	v, e := time.Parse(time.RFC3339, value)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestDSTAndLatestMissedSlot(t *testing.T) {
	tests := []struct {
		name, cadence, zone, clock, now, due, key string
		weekday                                   int
	}{
		{"spring gap", "daily", "America/Chicago", "02:30", "2026-03-08T12:00:00Z", "2026-03-08T08:00:00Z", "2026-03-08T02:30", 0},
		{"fall first occurrence", "daily", "America/Chicago", "01:30", "2026-11-01T12:00:00Z", "2026-11-01T06:30:00Z", "2026-11-01T01:30", 0},
		{"before repeated second occurrence", "daily", "America/Chicago", "01:30", "2026-11-01T07:15:00Z", "2026-11-01T06:30:00Z", "2026-11-01T01:30", 0},
		{"previous day before slot", "daily", "UTC", "23:00", "2026-11-24T12:00:00Z", "2026-11-23T23:00:00Z", "2026-11-23T23:00", 0},
		{"weekly latest", "weekly", "UTC", "12:00", "2026-11-24T12:00:00Z", "2026-11-22T12:00:00Z", "2026-11-22T12:00", 0},
		{"half hour DST gap", "daily", "Australia/Lord_Howe", "02:15", "2026-10-04T12:00:00Z", "2026-10-03T15:30:00Z", "2026-10-04T02:15", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := store.Schedule{ProfileID: "2f2d805c-e530-4f68-ab29-5cb4c8f63319", Cadence: test.cadence, Timezone: test.zone, LocalTime: test.clock, Weekday: test.weekday}
			slot, ok, e := Latest(s, instant(t, test.now))
			if e != nil || !ok || slot.Key != test.key || !slot.DueAt.Equal(instant(t, test.due)) {
				t.Fatalf("slot=%#v present=%v error=%v", slot, ok, e)
			}
		})
	}
}
func TestNewSchedulesDoNotBackfillBeforeCreation(t *testing.T) {
	s := store.Schedule{ProfileID: "2f2d805c-e530-4f68-ab29-5cb4c8f63319", Cadence: "daily", Timezone: "UTC", LocalTime: "12:00", CreatedAt: "2026-11-24T13:00:00Z"}
	if _, ok, e := Latest(s, instant(t, "2026-11-24T14:00:00Z")); e != nil || ok {
		t.Fatal("schedule ran before creation", ok, e)
	}
	s.Timezone = "Local"
	if _, _, e := Latest(s, time.Now()); e == nil {
		t.Fatal("host-dependent timezone accepted")
	}
}
