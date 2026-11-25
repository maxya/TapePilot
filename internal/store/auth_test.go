package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testCredential(t *testing.T, s *Store) AdminCredential {
	t.Helper()
	if err := s.SetAdminPassword(ctx, strings.Repeat("x", 64), false); err != nil {
		t.Fatal(err)
	}
	a, err := s.AdminCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func authSession(hash string, version *int64, now time.Time) Session {
	return Session{TokenHash: hash, AdminVersion: version, CreatedAt: now.Format(catalogInstantLayout), LastSeenAt: now.Format(catalogInstantLayout), IdleExpiresAt: now.Add(30 * time.Minute).Format(catalogInstantLayout), AbsoluteExpiresAt: now.Add(12 * time.Hour).Format(catalogInstantLayout), RequestID: "synthetic-request"}
}
func TestSessionExpiryTouchRotationAndPasswordVersion(t *testing.T) {
	s, _ := openTest(t)
	a := testCredential(t, s)
	now := time.Now().UTC()
	hash := strings.Repeat("a", 64)
	if err := s.CreateSession(ctx, authSession(hash, &a.Version, now), ""); err != nil {
		t.Fatal(err)
	}
	touched, err := s.Session(ctx, hash, now.Add(2*time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	expected := now.Add(32 * time.Minute).Format(catalogInstantLayout)
	if touched.IdleExpiresAt != expected {
		t.Fatal(touched.IdleExpiresAt, expected)
	}
	if _, err = s.Session(ctx, hash, now.Add(33*time.Minute), false); !errors.Is(err, ErrNotFound) {
		t.Fatal("idle expiry bypass", err)
	}
	if _, err = s.Session(ctx, hash, now.Add(13*time.Hour), false); !errors.Is(err, ErrNotFound) {
		t.Fatal("absolute expiry bypass", err)
	}
	next := strings.Repeat("b", 64)
	if err = s.CreateSession(ctx, authSession(next, &a.Version, now), hash); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, hash, now, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("old session retained after rotation", err)
	}
	if err = s.SetAdminPassword(ctx, strings.Repeat("y", 64), true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, next, now, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("password reset did not revoke session", err)
	}
	if err = s.CreateSession(ctx, authSession(strings.Repeat("c", 64), &a.Version, now), ""); !errors.Is(err, ErrConflict) {
		t.Fatal("stale credential login after reset", err)
	}
}
func TestSessionCapacityAndHashedStorage(t *testing.T) {
	s, _ := openTest(t)
	a := testCredential(t, s)
	now := time.Now().UTC()
	for i := 0; i < 16; i++ {
		hash := strings.Repeat("a", 62) + string("0123456789abcdef"[i]) + "0"
		if err := s.CreateSession(ctx, authSession(hash, &a.Version, now), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, authSession(strings.Repeat("b", 64), &a.Version, now), ""); !errors.Is(err, ErrSessionLimit) {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		hash := strings.Repeat("c", 60) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16]) + "00"
		if err := s.CreateSession(ctx, authSession(hash, nil, now), ""); err != nil {
			t.Fatal(i, err)
		}
	}
	if err := s.CreateSession(ctx, authSession(strings.Repeat("d", 64), nil, now), ""); !errors.Is(err, ErrSessionLimit) {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE length(token_hash)<>64").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := s.CreateSession(ctx, authSession("raw-cookie-token", nil, now), ""); err == nil {
		t.Fatal("raw token stored")
	}
}
func TestAuthSuccessAuditCommitsWithRotation(t *testing.T) {
	s, _ := openTest(t)
	a := testCredential(t, s)
	now := time.Now().UTC()
	hash := strings.Repeat("a", 64)
	if err := s.CreateSession(ctx, authSession(hash, &a.Version, now), ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM auth_audit_events WHERE action='LOGIN' AND outcome='SUCCEEDED' AND request_id='synthetic-request'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.LogoutSession(ctx, hash, "logout-request"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, hash, now, false); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM auth_audit_events WHERE action='LOGOUT' AND outcome='SUCCEEDED'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
