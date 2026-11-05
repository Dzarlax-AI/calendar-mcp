package storage

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/credentials"
)

func TestSyncPreviewEncryptedAtomicAndExpires(t *testing.T) {
	s := newSQLiteStore(t)
	runSyncPreviewStorageContract(t, s)
}

func runSyncPreviewStorageContract(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	s.SetArtifactCipher(cipher)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateConnection(ctx, Connection{ID: "account", Provider: "google", AccountFingerprint: "account", DisplayName: "test", Status: "connected", EncryptedCredentials: []byte("fake"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err := s.UpsertCalendar(ctx, Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRule(ctx, Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob(ctx, Job{ID: "job", RuleID: "rule", Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.StartRun(ctx, Run{ID: "run", JobID: "job", RuleID: "rule", Trigger: "dry_run", Outcome: "running", StartedAt: now, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	preview := &SyncRunPreview{Total: 1, Entries: []SyncRunPreviewEntry{{Action: "delete", Title: "private title", TargetStatus: "not_checked"}}}
	stale := *job
	stale.Attempt++
	if err := s.FinishRun(ctx, "run", stale, "succeeded", now, Run{Preview: preview}); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("lease: %v", err)
	}
	if _, state, err := s.GetSyncRunPreview(ctx, "run", now); err != nil || state != "unavailable" {
		t.Fatalf("stale preview: %s %v", state, err)
	}
	if err := s.FinishRun(ctx, "run", *job, "succeeded", now, Run{Preview: preview}); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM sync_run_previews WHERE run_id='run'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("private title")) {
		t.Fatal("private event title stored in plaintext")
	}
	got, state, err := s.GetSyncRunPreview(ctx, "run", now)
	if err != nil || state != "available" || got.Entries[0].Title != "private title" {
		t.Fatalf("read %s %v", state, err)
	}
	if err := s.ExpireSyncRunPreviews(ctx, now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, state, err := s.GetSyncRunPreview(ctx, "run", now.Add(7*24*time.Hour)); err != nil || state != "expired" {
		t.Fatalf("expiry %s %v", state, err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_run_previews WHERE payload IS NOT NULL`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("payloads %d %v", remaining, err)
	}
}
