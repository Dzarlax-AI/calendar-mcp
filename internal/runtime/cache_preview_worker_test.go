package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type forbiddenPreviewBuilder struct{ calls int }

func (p *forbiddenPreviewBuilder) Build(context.Context) ([]calendar.Provider, error) {
	p.calls++
	return nil, errors.New("provider must not be accessed")
}

func TestCachedPreviewWorkerUsesOnlyVerifiedLocalSnapshot(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cache-preview.db")
	s, err := storage.Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s.SetArtifactCipher(cipher)
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, storage.Connection{ID: "account", Provider: "google", AccountFingerprint: "account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "target"} {
		if err = s.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	rule := storage.Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err = s.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	// Seed a synthetic verified fixture; production publication remains gated.
	start, end := now.Add(-time.Hour), now.AddDate(0, 0, 15)
	if err = s.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{ID: "event", CalendarID: "source", Provider: "google", Title: "Synthetic", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
	payload, err := json.Marshal(map[string]any{"version": 1, "events": []calendar.EventV2{event}, "attendee_evidence": []map[string]any{{"event_id": "event", "complete": false}}})
	if err != nil {
		t.Fatal(err)
	}
	base := int64(0)
	input := storage.ProjectionSnapshotInput{CalendarID: "source", StageID: "test-stage-012345678901", Timezone: "UTC", BaseRevision: &base, WindowStart: start, WindowEnd: end, CompletedAt: now, Cursor: "synthetic", Payload: payload, Complete: true}
	if err = s.StageProjectionSnapshot(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishProjectionSnapshot(ctx, "source", input.StageID); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetProjectionSnapshot(ctx, "source", 1)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?`, "source", 1); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO projection_snapshots(calendar_id,revision,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "source", 1, snap.StageID, snap.GuardEpoch, snap.BaseRevision, snap.WindowStart, snap.WindowEnd, snap.CompletedAt, snap.Timezone, snap.Cursor, snap.Payload, true); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO projection_revision_provenance(calendar_id,revision,observations) VALUES(?,?,?)`, "source", 1, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE calendars SET timezone='UTC' WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE calendar_sync_state SET status='ready',cursor=?,last_success_at=? WHERE calendar_id='source'`, "synthetic", now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.EnqueueJob(ctx, storage.Job{ID: "job", RuleID: "rule", Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	factory := &forbiddenPreviewBuilder{}
	if err = executeJob(ctx, s, factory, *job, now); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != "succeeded" {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	preview, status, err := s.GetSyncRunPreview(ctx, runs[0].ID, time.Now().UTC())
	if err != nil || status != "available" || preview.TargetStatus != "not_checked" || preview.SourceRevision != 1 || preview.Total != 1 || preview.Entries[0].Action != "candidate" || factory.calls != 0 {
		t.Fatalf("preview: %+v %s %v calls=%d", preview, status, err, factory.calls)
	}
	if err = s.SetRuleState(ctx, "rule", "enabled", nil, now); err == nil {
		t.Fatal("cached rule enabled")
	}
	if err = s.EnqueueJob(ctx, storage.Job{ID: "manual", RuleID: "rule", Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); !errors.Is(err, storage.ErrCalendarSyncIneligible) {
		t.Fatalf("manual write on paused rule must be rejected: %v", err)
	}
	if err = s.EnableCachedRuleIfReady(ctx, "rule", now.Add(16*time.Minute), 15*time.Minute); !errors.Is(err, storage.ErrProjectionSnapshotUnavailable) {
		t.Fatalf("stale source enabled: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='primary' WHERE id='target'`); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableCachedRuleIfReady(ctx, "rule", time.Now().UTC(), 15*time.Minute); !errors.Is(err, storage.ErrCacheExecutionIneligible) {
		t.Fatalf("ambiguous target enabled: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE calendars SET provider_calendar_id='target' WHERE id='target'`); err != nil {
		t.Fatal(err)
	}
	// A later rule revision cannot reuse the prior preview.
	if _, err = db.ExecContext(ctx, `UPDATE sync_rules SET config_version=config_version+1 WHERE id='rule'`); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableCachedRuleIfReady(ctx, "rule", time.Now().UTC(), 15*time.Minute); !errors.Is(err, storage.ErrCacheExecutionIneligible) {
		t.Fatalf("stale preview configuration enabled: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE sync_rules SET config_version=config_version-1 WHERE id='rule'`); err != nil {
		t.Fatal(err)
	}
	// A newer verified revision also requires a new preview.
	if _, err = db.ExecContext(ctx, `INSERT INTO projection_snapshots(calendar_id,revision,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified) SELECT calendar_id,2,'second-stage',guard_epoch,1,window_start,window_end,completed_at,timezone,cursor,payload,verified FROM projection_snapshots WHERE calendar_id='source' AND revision=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO projection_revision_provenance(calendar_id,revision,observations) VALUES('source',2,?)`, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE projection_mutation_guards SET revision=2 WHERE calendar_id='source'`); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableCachedRuleIfReady(ctx, "rule", time.Now().UTC(), 15*time.Minute); !errors.Is(err, storage.ErrCacheExecutionIneligible) {
		t.Fatalf("stale preview revision enabled: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE projection_mutation_guards SET revision=1 WHERE calendar_id='source'`); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableCachedRuleIfReady(ctx, "rule", time.Now().UTC(), 15*time.Minute); err != nil {
		t.Fatalf("ready enable: %v", err)
	}
	enabled, err := s.RuleByID(ctx, "rule")
	if err != nil || enabled.State != "enabled" || enabled.SyncMode != "cached_instances" {
		t.Fatalf("enabled rule: %+v %v", enabled, err)
	}
}
