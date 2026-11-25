// Package schedule resolves persisted daily/weekly local slots deterministically.
package schedule

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/store"
)

type Slot struct {
	Key   string
	DueAt time.Time
}

// Latest returns only the latest eligible missed slot. Nonexistent wall times
// run at the first later minute on that date; repeated times choose their first
// occurrence. A completely skipped local calendar date has no occurrence.
func Latest(s store.Schedule, now time.Time) (Slot, bool, error) {
	if e := store.ValidSchedule(s); e != nil {
		return Slot{}, false, e
	}
	location, e := time.LoadLocation(s.Timezone)
	if e != nil {
		return Slot{}, false, e
	}
	hour, _ := strconv.Atoi(s.LocalTime[:2])
	minute, _ := strconv.Atoi(s.LocalTime[3:])
	local := now.In(location)
	year, month, day := local.Date()
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	var created time.Time
	if s.CreatedAt != "" {
		created, e = time.Parse(time.RFC3339Nano, s.CreatedAt)
		if e != nil {
			return Slot{}, false, e
		}
	}
	for ago := 0; ago <= 14; ago++ {
		candidateDate := date.AddDate(0, 0, -ago)
		if s.Cadence == "weekly" && int(candidateDate.Weekday()) != s.Weekday {
			continue
		}
		due, ok := occurrence(candidateDate, hour*60+minute, location)
		if !ok || due.After(now) {
			continue
		}
		if !created.IsZero() && due.Before(created) {
			return Slot{}, false, nil
		}
		return Slot{candidateDate.Format("2006-01-02") + "T" + s.LocalTime, due}, true, nil
	}
	return Slot{}, false, nil
}
func occurrence(date time.Time, targetMinute int, location *time.Location) (time.Time, bool) {
	year, month, day := date.Date()
	begin := date.Add(-24 * time.Hour)
	end := date.Add(48 * time.Hour)
	var best time.Time
	bestMinute := 24 * 60
	for instant := begin; instant.Before(end); instant = instant.Add(time.Minute) {
		local := instant.In(location)
		y, m, d := local.Date()
		if y != year || m != month || d != day || local.Second() != 0 {
			continue
		}
		minute := local.Hour()*60 + local.Minute()
		if minute >= targetMinute && minute < bestMinute {
			best = instant
			bestMinute = minute
		}
	}
	return best, !best.IsZero()
}

// Tick queues at most one job per profile and wakes the existing single-worker
// scheduler. The store transaction handles concurrent ticks and slot dedupe.
func Tick(ctx context.Context, r *app.Runtime, now time.Time, wake func()) error {
	if r == nil || r.Store == nil || now.IsZero() {
		return store.ErrOperations
	}
	schedules, e := r.Store.ListSchedules(ctx)
	if e != nil {
		return e
	}
	for _, s := range schedules {
		if e = ctx.Err(); e != nil {
			return e
		}
		if !s.Enabled {
			continue
		}
		slot, ok, e := Latest(s, now)
		if e != nil {
			return e
		}
		if !ok || slot.Key <= s.LastSlot {
			continue
		}
		profile, e := r.Store.BackupProfile(ctx, s.ProfileID)
		if e != nil {
			return e
		}
		_, sourceAllowed := r.Settings.Sources[profile.SourceID]
		_, created, e := r.Store.FireSchedule(ctx, s.ID, s.Revision, slot.Key, slot.DueAt, now, sourceAllowed)
		if errors.Is(e, store.ErrConflict) {
			continue
		}
		if e != nil {
			return e
		}
		if created && wake != nil {
			wake()
		}
	}
	return nil
}
func Run(ctx context.Context, r *app.Runtime, wake func()) error {
	if e := Tick(ctx, r, time.Now().UTC(), wake); e != nil {
		return e
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if e := Tick(ctx, r, now.UTC(), wake); e != nil {
				return e
			}
		}
	}
}
